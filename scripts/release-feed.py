#!/usr/bin/env python3
# 中文说明：统一正式版与预览版的更新来源，核验六平台发布资产并生成每个平台唯一的更新 feed，不访问网络或读取私钥。
import argparse
import datetime
import hashlib
import json
from pathlib import Path
import re
import shutil
import zipfile


FEED_NAME = "codex-chronicle-feed.json"
VERSION = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-preview\.(?:0|[1-9][0-9]*))?"


def release_config(version, repository):
    if not re.fullmatch(VERSION, version):
        raise ValueError("版本必须采用 1.2.3 或 1.2.3-preview.1 形式，不带 v 前缀")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("仓库必须采用 owner/repository 形式")
    preview = "-preview." in version
    feed_url = (f"https://raw.githubusercontent.com/{repository}/update-feeds/preview.json" if preview else
                f"https://github.com/{repository}/releases/latest/download/{FEED_NAME}")
    return {"version": version, "channel": "preview" if preview else "stable", "feed_url": feed_url}


def expected_packages(version):
    result = {}
    for operating_system in ("darwin", "windows", "linux"):
        for arch in ("amd64", "arm64"):
            prefix = f"Codex-Chronicle-{version}"
            names = [f"{prefix}-{operating_system}-{arch}.zip"]
            if operating_system == "darwin":
                names.append(f"{prefix}-macOS-{arch}.dmg")
            elif operating_system == "windows":
                names.append(f"{prefix}-{arch}-Setup.exe")
            else:
                names.extend(f"{prefix}-linux-{arch}.{suffix}" for suffix in ("tar.gz", "deb", "rpm"))
            result[(operating_system, arch)] = names
    return result


