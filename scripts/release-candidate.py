#!/usr/bin/env python3
# 中文说明：冻结最终发布字节、验证候选来源和 GitHub 构建证明；公开发布只复用验收产物，不重新构建或覆盖资产。
import argparse
import base64
import datetime
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import zipfile


spec = importlib.util.spec_from_file_location("release_feed", Path(__file__).with_name("release-feed.py"))
release_feed = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release_feed)
MANIFEST = "codex-chronicle-candidate.json"
WORKFLOW = ".github/workflows/package-preview.yml"
SEAL_JOB = "冻结候选与构建来源"
COMMIT = re.compile(r"[0-9a-f]{40}")
DIGEST = re.compile(r"[0-9a-f]{64}")


def read_json(path, limit=256 << 10):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > limit:
        raise ValueError("候选元数据不是有效普通文件")
    return json.loads(path.read_text(encoding="utf-8"))


def package_names(version):
    return {name for names in release_feed.expected_packages(version).values() for name in names}


def verify_payload(root, version, repository, commit):
    if root.is_symlink() or not root.is_dir():
        raise ValueError("候选根不是有效目录")
    expected = package_names(version) | {release_feed.FEED_NAME, "SHA256SUMS"}
    actual = {path.name for path in root.iterdir()} - {MANIFEST}
    if actual != expected or any(path.is_symlink() or not path.is_file() for path in root.iterdir()):
        raise ValueError("候选资产缺失、重复或包含非预期文件")
    feed = read_json(root / release_feed.FEED_NAME, 4 << 20)
    release_feed.validate_channel_feed(feed, version, repository)
    if feed.get("updates_paused") or feed.get("withdrawn_versions") or feed.get("held_releases"):
        raise ValueError("冻结候选不能包含可变通道策略")
    sums = {}
    for line in (root / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([^/\\]+)", line)
        if not match or match[2] in sums:
            raise ValueError("候选摘要清单无效或重复")
        sums[match[2]] = match[1]
    if set(sums) != expected - {"SHA256SUMS"}:
        raise ValueError("候选摘要清单未完整覆盖发布文件")
    for name, digest in sums.items():
        if release_feed.file_digest(root / name) != digest:
            raise ValueError("候选文件摘要不匹配：" + name)
    for item in feed["releases"]:
        name = item["url"].rsplit("/", 1)[1]
        path = root / name
        if item.get("build_id") != commit or item["sha256"] != sums[name] or item["size_bytes"] != path.stat().st_size:
            raise ValueError("更新索引与候选构建身份不一致")
        with zipfile.ZipFile(path) as archive:
            entry = archive.getinfo("codex-chronicle-release.json")
            if entry.file_size > 4 << 20:
                raise ValueError("包内清单超过大小限制")
            manifest = json.loads(archive.read(entry))
        if (manifest.get("version"), manifest.get("build_id"), manifest.get("os"), manifest.get("arch")) != (version, commit, item["os"], item["arch"]):
            raise ValueError("最终 ZIP 与候选身份不一致")
        if not manifest.get("signature"):
            raise ValueError("最终 ZIP 缺少更新签名")
    return expected


def seal(root, version, repository, commit, run_id, attempt, notes):
    config = release_feed.release_config(version, repository)
    if not COMMIT.fullmatch(commit) or run_id <= 0 or attempt <= 0:
        raise ValueError("必须固定完整源码提交和有效构建编号")
    if not notes.strip() or len(notes.encode()) > 64 << 10:
        raise ValueError("发布说明为空或超过大小限制")
    if (root / MANIFEST).exists():
        raise ValueError("候选已经冻结，拒绝重新生成")
    names = verify_payload(root, version, repository, commit)
    manifest = {"format_version": 1, "product": "codex-chronicle", "repository": repository,
                "version": version, "channel": config["channel"], "source_commit": commit,
                "workflow": WORKFLOW, "run_id": run_id, "run_attempt": attempt,
                "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "release_notes": notes,
                "files": [{"name": name, "size_bytes": (root / name).stat().st_size,
                           "sha256": release_feed.file_digest(root / name)} for name in sorted(names)]}
    path = root / MANIFEST
    with path.open("x", encoding="utf-8") as target:
        target.write(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
    return release_feed.file_digest(path)


def verify(root, version, repository, expected_digest, selection=None):
    path = root / MANIFEST
    manifest = read_json(path)
    if not DIGEST.fullmatch(expected_digest) or release_feed.file_digest(path) != expected_digest:
        raise ValueError("候选清单摘要与指定验收记录不符")
    config = release_feed.release_config(version, repository)
    if (manifest.get("format_version"), manifest.get("product"), manifest.get("repository"), manifest.get("version"), manifest.get("channel"), manifest.get("workflow")) != (1, "codex-chronicle", repository, version, config["channel"], WORKFLOW):
        raise ValueError("候选清单版本、仓库或工作流不匹配")
    commit = manifest.get("source_commit", "")
    if not isinstance(commit, str) or not COMMIT.fullmatch(commit):
        raise ValueError("候选源码提交无效")
    for field in ("run_id", "run_attempt"):
        if type(manifest.get(field)) is not int or manifest[field] <= 0:
            raise ValueError("候选构建编号无效")
    if selection and any(manifest.get(key) != selection.get(key) for key in ("source_commit", "run_id", "run_attempt", "repository")):
        raise ValueError("候选不属于所选构建，或该构建已经重新运行")
    files = manifest.get("files")
    expected = verify_payload(root, version, repository, commit)
    if not isinstance(files, list) or len(files) != len(expected) or {item.get("name") for item in files if isinstance(item, dict)} != expected:
        raise ValueError("冻结清单没有完整覆盖候选资产")
    for item in files:
        path = root / item["name"]
        if item.get("size_bytes") != path.stat().st_size or item.get("sha256") != release_feed.file_digest(path):
            raise ValueError("冻结后的候选文件已变化：" + item["name"])
    if not isinstance(manifest.get("release_notes"), str) or not manifest["release_notes"].strip() or len(manifest["release_notes"].encode()) > 64 << 10:
        raise ValueError("候选发布说明无效")
    return manifest


def gh(arguments, *, body=None, missing=False):
    args = ["gh", *arguments]
    if body is not None:
        args += ["--input", "-"]
    result = subprocess.run(args, input=None if body is None else json.dumps(body), capture_output=True, text=True, timeout=180)
    if result.returncode:
        if missing and "(HTTP 404)" in result.stderr:
            return None
        raise ValueError("GitHub 操作失败（" + " ".join(arguments[:2]) + "）：" + result.stderr.strip())
    return json.loads(result.stdout) if result.stdout.strip() else None


def select_run(run, jobs, artifacts, version, repository, workflow_id, publishing=False):
    release_feed.release_config(version, repository)
    if (run.get("repository", {}).get("full_name"), run.get("head_repository", {}).get("full_name")) != (repository, repository):
        raise ValueError("不接受其他仓库或 fork 的候选")
    if run.get("status") != "completed" or run.get("conclusion") != "success" or run.get("event") != "workflow_dispatch" or run.get("workflow_id") != workflow_id or run.get("path", "").split("@")[0] != WORKFLOW:
        raise ValueError("候选构建尚未成功，或不属于受信任的构建工作流")
    commit, branch = run.get("head_sha", ""), run.get("head_branch", "")
    if not COMMIT.fullmatch(commit) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._/-]{0,200}", branch):
        raise ValueError("候选构建的提交或分支无效")
    if publishing and branch != "main":
        raise ValueError("公开版本只能来自 main 分支的成功候选")
    required = {"回归 / " + runner for runner in ("windows-latest", "ubuntu-latest", "macos-latest")}
    required |= {"安装包 / " + operating_system + "-" + arch for operating_system in ("darwin", "windows", "linux") for arch in ("amd64", "arm64")}
    required.add(SEAL_JOB)
    for name in required:
        matching = [job for job in jobs if job.get("name") == name]
        if len(matching) != 1 or matching[0].get("conclusion") != "success":
            raise ValueError("候选验收门禁没有通过：" + name)
    run_id, attempt = run.get("id"), run.get("run_attempt")
    if type(run_id) is not int or type(attempt) is not int or run_id <= 0 or attempt <= 0:
        raise ValueError("候选构建编号无效")
    name = f"Codex-Chronicle-candidate-{version}-{run_id}-{attempt}"
    matching = [artifact for artifact in artifacts if artifact.get("name") == name and not artifact.get("expired")]
    if len(matching) != 1 or type(matching[0].get("id")) is not int or not re.fullmatch(r"sha256:[0-9a-f]{64}", matching[0].get("digest", "")):
        raise ValueError("所选构建没有唯一、未过期且有摘要的冻结候选")
    return {"repository": repository, "version": version, "source_commit": commit, "source_branch": branch,
            "run_id": run_id, "run_attempt": attempt, "artifact_id": matching[0]["id"], "artifact_digest": matching[0]["digest"]}


def select_from_github(run_id, version, repository, publishing=False):
    prefix = "repos/" + repository
    run = gh(["api", f"{prefix}/actions/runs/{run_id}"])
    workflow = gh(["api", f"{prefix}/actions/workflows/package-preview.yml"])
    jobs = gh(["api", f"{prefix}/actions/runs/{run_id}/jobs?filter=latest&per_page=100"])
    artifacts = gh(["api", f"{prefix}/actions/runs/{run_id}/artifacts?per_page=100"])
    if jobs.get("total_count", 0) > 100 or artifacts.get("total_count", 0) > 100:
        raise ValueError("候选记录超过验收查询范围，停止发布")
    return select_run(run, jobs.get("jobs", []), artifacts.get("artifacts", []), version, repository, workflow["id"], publishing)


def verify_attestation(root, selection):
    # 身份条件来自 GitHub API 与 OIDC 证书约束，不能仅信任可自行填写的 provenance 正文字段。
    args = ["gh", "attestation", "verify", str(root / MANIFEST), "--repo", selection["repository"],
            "--signer-workflow", selection["repository"] + "/" + WORKFLOW,
            "--source-digest", selection["source_commit"], "--signer-digest", selection["source_commit"],
            "--source-ref", "refs/heads/" + selection["source_branch"], "--deny-self-hosted-runners"]
    result = subprocess.run(args, capture_output=True, text=True, timeout=180)
    if result.returncode:
        raise ValueError("GitHub 构建来源证明未通过：" + result.stderr.strip())


def resolve_tag(repository, tag):
    ref = gh(["api", f"repos/{repository}/git/ref/tags/{tag}"], missing=True)
    if ref is None:
        return None
    obj = ref["object"]
    for _ in range(8):
        if obj.get("type") == "commit":
            return obj.get("sha")
        if obj.get("type") != "tag":
            break
        obj = gh(["api", f"repos/{repository}/git/tags/{obj['sha']}"])["object"]
    raise ValueError("发布标签不能解析为唯一源码提交")


def verify_channel_order(repository, version):
    config = release_feed.release_config(version, repository)
    entry = gh(["api", f"repos/{repository}/contents/{config['channel']}.json?ref=update-feeds"], missing=True)
    if entry is None:
        return
    if entry.get("encoding") != "base64" or entry.get("size", 0) > 4 << 20:
        raise ValueError("现有更新通道无法安全核验")
    feed = json.loads(base64.b64decode(entry["content"], validate=False))
    items = feed.get("releases") or feed.get("held_releases")
    if not isinstance(items, list) or not items:
        raise ValueError("现有更新通道缺少有效版本")
    current = items[0].get("version")
    release_feed.validate_channel_feed(feed, current, repository)
    def order(value):
        core, _, number = value.partition("-preview.")
        return tuple(int(part) for part in core.split(".")) + (int(number) if number else 0,)
    if order(version) < order(current) or version in {item["version"] for item in feed.get("withdrawn_versions", [])}:
        raise ValueError("目标版本已经落后或已被撤回，停止公开")


def dispatch_publish(run_id, version, repository, expected_digest):
    # 管理设置只由本机操作者读取；不把个人管理令牌交给发布工作流。
    if os.environ.get("GITHUB_ACTIONS") == "true" or not run_id or not DIGEST.fullmatch(expected_digest):
        raise ValueError("请在本机指定成功候选编号和完整 SHA-256 派发发布")
    prefix = "repos/" + repository
    if gh(["api", prefix + "/immutable-releases"]).get("enabled") is not True:
        raise ValueError("仓库未启用不可变发布，停止派发")
    selected = select_from_github(run_id, version, repository, publishing=True)
    commit, tag = selected["source_commit"], "v" + version
    if gh(["api", f"{prefix}/compare/{commit}...main"]).get("status") not in ("ahead", "identical"):
        raise ValueError("候选提交不在当前 main 历史中")
    verify_channel_order(repository, version)
    existing = resolve_tag(repository, tag)
    if existing is not None and existing != commit:
        raise ValueError("同名版本标签指向其他提交，禁止移动标签")
    if existing is None:
        gh(["api", prefix + "/git/refs", "--method", "POST"], body={"ref": "refs/tags/" + tag, "sha": commit})
    gh(["api", prefix + "/actions/workflows/package-preview.yml/dispatches", "--method", "POST"], body={
        "ref": "main", "inputs": {"operation": "publish", "version": version, "candidate_run_id": str(run_id),
        "candidate_sha256": expected_digest, "immutable_releases_confirmed": True}})


def publish(root, manifest, selection, expected_digest):
    repository, version = manifest["repository"], manifest["version"]
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("GITHUB_REF") != "refs/heads/main" or os.environ.get("GITHUB_REPOSITORY") != repository:
        raise ValueError("公开发布只能从本仓库 main 的受控工作流执行")
    if os.environ.get("CHRONICLE_IMMUTABLE_RELEASES_CONFIRMED") != "true":
        raise ValueError("发布操作者尚未核验仓库不可变设置，请先在本机运行 dispatch-publish")
    latest = select_from_github(selection["run_id"], version, repository, publishing=True)
    if latest != selection:
        raise ValueError("验收后候选记录发生变化，停止发布")
    manifest = verify(root, version, repository, expected_digest, selection)
    verify_attestation(root, selection)
    prefix, commit, tag = "repos/" + repository, manifest["source_commit"], "v" + version
    comparison = gh(["api", f"{prefix}/compare/{commit}...main"])
    if comparison.get("status") not in ("ahead", "identical"):
        raise ValueError("候选提交不在当前 main 历史中")
    verify_channel_order(repository, version)
    existing = resolve_tag(repository, tag)
    if existing is not None and existing != commit:
        raise ValueError("同名版本标签指向其他提交，禁止移动标签")
    if existing is None:
        raise ValueError("缺少已核验的版本标签，请先在本机运行 dispatch-publish")
    release = gh(["api", f"{prefix}/releases/tags/{tag}"], missing=True)
    preview = manifest["channel"] == "preview"
    if release is None:
        # 标签已绑定冻结源码；省略无效的 target_commitish，避免为历史候选额外申请 workflows 写权限。
        release = gh(["api", prefix + "/releases", "--method", "POST"], body={"tag_name": tag, "name": "Codex Chronicle " + version,
                      "body": manifest["release_notes"], "draft": True, "prerelease": preview})
    expected = {path.name: path for path in root.iterdir()}
    present = {asset["name"]: asset for asset in release.get("assets", [])}
    if len(present) != len(release.get("assets", [])) or set(present) - set(expected):
        raise ValueError("已有 Release 含重复或非候选资产，停止发布")
    for name, asset in present.items():
        if asset.get("state") != "uploaded" or asset.get("size") != expected[name].stat().st_size or asset.get("digest") != "sha256:" + release_feed.file_digest(expected[name]):
            raise ValueError("已有 Release 的同名资产与候选不符，禁止覆盖：" + name)
    if release.get("draft") is not True:
        release_feed.verify_uploaded_assets(root, release, version, require_draft=False)
        if release.get("immutable") is not True or release.get("prerelease") != preview:
            raise ValueError("同名公开版本不是匹配的不可变发布")
        return release
    if release.get("body") != manifest["release_notes"] or release.get("prerelease") != preview:
        raise ValueError("已有草稿的发布说明或通道与冻结候选不一致")
    missing = [str(path) for name, path in expected.items() if name not in present]
    if missing:
        result = subprocess.run(["gh", "release", "upload", tag, "--repo", repository, *missing], capture_output=True, text=True, timeout=900)
        if result.returncode:
            raise ValueError("草稿上传未完成，可重试同一候选：" + result.stderr.strip())
    release = gh(["api", f"{prefix}/releases/{release['id']}"])
    release_feed.verify_uploaded_assets(root, release, version)
    gh(["api", f"{prefix}/releases/{release['id']}", "--method", "PATCH"], body={"draft": False, "prerelease": preview, "make_latest": "false" if preview else "true"})
    release = gh(["api", f"{prefix}/releases/{release['id']}"])
    release_feed.verify_uploaded_assets(root, release, version, require_draft=False)
    if release.get("immutable") is not True:
        raise ValueError("版本已公开但未报告不可变状态，停止推进更新通道并检查 GitHub 状态")
    return release



def dispatch_recommend(run_id, version, repository, expected_digest, confirmed, note):
    if not confirmed or not note.strip() or len(note) > 2000:
        raise ValueError("推荐前必须确认已完成观察验收并填写观察结论")
    if os.environ.get("GITHUB_ACTIONS") == "true" or not run_id or not DIGEST.fullmatch(expected_digest):
        raise ValueError("请在本机指定候选编号和完整摘要派发推荐")
    selected = select_from_github(run_id, version, repository, publishing=True)
    prefix, tag = "repos/" + repository, "v" + version
    release = gh(["api", f"{prefix}/releases/tags/{tag}"])
    if release.get("draft") is not False or release.get("immutable") is not True or resolve_tag(repository, tag) != selected["source_commit"]:
        raise ValueError("必须先公开与候选源码匹配的不可变 Release")
    verify_channel_order(repository, version)
    gh(["api", prefix + "/actions/workflows/package-preview.yml/dispatches", "--method", "POST"], body={
        "ref": "main", "inputs": {"operation": "recommend", "version": version, "candidate_run_id": str(run_id),
        "candidate_sha256": expected_digest, "observation_confirmed": True, "observation_note": note.strip()}})


def recommend(root, manifest, selection, expected_digest, confirmed, note):
    if not confirmed or not note.strip() or len(note) > 2000:
        raise ValueError("推荐前必须确认已完成观察验收并填写观察结论")
    repository, version = manifest["repository"], manifest["version"]
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("GITHUB_REF") != "refs/heads/main" or os.environ.get("GITHUB_REPOSITORY") != repository:
        raise ValueError("自动推荐只能从本仓库 main 的受控工作流执行")
    latest = select_from_github(selection["run_id"], version, repository, publishing=True)
    if latest != selection:
        raise ValueError("观察期间候选记录发生变化，停止推荐")
    manifest = verify(root, version, repository, expected_digest, selection)
    verify_attestation(root, selection)
    prefix, tag = "repos/" + repository, "v" + version
    if resolve_tag(repository, tag) != manifest["source_commit"]:
        raise ValueError("公开标签与观察候选的源码不一致")
    if gh(["api", f"{prefix}/compare/{manifest['source_commit']}...main"]).get("status") not in ("ahead", "identical"):
        raise ValueError("候选不在 main 历史中")
    verify_channel_order(repository, version)
    release = gh(["api", f"{prefix}/releases/tags/{tag}"])
    if release.get("draft") is not False or release.get("immutable") is not True or release.get("prerelease") != (manifest["channel"] == "preview"):
        raise ValueError("公开版本不是匹配的不可变 Release")
    release_feed.verify_uploaded_assets(root, release, version, require_draft=False)
    print(json.dumps({"recommended_version": version, "release_id": release["id"], "source_commit": manifest["source_commit"],
                      "observation_note": note.strip()}, ensure_ascii=False))
    return release


def main():
    parser = argparse.ArgumentParser(description="冻结、验证和推广同一份已验收的 GitHub 发布候选")
    parser.add_argument("command", choices=("seal", "select", "verify", "dispatch-publish", "publish", "dispatch-recommend", "recommend"))
    parser.add_argument("--version", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--root", type=Path, default=Path("publish"))
    parser.add_argument("--commit", default="")
    parser.add_argument("--run-id", type=int)
    parser.add_argument("--attempt", type=int)
    parser.add_argument("--notes-file", type=Path)
    parser.add_argument("--expected-sha256", default="")
    parser.add_argument("--selection", type=Path)
    parser.add_argument("--for-publish", action="store_true")
    parser.add_argument("--attestation", action="store_true")
    parser.add_argument("--observation-confirmed", action="store_true")
    parser.add_argument("--observation-note", default="")
    args = parser.parse_args()
    try:
        config = release_feed.release_config(args.version, args.repository)
        if args.command == "seal":
            notes_path = args.notes_file
            if notes_path is None:
                if config["channel"] != "preview":
                    raise ValueError("正式版候选必须明确指定已审核的发布说明")
                notes_path = Path("packaging/预览版说明.md")
            project = Path(__file__).resolve().parents[1]
            if notes_path.is_absolute() or notes_path.suffix.lower() != ".md" or notes_path.is_symlink() or not notes_path.resolve().is_relative_to(project):
                raise ValueError("发布说明必须是仓库内受版本管理的 Markdown 文件")
            if subprocess.run(["git", "ls-files", "--error-unmatch", "--", str(notes_path)], capture_output=True).returncode:
                raise ValueError("发布说明尚未纳入版本管理")
            digest = seal(args.root, args.version, args.repository, args.commit, args.run_id or 0, args.attempt or 0, notes_path.read_text(encoding="utf-8"))
            print("candidate_sha256=" + digest)
        elif args.command == "dispatch-publish":
            dispatch_publish(args.run_id, args.version, args.repository, args.expected_sha256)
            print("已核验仓库设置、固定候选标签并派发同包发布")
        elif args.command == "dispatch-recommend":
            dispatch_recommend(args.run_id, args.version, args.repository, args.expected_sha256, args.observation_confirmed, args.observation_note)
            print("已派发独立推荐；工作流将重新核验公开资产与观察候选")
        elif args.command == "select":
            if not args.run_id or args.selection is None:
                raise ValueError("选择候选必须指定构建编号和 selection 文件")
            selected = select_from_github(args.run_id, args.version, args.repository, args.for_publish)
            args.selection.write_text(json.dumps(selected), encoding="utf-8")
            for key in ("artifact_id", "source_commit", "source_branch", "run_attempt"):
                print(f"{key}={selected[key]}")
        else:
            if args.selection is None:
                raise ValueError("验证候选必须指定 selection 文件")
            selection = read_json(args.selection)
            manifest = verify(args.root, args.version, args.repository, args.expected_sha256, selection)
            if args.attestation:
                verify_attestation(args.root, selection)
            if args.command == "publish":
                publish(args.root, manifest, selection, args.expected_sha256)
                print("Release已公开，静态自动更新通道保持不变；观察后另行推荐")
            elif args.command == "recommend":
                recommend(args.root, manifest, selection, args.expected_sha256, args.observation_confirmed, args.observation_note)
            print("已公开并核验同一候选" if args.command == "publish" else "候选字节、来源身份与指定验收记录核对通过")
    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError, zipfile.BadZipFile) as error:
        parser.exit(1, f"发布候选检查失败：{error}\n")


if __name__ == "__main__":
    main()
