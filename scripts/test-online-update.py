#!/usr/bin/env python3
# 中文说明：用临时签名、本地 HTTP 更新源和独立后台验证在线检查、浏览器预检、实际版本重启、坏候选拒绝及回滚；不发布到互联网。
from contextlib import closing
import hashlib
import http.server
import json
import os
import pathlib
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request


def run(arguments, **kwargs):
    result = subprocess.run([str(arg) for arg in arguments], text=True, capture_output=True, timeout=180, **kwargs)
    if result.returncode:
        raise AssertionError(f"命令失败：{arguments}\n{result.stdout}\n{result.stderr}")
    return result.stdout


def main():
    if len(sys.argv) != 3:
        raise SystemExit("用法：python3 scripts/test-online-update.py '/旧 payload' '/新 payload'")
    old, new = [pathlib.Path(arg).resolve() for arg in sys.argv[1:]]
    project = pathlib.Path(__file__).resolve().parents[1]
    scratch = pathlib.Path(tempfile.mkdtemp(prefix="chronicle-online-smoke-"))
    profile, source, updates = [scratch / name for name in ["profile", "source", "updates"]]
    profile.mkdir()
    source.mkdir()
    sample = source / "rollout.jsonl"
    sample.write_text('{"type":"session_meta","payload":{"id":"online-update-fixture"}}\n', encoding="utf-8")
    original = hashlib.sha256(sample.read_bytes()).hexdigest()
    environment = dict(os.environ)
    environment.pop("CODEX_HOME", None)
    seed = scratch / "temporary-signing-seed"
    seed.write_bytes(os.urandom(32))
    seed.chmod(0o600)
    key_helper = scratch / "public.go"
    key_helper.write_text('''// 中文说明：从本次临时 seed 推导公开验签密钥，不打印私钥。
package main
import("crypto/ed25519";"encoding/base64";"fmt";"os")
func main(){seed,err:=os.ReadFile(os.Args[1]);if err!=nil{panic(err)};fmt.Print(base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)))}
''', encoding="utf-8")
    public = scratch / "trusted-public-key"
    public.write_text(run(["go", "run", key_helper, seed], cwd=project), encoding="utf-8")
    binaries = ".exe" if sys.platform == "win32" else ""

    def package(payload, version, output, daemon=None):
        run(["go", "run", "./cmd/codex-chronicle-release", "--version", version, "--build-id", "online-smoke-" + version,
             "--daemon", daemon or payload / ("bin/codex-chronicle" + binaries),
             "--launcher", payload / ("bin/codex-chronicle-desktop" + binaries),
             "--trash-bridge", payload / ("bin/chronicle-native-trash" + binaries), "--web-dist", payload / "web",
             "--signing-private-key-file", seed, "--signing-key-id", "temporary-test-key", "--format", "zip" if output.suffix == ".zip" else "directory", "--output", output], cwd=project)

    old_version = json.loads((old / "codex-chronicle-release.json").read_text())["version"]
    new_version = json.loads((new / "codex-chronicle-release.json").read_text())["version"]
    signed_old, signed_new, archive = scratch / "old-payload", scratch / "new-payload", scratch / "new.zip"
    package(old, old_version, signed_old)
    package(new, new_version, signed_new)
    package(new, new_version, archive)
    artifact = archive.read_bytes()
    release = {"version": new_version, "build_id": "online-smoke-" + new_version, "os": json.loads((new / "codex-chronicle-release.json").read_text())["os"],
               "arch": json.loads((new / "codex-chronicle-release.json").read_text())["arch"], "sha256": hashlib.sha256(artifact).hexdigest(), "size_bytes": len(artifact)}
    payloads = {"/release.zip": artifact}
    slow_download = threading.Event()
    release_download = threading.Event()

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/continue":
                release_download.set()
                self.send_response(200)
                self.end_headers()
                return
            if self.path == "/feed.json":
                value = dict(release, url=f"http://127.0.0.1:{self.server.server_port}/release.zip")
                body = json.dumps({"format_version": 1, "product": "codex-chronicle", "releases": [value]}).encode()
            else:
                body = payloads.get(self.path)
                if body is None:
                    self.send_error(404)
                    return
            offset = 0
            if self.path == "/release.zip" and self.headers.get("Range"):
                value = self.headers["Range"]
                assert value.startswith("bytes=") and value.endswith("-")
                offset = int(value[6:-1])
                assert 0 < offset < len(body)
            total = len(body)
            body = body[offset:]
            self.send_response(206 if offset else 200)
            if offset:
                self.send_header("Content-Range", f"bytes {offset}-{total-1}/{total}")
            self.send_header("ETag", '"' + hashlib.sha256(payloads.get(self.path, body)).hexdigest() + '"')
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if self.path == "/release.zip" and slow_download.is_set():
                self.wfile.write(body[:1024])
                self.wfile.flush()
                release_download.wait(30)
                body = body[1024:]
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass

        def log_message(self, *_):
            pass

    feed = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=feed.serve_forever, daemon=True).start()
    feed_url = f"http://127.0.0.1:{feed.server_port}/feed.json"
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    daemon_process = None
    address = None
    logfile = open(scratch / "daemon.log", "w", encoding="utf-8")
    launcher = new / ("bin/codex-chronicle-desktop" + binaries)

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

    def stop():
        nonlocal daemon_process
        if daemon_process and daemon_process.poll() is None:
            run([launcher, "--quit", "--data-dir", profile])
            daemon_process.wait(timeout=20)
        daemon_process = None

    def start(payload):
        nonlocal daemon_process, address
        args = [payload / ("bin/codex-chronicle" + binaries), "--address", "127.0.0.1:0", "--scan-root", source,
                "--desktop-state-dir", profile / "runtime", "--archive-db", profile / "archive.db", "--web-root", payload / "web",
                "--workspace-config", profile / "workspaces.json", "--auth-state", profile / "auth.json",
                "--trash-settings", profile / "trash-settings.json", "--github-settings", profile / "github-settings.json",
                "--github-archive-selection", profile / "github-selection.json", "--github-account", "chronicle-isolated-update-test",
                "--journal-root", profile / "lifecycle", "--remote-recovery-root", profile / "remote-recovery",
                "--install-root", updates, "--update-require-signature", "--update-trusted-public-key-file", public, "--update-feed-url", feed_url]
        daemon_process = subprocess.Popen([str(arg) for arg in args], env=environment, stdout=logfile, stderr=logfile)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if daemon_process.poll() is not None:
                raise AssertionError("隔离后台启动失败：" + (scratch / "daemon.log").read_text())
            result = subprocess.run([str(launcher), "--status", "--data-dir", str(profile)], capture_output=True, text=True, timeout=5)
            if result.returncode == 0:
                address = json.loads(result.stdout)["url"]
                return
            time.sleep(.1)
        raise AssertionError("隔离后台启动超时")

    try:
        start(signed_old)
        assert request("/healthz")["version"] == old_version
        baseline = request("/api/v1/update/prepare", {"bundle_root": str(signed_old)}, 201)["plan"]
        request("/api/v1/update/activate", {"plan_id": baseline["id"]})
        with closing(sqlite3.connect(profile / "archive.db")) as db:
            db.execute("CREATE TABLE online_update_marker(value TEXT)")
            db.execute("INSERT INTO online_update_marker VALUES ('跨版本保留')")
            db.commit()
        check = request("/api/v1/update/check")
        assert check["check"]["release"]["version"] == new_version
        browser = project / ".local/ui/browser.mjs"
        if browser.exists() and pathlib.Path("/Applications/Google Chrome.app").exists():
            slow_download.set()
            script = '''// 中文说明：浏览器只在隔离后台点击在线检查和下载预检，版本切换由后续测试控制。
import {openBrowser} from './.local/ui/browser.mjs';
const b=await openBrowser();
try{
 await b.navigate(process.env.CHRONICLE_TEST_UPDATE_UI_URL+'/settings/system');
 await b.waitFor('document.querySelector(".update-online")');
 await b.evaluate(`[...document.querySelectorAll('.update-online button')].find(e=>e.textContent.includes('检查在线更新')).click()`);
 await b.waitFor('document.querySelector(".update-online-release")');
 await b.evaluate(`[...document.querySelectorAll('.update-online button')].find(e=>e.textContent.includes('下载并预检')).click()`);
 await b.waitFor('document.querySelector(".update-panel progress")?.value > 0');
 await b.evaluate('document.querySelector(".update-panel").scrollIntoView({block:"center"})');
 const progress=await b.evaluate('({value:document.querySelector(".update-panel progress").value,max:document.querySelector(".update-panel progress").max,width:document.querySelector(".update-panel progress").getBoundingClientRect().width,parent:document.querySelector(".update-panel").getBoundingClientRect().width})');
 if(progress.value!==1024||progress.max<=progress.value||progress.width>progress.parent)throw Error('下载进度数值或布局错误：'+JSON.stringify(progress));
 if(process.env.CHRONICLE_TEST_PROGRESS_SCREENSHOT){const fs=await import('node:fs/promises');const shot=await b.send('Page.captureScreenshot',{format:'png'});await fs.writeFile(process.env.CHRONICLE_TEST_PROGRESS_SCREENSHOT,Buffer.from(shot.data,'base64'));}
 await fetch(process.env.CHRONICLE_TEST_FEED_URL+'/continue');
 await b.waitFor('document.querySelector(".update-plan")',60000);
 if(b.events.some(e=>e.method==='Runtime.exceptionThrown'))throw Error('更新页面有运行异常');
 await b.screenshot('online-update-isolated');
}finally{await fetch(process.env.CHRONICLE_TEST_FEED_URL+'/continue').catch(()=>{});await b.close()}
'''
            run(["node", "--input-type=module", "-e", script], cwd=project, env=dict(environment, CHRONICLE_TEST_UPDATE_UI_URL=address, CHRONICLE_TEST_FEED_URL=f"http://127.0.0.1:{feed.server_port}"))
            slow_download.clear()
            plans = request("/api/v1/update/status")["status"]["pending_plan_ids"]
            assert len(plans) == 1
            plan_id = plans[0]
            print("通过：真实浏览器显示下载字节进度、布局不溢出，下载并预检生成计划，无前端运行异常", flush=True)
        else:
            plan_id = request("/api/v1/update/prepare-online", {}, 201)["plan"]["id"]
        activated = request("/api/v1/update/activate", {"plan_id": plan_id})["result"]
        assert activated["active_version"] == new_version
        assert request("/healthz")["version"] == old_version, "切换指针不应偷偷重启正在运行的后台"
        stop()
        start(pathlib.Path(activated["version_root"]))
        assert request("/healthz")["version"] == new_version
        assert not request("/api/v1/update/check")["available"]
        print("通过：下载、强制验签、预检、切换指针、手动重启实际新版本，数据库继续可用", flush=True)

        release["version"] = "99.0.0"
        request("/api/v1/update/prepare-online", {}, 409)
        release["version"] = new_version
        invalid_daemon = scratch / "invalid-daemon"
        invalid_daemon.write_text("#!/bin/sh\n# 中文说明：仅供隔离实验制造无法启动的更新。\nexit 31\n", encoding="utf-8")
        invalid_daemon.chmod(0o755)
        bad_archive = scratch / "bad.zip"
        package(new, "99.0.0", bad_archive, daemon=invalid_daemon)
        bad = bad_archive.read_bytes()
        payloads["/release.zip"] = bad
        release.update(version="99.0.0", build_id="online-smoke-99.0.0", sha256=hashlib.sha256(bad).hexdigest(), size_bytes=len(bad))
        bad_plan = request("/api/v1/update/prepare-online", {}, 201)["plan"]
        failure = request("/api/v1/update/activate", {"plan_id": bad_plan["id"]}, 409)
        assert "隔离启动检查失败" in failure["error"]
        assert request("/api/v1/update/status")["status"]["state"]["active_version"] == new_version
        assert request("/healthz")["version"] == new_version
        print("通过：feed 版本不符被拒绝；签名正确但无法启动的候选被拒绝，当前后台和版本保持可用", flush=True)
        rollback = request("/api/v1/update/rollback", {})["result"]
        assert rollback["active_version"] == old_version
        stop()
        start(pathlib.Path(rollback["version_root"]))
        assert request("/healthz")["version"] == old_version
        with closing(sqlite3.connect(profile / "archive.db")) as db:
            assert db.execute("PRAGMA quick_check").fetchone()[0] == "ok"
            assert db.execute("SELECT value FROM online_update_marker").fetchone()[0] == "跨版本保留"
        assert hashlib.sha256(sample.read_bytes()).hexdigest() == original
        print("通过：回滚后实际运行旧版本、项目数据库与源样本均保留", flush=True)

        # 下载进行中只强杀本次 Popen 的隔离后台；重启后保留片段并从 HTTP Range 继续下载。
        current = request("/api/v1/update/rollback", {})["result"]
        assert current["active_version"] == new_version
        stop()
        start(pathlib.Path(current["version_root"]))
        assert request("/healthz")["version"] == new_version
        slow_download.set()
        release_download.clear()
        interrupted = []

        def downloading():
            try:
                request("/api/v1/update/prepare-online", {}, 201)
            except Exception as error:
                interrupted.append(type(error).__name__)

        download_thread = threading.Thread(target=downloading, daemon=True)
        download_thread.start()
        deadline = time.monotonic() + 10
        while not any(path.stat().st_size for path in (updates / "partials").glob("*.part")):
            assert time.monotonic() < deadline, "下载没有抵达强杀检查点"
            time.sleep(.05)
        daemon_process.kill()
        daemon_process.wait(timeout=10)
        daemon_process = None
        slow_download.clear()
        release_download.set()
        download_thread.join(timeout=10)
        assert interrupted and not download_thread.is_alive(), "进程被强杀后下载请求未结束"
        start(pathlib.Path(current["version_root"]))
        assert any(path.stat().st_size == 1024 for path in (updates / "partials").glob("*.part")), "重启后续传片段丢失"
        assert not list((updates / "bundles").glob("*.tmp"))
        assert not list((updates / "bundles").glob("*.tmp.lock"))
        assert request("/healthz")["version"] == new_version
        assert request("/api/v1/update/status")["status"]["state"]["active_version"] == new_version
        retry_plan = request("/api/v1/update/prepare-online", {}, 201)["plan"]
        assert retry_plan["version"] == "99.0.0"
        assert not list((updates / "partials").glob("*.part")), "续传成功后仍保留片段"
        assert hashlib.sha256(sample.read_bytes()).hexdigest() == original
        print("通过：实际后台下载中强杀，重启后保留片段并续传成功，原版本与源文件继续可用", flush=True)
    finally:
        try:
            stop()
        finally:
            feed.shutdown()
            feed.server_close()
            logfile.close()
        if daemon_process is None:
            shutil.rmtree(scratch)
        else:
            print("保留尚未退出的实验目录：" + str(scratch), file=sys.stderr)


if __name__ == "__main__":
    main()
