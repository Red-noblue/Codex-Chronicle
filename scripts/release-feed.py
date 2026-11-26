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
    channel = "preview" if preview else "stable"
    feed_url = f"https://raw.githubusercontent.com/{repository}/update-feeds/{channel}.json"
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


def validate_channel_feed(feed, version, repository):
    channel = release_config(version, repository)["channel"]
    if not isinstance(feed, dict) or (feed.get("format_version"), feed.get("product"), feed.get("channel")) != (1, "codex-chronicle", channel):
        raise ValueError("更新索引格式、产品或通道不匹配")
    visible, held = feed.get("releases"), feed.get("held_releases", [])
    if not isinstance(visible, list) or not isinstance(held, list) or visible and held:
        raise ValueError("公开和暂缓发布包不能混用")
    releases = visible or held
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
    if type(feed.get("updates_paused", False)) is not bool or not isinstance(feed.get("pause_reason", ""), str) or len(feed.get("pause_reason", "")) > 500:
        raise ValueError("暂停更新策略无效")
    withdrawn = feed.get("withdrawn_versions", [])
    if not isinstance(withdrawn, list) or len(withdrawn) > 1024:
        raise ValueError("撤回版本列表无效")
    seen_versions = set()
    for item in withdrawn:
        if not isinstance(item, dict) or not isinstance(item.get("version"), str) or release_config(item["version"], repository)["channel"] != channel or item["version"] in seen_versions or not isinstance(item.get("reason", ""), str) or len(item.get("reason", "")) > 500:
            raise ValueError("撤回版本条目无效或重复")
        seen_versions.add(item["version"])
    if held and not feed.get("updates_paused") and version not in seen_versions:
        raise ValueError("暂缓发布包缺少对应暂停或撤回策略")


def validate_preview_feed(feed, version, repository):
    if not isinstance(version, str) or release_config(version, repository)["channel"] != "preview":
        raise ValueError("预览通道只能接收预览版本")
    validate_channel_feed(feed, version, repository)


def promote_channel_feed(source, output, version, repository):
    data = source.read_bytes()
    feed = json.loads(data)
    validate_channel_feed(feed, version, repository)
    if output.is_symlink():
        raise ValueError("预览通道文件不能是符号链接")
    if output.exists():
        previous = json.loads(output.read_text(encoding="utf-8"))
        items = (previous.get("releases") or previous.get("held_releases")) if isinstance(previous, dict) else None
        if not isinstance(items, list) or not items or not isinstance(items[0], dict):
            raise ValueError("现有预览通道无效，拒绝覆盖")
        old_version = items[0].get("version")
        validate_channel_feed(previous, old_version, repository)
        if previous["channel"] != feed["channel"]:
            raise ValueError("拒绝混用正式和预览更新通道")

        def preview_order(value):
            core, separator, number = value.partition("-preview.")
            return tuple(int(part) for part in core.split(".")) + ((int(number) if separator else 0),)

        if preview_order(old_version) > preview_order(version):
            return False
        # 发布新版本不擅自解除人工暂停，也不丢弃历史撤回记录。
        for field in ("updates_paused", "pause_reason", "withdrawn_versions"):
            if field in previous:
                feed[field] = previous[field]
        if old_version == version:
            order = lambda item: (item["os"], item["arch"])
            previous_items = previous.get("releases") or previous.get("held_releases", [])
            source_items = feed.get("releases") or feed.get("held_releases", [])
            if sorted(previous_items, key=order) != sorted(source_items, key=order):
                raise ValueError("同一版本的预览更新内容已变化，拒绝覆盖")
            return False
    apply_channel_visibility(feed)
    data = (json.dumps(feed, ensure_ascii=False, indent=2) + "\n").encode()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_bytes(data)
    return True


def promote_preview_feed(source, output, version, repository):
    if release_config(version, repository)["channel"] != "preview":
        raise ValueError("预览通道只能接收预览版本")
    return promote_channel_feed(source, output, version, repository)


