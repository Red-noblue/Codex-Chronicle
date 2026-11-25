#!/usr/bin/env python3
# 中文说明：使用临时签名和本机更新源，验证真实安装入口的首次基线、完整版本重启、坏候选拒绝及回滚；不修改用户安装。
from contextlib import closing
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import threading
import urllib.error
import urllib.request


def run(arguments, **kwargs):
    result = subprocess.run([str(arg) for arg in arguments], capture_output=True, text=True, timeout=180, **kwargs)
    if result.returncode:
        raise AssertionError(f"命令失败：{arguments}\n{result.stdout}\n{result.stderr}")
    return result.stdout


def main():
    project = Path(__file__).resolve().parents[1]
    scratch = Path(tempfile.mkdtemp(prefix="chronicle-installed-update-"))
    environment = dict(os.environ, CHRONICLE_NO_OPEN="1")
    environment.pop("CODEX_HOME", None)
    suffix = ".exe" if sys.platform == "win32" else ""
    profile, source = scratch / "profile", scratch / "source"
    profile.mkdir()
    source.mkdir()
    sample = source / "sentinel.jsonl"
    sample.write_text('{"safe":"原始对话源不变"}\n', encoding="utf-8")
    original = hashlib.sha256(sample.read_bytes()).hexdigest()
    (profile / "launcher.json").write_text(json.dumps({"scan_root": str(source)}), encoding="utf-8")
    seed = scratch / "temporary-signing-seed"
    seed.write_bytes(os.urandom(32))
    seed.chmod(0o600)
    release = {}
    artifacts = {}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path in ("/preview.json", "/stable.json"):
                body = json.dumps({"format_version": 1, "product": "codex-chronicle", "releases": [release]}).encode()
            else:
                body = artifacts.get(self.path)
                if body is None:
                    self.send_error(404)
                    return
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base_url = f"http://127.0.0.1:{server.server_port}"
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    address = ""
    launcher = None
    safe_to_cleanup = True

    def request(path, body=None, expected=200):
        data = None if body is None else json.dumps(body).encode()
        value = urllib.request.Request(address + path, data=data, headers={"Content-Type": "application/json"})
        try:
            response = opener.open(value, timeout=120)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            result = json.load(response)
            assert response.status == expected, (path, response.status, result)
            return result

    def launch(entry):
        nonlocal address, safe_to_cleanup
        safe_to_cleanup = False
        instance = json.loads(run([entry, "--no-browser", "--data-dir", profile, "--address", "127.0.0.1:0"], env=environment))
        address = instance["url"]
        return instance

    def stop():
        nonlocal safe_to_cleanup
        if launcher is not None:
            run([launcher, "--quit", "--data-dir", profile], env=environment)
        safe_to_cleanup = True

    def select_release(version, archive):
        artifact = archive.read_bytes()
        artifacts["/release.zip"] = artifact
        release.update(version=version, os=target_os, arch=arch, url=base_url + "/release.zip",
                       sha256=hashlib.sha256(artifact).hexdigest(), size_bytes=len(artifact))

    try:
        target_os = run(["go", "env", "GOOS"], cwd=project).strip()
        arch = run(["go", "env", "GOARCH"], cwd=project).strip()
        packager = scratch / ("packager" + suffix)
        trash = scratch / ("chronicle-native-trash" + suffix)
        run(["go", "build", "-o", packager, "./cmd/codex-chronicle-release"], cwd=project, env=environment)
        run(["go", "build", "-o", trash, "./cmd/chronicle-native-trash"], cwd=project, env=environment)

        def package(version, name, feed_path, bad=False):
            binaries = scratch / (name + "-bin")
            binaries.mkdir()
            daemon = binaries / ("codex-chronicle" + suffix)
            entry = binaries / ("codex-chronicle-desktop" + suffix)
            if bad:
                failure = scratch / "invalid-daemon.go"
                failure.write_text('// 中文说明：仅为本轮隔离验证制造无法启动的已签名候选。\npackage main\nimport "os"\nfunc main(){os.Exit(31)}\n', encoding="utf-8")
                run(["go", "build", "-o", daemon, failure], cwd=project, env=environment)
            else:
                run(["go", "build", "-ldflags", "-X main.version=" + version, "-o", daemon, "./cmd/codex-chronicle"], cwd=project, env=environment)
            run(["go", "build", "-ldflags", f"-X main.version={version} -X main.updateFeedURL={base_url}{feed_path}",
                 "-o", entry, "./cmd/codex-chronicle-desktop"], cwd=project, env=environment)
            arguments = [packager, "--version", version, "--daemon", daemon, "--launcher", entry, "--trash-bridge", trash,
                         "--web-dist", project / "web/dist", "--signing-private-key-file", seed, "--signing-key-id", "temporary-test-key"]
            payload, archive = scratch / name, scratch / (name + ".zip")
            run(arguments + ["--format", "directory", "--output", payload], cwd=project)
            run(arguments + ["--format", "zip", "--output", archive], cwd=project)
            return payload, archive, arguments

        old_version, new_version, later_version = "1.0.0-preview.1", "1.0.0", "1.1.0"
        old, _, old_args = package(old_version, "installed", "/preview.json")
        new, new_archive, new_args = package(new_version, "updated", "/stable.json")
        launcher = old / ("bin/codex-chronicle-desktop" + suffix)
        first = launch(launcher)
        state = request("/api/v1/update/status")["status"]
        assert first["version"] == old_version
        assert state["state"]["active_version"] == old_version and state["state"]["installed_version"] == old_version
        assert not state["state"].get("previous_version") and state["signature_required"]
        assert state["online_feed_url"] == base_url + "/preview.json"
        assert json.loads(run([launcher, "--no-browser", "--data-dir", profile], env=environment))["id"] == first["id"]
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            database.execute("CREATE TABLE installed_update_marker(value TEXT)")
            database.execute("INSERT INTO installed_update_marker VALUES ('跨版本保留')")
            database.commit()
        print("通过：首次启动自动建立签名基线、版本准确、重复打开复用已有后台", flush=True)

        select_release(new_version, new_archive)
        plan = request("/api/v1/update/prepare-online", {}, 201)["plan"]
        assert plan["previous_version"] == old_version
        assert request("/api/v1/update/activate", {"plan_id": plan["id"]})["restart_required"]
        assert request("/healthz")["version"] == old_version
        stop()
        second = launch(launcher)
        state = request("/api/v1/update/status")["status"]
        assert second["version"] == new_version and second["id"] != first["id"]
        assert state["state"]["previous_version"] == old_version
        assert state["online_feed_url"] == base_url + "/stable.json", "旧启动器沿用了旧更新渠道"
        assert not request("/api/v1/update/check")["available"]
        print("通过：原安装入口重启到完整新版本，启动器更新渠道同步切换，保留首次回滚基线", flush=True)

        _, bad_archive, _ = package(later_version, "bad", "/stable.json", bad=True)
        select_release(later_version, bad_archive)
        bad_plan = request("/api/v1/update/prepare-online", {}, 201)["plan"]
        failure = request("/api/v1/update/activate", {"plan_id": bad_plan["id"]}, 409)
        assert "隔离启动检查失败" in failure["error"]
        assert request("/api/v1/update/status")["status"]["state"]["active_version"] == new_version
        assert request("/healthz")["version"] == new_version
        print("通过：签名正确但无法启动的候选被拒绝，当前版本和运行后台继续可用", flush=True)

        request("/api/v1/update/rollback", {})
        stop()
        assert launch(launcher)["version"] == old_version
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            assert database.execute("PRAGMA quick_check").fetchone()[0] == "ok"
            assert database.execute("SELECT value FROM installed_update_marker").fetchone()[0] == "跨版本保留"
        print("通过：从原入口重启实际回滚到旧版本，数据库记录保留", flush=True)

        stop()
        later, _, _ = package(later_version, "new-installer", "/stable.json")
        launcher = later / ("bin/codex-chronicle-desktop" + suffix)
        assert launch(launcher)["version"] == later_version
        state = request("/api/v1/update/status")["status"]["state"]
        assert state["installed_version"] == later_version and state["previous_version"] == old_version
        request("/api/v1/update/rollback", {})
        stop()
        assert launch(launcher)["version"] == old_version, "再次打开新安装器撤销了人工回滚"
        assert hashlib.sha256(sample.read_bytes()).hexdigest() == original
        stop()
        print("通过：新安装器不被旧指针遮蔽，人工回滚不会被下次打开撤销，源文件摘要不变", flush=True)

        if sys.platform == "darwin":
            old_app, new_app = scratch / "old.app", scratch / "new.app"
            for arguments, app in ((old_args, old_app), (new_args, new_app)):
                run(arguments + ["--format", "macos-app", "--macos-installer", project / "packaging/macos/install.sh", "--output", app], cwd=project)
                run(["codesign", "--force", "--sign", "-", app])
            for script in ("test-macos-install.py", "test-macos-install-interruption.py"):
                print(run([sys.executable, project / "scripts" / script, old_app, new_app], env=environment), end="", flush=True)
    finally:
        try:
            if not safe_to_cleanup:
                stop()
        finally:
            server.shutdown()
            server.server_close()
        if safe_to_cleanup:
            shutil.rmtree(scratch)
        else:
            print("保留尚未退出的隔离测试目录：" + str(scratch), file=sys.stderr)


if __name__ == "__main__":
    main()