def file_digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def assemble_release(artifacts, output, version, repository):
    config = release_config(version, repository)
    packages = expected_packages(version)
    expected = {name for names in packages.values() for name in names}
    verified = {}
    if output.exists():
        raise ValueError(f"输出目录已存在，拒绝覆盖：{output}")
    for sums in sorted(artifacts.glob("*/SHA256SUMS")):
        for line in sums.read_text(encoding="utf-8").splitlines():
            match = re.fullmatch(r"([0-9a-fA-F]{64})  ([^/\\]+)", line)
            if not match or match[2] not in expected:
                raise ValueError(f"摘要清单含无效或非预期文件名：{line}")
            digest, name = match.groups()
            source = sums.parent / name
            if name in verified:
                raise ValueError(f"发布资产重复：{name}")
            if source.is_symlink() or not source.is_file() or file_digest(source) != digest.lower():
                raise ValueError(f"发布资产缺失或 SHA-256 不匹配：{name}")
            verified[name] = (source, digest.lower())
    if set(verified) != expected:
        raise ValueError("缺少发布资产：" + "、".join(sorted(expected - set(verified))))

    releases = []
    for (operating_system, arch), names in packages.items():
        # 所有平台统一选 ZIP；TAR.GZ 和各平台安装器继续分发，避免同平台同版本出现重复候选。
        name = names[0]
        source, digest = verified[name]
        try:
            with zipfile.ZipFile(source) as archive:
                info = archive.getinfo("codex-chronicle-release.json")
                if info.file_size > 4 << 20:
                    raise ValueError(f"发布清单过大：{name}")
                manifest = json.loads(archive.read(info))
        except (OSError, KeyError, zipfile.BadZipFile, ValueError) as error:
            raise ValueError(f"无法读取发布清单：{name}：{error}") from error
        if not isinstance(manifest, dict) or (manifest.get("version"), manifest.get("os"), manifest.get("arch")) != (version, operating_system, arch):
            raise ValueError(f"发布清单版本或平台与资产名称不匹配：{name}")
        release = {
            "version": version, "os": operating_system, "arch": arch,
            "url": f"https://github.com/{repository}/releases/download/v{version}/{name}",
            "sha256": digest, "size_bytes": source.stat().st_size,
        }
        if manifest.get("build_id"):
            release["build_id"] = manifest["build_id"]
        releases.append(release)

    feed = {
        "format_version": 1, "product": "codex-chronicle", "channel": config["channel"],
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z"),
        "releases": releases,
    }
    # 所有输入验证成功后才创建输出，错误的候选不会留下可以误上传的半份发布目录。
    output.mkdir(parents=True)
    rows = []
    for name, (source, digest) in sorted(verified.items()):
        shutil.copy2(source, output / name)
        rows.append(digest + "  " + name)
    feed_path = output / FEED_NAME
    feed_path.write_text(json.dumps(feed, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    rows.append(file_digest(feed_path) + "  " + FEED_NAME)
    (output / "SHA256SUMS").write_text("\n".join(sorted(rows)) + "\n", encoding="utf-8")
    return feed


def verify_uploaded_assets(output, release, version):
    if release.get("tag_name") != "v" + version or release.get("draft") is not True:
        raise ValueError("上传核验只接受当前标签的未公开草稿")
    assets = release.get("assets", [])
    expected = {path.name: path for path in output.iterdir() if path.is_file()}
    if len(assets) != len(expected) or {asset.get("name") for asset in assets} != set(expected):
        raise ValueError("GitHub 草稿资产集合不完整或存在重复，拒绝公开发布")
    for asset in assets:
        path = expected[asset["name"]]
        if asset.get("state") != "uploaded" or asset.get("size") != path.stat().st_size:
            raise ValueError(f"GitHub 草稿资产未上传完成或大小不匹配：{path.name}")
        digest = asset.get("digest")
        if digest and digest != "sha256:" + file_digest(path):
            raise ValueError(f"GitHub 草稿资产摘要不匹配：{path.name}")


def validate_preview_feed(feed, version, repository):
    if not isinstance(version, str) or release_config(version, repository)["channel"] != "preview":
        raise ValueError("预览通道只能接收预览版本")
    if not isinstance(feed, dict) or (feed.get("format_version"), feed.get("product"), feed.get("channel")) != (1, "codex-chronicle", "preview"):
        raise ValueError("预览更新索引格式或产品不匹配")
    releases = feed.get("releases")
    expected = expected_packages(version)
    if not isinstance(releases, list) or len(releases) != len(expected):
        raise ValueError("预览更新索引必须完整覆盖六个平台")
    seen = set()
    for item in releases:
        if not isinstance(item, dict):
            raise ValueError("预览更新条目无效")
        platform = (item.get("os"), item.get("arch"))
        if not all(isinstance(value, str) for value in platform):
            raise ValueError("预览更新条目缺少有效平台")
        names = expected.get(platform)
        if names is None or platform in seen or item.get("version") != version:
            raise ValueError("预览更新条目的版本或平台重复、不匹配")
        if item.get("url") != f"https://github.com/{repository}/releases/download/v{version}/{names[0]}":
            raise ValueError("预览更新下载地址必须指向本版本的唯一 ZIP")
        if not isinstance(item.get("sha256"), str) or not re.fullmatch(r"[0-9a-f]{64}", item["sha256"]) or type(item.get("size_bytes")) is not int or item["size_bytes"] <= 0:
            raise ValueError("预览更新条目的摘要或大小无效")
        seen.add(platform)


def promote_preview_feed(source, output, version, repository):
    data = source.read_bytes()
    feed = json.loads(data)
    validate_preview_feed(feed, version, repository)
    if output.is_symlink():
        raise ValueError("预览通道文件不能是符号链接")
    if output.exists():
        previous = json.loads(output.read_text(encoding="utf-8"))
        items = previous.get("releases") if isinstance(previous, dict) else None
        if not isinstance(items, list) or not items or not isinstance(items[0], dict):
            raise ValueError("现有预览通道无效，拒绝覆盖")
        old_version = items[0].get("version")
        validate_preview_feed(previous, old_version, repository)

        def preview_order(value):
            core, number = value.split("-preview.")
            return tuple(int(part) for part in core.split(".")) + (int(number),)

        if preview_order(old_version) > preview_order(version):
            return False
        if old_version == version:
            order = lambda item: (item["os"], item["arch"])
            if sorted(previous["releases"], key=order) != sorted(feed["releases"], key=order):
                raise ValueError("同一版本的预览更新内容已变化，拒绝覆盖")
            return False
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_bytes(data)
    return True


def main():
    parser = argparse.ArgumentParser(description="校验发布资产并生成固定渠道配置与更新索引")
    parser.add_argument("command", choices=("config", "feed-url", "assemble", "verify-upload", "promote-preview"))
    parser.add_argument("version")
    parser.add_argument("--repository", default="Red-noblue/Codex-Chronicle")
    parser.add_argument("--artifacts", type=Path, default=Path("release-assets"))
    parser.add_argument("--output", type=Path, default=Path("publish"))
    parser.add_argument("--release-json", type=Path)
    parser.add_argument("--feed-json", type=Path)
    args = parser.parse_args()
    try:
        config = release_config(args.version, args.repository)
        if args.command == "config":
            for name, value in config.items():
                print(f"{name}={value}")
        elif args.command == "feed-url":
            print(config["feed_url"])
        elif args.command == "verify-upload":
            if args.release_json is None:
                raise ValueError("上传核验必须指定 --release-json")
            verify_uploaded_assets(args.output, json.loads(args.release_json.read_text(encoding="utf-8")), args.version)
            print("GitHub 草稿资产核验通过")
        elif args.command == "promote-preview":
            if args.feed_json is None:
                raise ValueError("更新预览通道必须指定 --feed-json")
            changed = promote_preview_feed(args.feed_json, args.output, args.version, args.repository)
            print("预览通道已更新" if changed else "预览通道已是相同或更新版本，无需覆盖")
        else:
            feed = assemble_release(args.artifacts, args.output, args.version, args.repository)
            print(f"发布资产验证通过：{args.version} / {config['channel']} / {len(feed['releases'])} 个平台更新包")
    except (ValueError, OSError) as error:
        parser.exit(1, f"发布校验失败：{error}\n")


if __name__ == "__main__":
    main()