def apply_channel_visibility(feed):
    # 旧客户端不认识撤回字段；隐藏下载条目使其检查失败，避免继续下载已暂停或撤回的版本。
    candidates = feed.get("releases") or feed.get("held_releases", [])
    withdrawn = {item["version"] for item in feed.get("withdrawn_versions", [])}
    if feed.get("updates_paused") or any(item["version"] in withdrawn for item in candidates):
        feed["releases"], feed["held_releases"] = [], candidates
    else:
        feed["releases"] = candidates
        feed.pop("held_releases", None)


def set_channel_policy(path, version, repository, action, target_version="", reason=""):
    if path.is_symlink():
        raise ValueError("更新通道不能是符号链接")
    original = path.read_bytes()
    feed = json.loads(original)
    validate_channel_feed(feed, version, repository)
    reason = reason.strip()
    if len(reason) > 500 or action in ("pause", "withdraw") and not reason:
        raise ValueError("暂停或撤回必须提供不超过 500 字的原因")
    if action == "withdraw":
        if release_config(target_version, repository)["channel"] != feed["channel"]:
            raise ValueError("只能撤回当前通道中的版本")
        withdrawn = {item["version"]: item for item in feed.get("withdrawn_versions", [])}
        withdrawn[target_version] = {"version": target_version, "reason": reason}
        feed["withdrawn_versions"] = [withdrawn[key] for key in sorted(withdrawn)]
    elif action in ("pause", "resume"):
        if target_version:
            raise ValueError("暂停或恢复通道不应指定目标版本")
        feed["updates_paused"] = action == "pause"
        feed["pause_reason"] = reason if action == "pause" else ""
    else:
        raise ValueError("不支持的更新策略操作")
    apply_channel_visibility(feed)
    validate_channel_feed(feed, version, repository)
    if feed == json.loads(original):
        return False
    feed["generated_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
    path.write_text(json.dumps(feed, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return True


def main():
    parser = argparse.ArgumentParser(description="校验发布资产并生成固定渠道配置与更新索引")
    parser.add_argument("command", choices=("config", "feed-url", "assemble", "verify-upload", "promote-preview", "promote-channel", "set-policy"))
    parser.add_argument("version")
    parser.add_argument("--repository", default="Red-noblue/Codex-Chronicle")
    parser.add_argument("--artifacts", type=Path, default=Path("release-assets"))
    parser.add_argument("--output", type=Path, default=Path("publish"))
    parser.add_argument("--release-json", type=Path)
    parser.add_argument("--feed-json", type=Path)
    parser.add_argument("--action", choices=("pause", "resume", "withdraw"))
    parser.add_argument("--target-version", default="")
    parser.add_argument("--reason", default="")
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
        elif args.command in ("promote-preview", "promote-channel"):
            if args.feed_json is None:
                raise ValueError("更新预览通道必须指定 --feed-json")
            promote = promote_preview_feed if args.command == "promote-preview" else promote_channel_feed
            changed = promote(args.feed_json, args.output, args.version, args.repository)
            print("预览通道已更新" if changed else "预览通道已是相同或更新版本，无需覆盖")
        elif args.command == "set-policy":
            if args.feed_json is None or args.action is None:
                raise ValueError("调整更新策略必须指定 --feed-json 和 --action")
            changed = set_channel_policy(args.feed_json, args.version, args.repository, args.action, args.target_version, args.reason)
            print("更新策略已修改" if changed else "更新策略未变化")
        else:
            feed = assemble_release(args.artifacts, args.output, args.version, args.repository)
            print(f"发布资产验证通过：{args.version} / {config['channel']} / {len(feed['releases'])} 个平台更新包")
    except (ValueError, OSError) as error:
        parser.exit(1, f"发布校验失败：{error}\n")


if __name__ == "__main__":
    main()
