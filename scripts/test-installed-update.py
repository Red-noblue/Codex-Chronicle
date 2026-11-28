#!/usr/bin/env python3
# 中文说明：使用临时签名和本机更新源，验证真实安装入口的首次基线、完整版本重启、坏候选拒绝及回滚；不修改用户安装。
from contextlib import closing
import hashlib
from datetime import datetime, timedelta, timezone
import http.server
import io
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile


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
    policy = {}
    artifacts = {}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            # 同一隔离服务也接收 HTTP 代理的绝对地址请求，覆盖保存代理后的完整启动链路。
            request_path = urllib.parse.urlsplit(self.path).path
            if request_path in ("/preview.json", "/stable.json"):
                feed = {"format_version": 1, "product": "codex-chronicle", "releases": [release], **policy}
                if policy.get("updates_paused") or any(item["version"] == release.get("version") for item in policy.get("withdrawn_versions", [])):
                    feed["held_releases"], feed["releases"] = feed["releases"], []
                body = json.dumps(feed).encode()
            else:
                body = artifacts.get(request_path)
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

    def request(path, body=None, expected=200, method=None):
        data = None if body is None else json.dumps(body).encode()
        value = urllib.request.Request(address + path, data=data, headers={"Content-Type": "application/json"}, method=method)
        try:
            response = opener.open(value, timeout=120)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            result = json.load(response)
            assert response.status == expected, (path, response.status, result)
            return result

    def launch(entry, use_saved_address=False):
        nonlocal address, safe_to_cleanup
        safe_to_cleanup = False
        arguments = [entry, "--no-browser", "--data-dir", profile]
        if not use_saved_address:
            arguments += ["--address", "127.0.0.1:0"]
        instance = json.loads(run(arguments, env=environment))
        address = instance["url"]
        return instance

    def stop():
        nonlocal safe_to_cleanup
        if launcher is not None:
            run([launcher, "--quit", "--data-dir", profile], env=environment)
        safe_to_cleanup = True

    def await_restart(previous_id, expected_version):
        nonlocal address
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            time.sleep(0.2)
            try:
                current = json.loads((profile / "runtime/instance.json").read_text(encoding="utf-8"))
                if current["id"] == previous_id or current["version"] != expected_version:
                    continue
                address = current["url"]
                state = request("/api/v1/update/status")
                confirmed = state["status"]["state"].get("confirmed_at")
                if request("/healthz")["version"] == expected_version and confirmed:
                    return current
            except (OSError, ValueError, AssertionError):
                continue
        restart_log = profile / "logs/update-restart.log"
        details = restart_log.read_text(encoding="utf-8")[-4000:] if restart_log.exists() else "协作进程未留下日志"
        raise AssertionError("更新未能自动重新启动：" + details)

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

        def package(version, name, feed_path, bad=False, fail_profile=False, source_project=project):
            binaries = scratch / (name + "-bin")
            binaries.mkdir()
            daemon = binaries / ("codex-chronicle" + suffix)
            entry = binaries / ("codex-chronicle-desktop" + suffix)
            if bad:
                failure = scratch / "invalid-daemon.go"
                failure.write_text('// 中文说明：仅为本轮隔离验证制造无法启动的已签名候选。\npackage main\nimport "os"\nfunc main(){os.Exit(31)}\n', encoding="utf-8")
                run(["go", "build", "-o", daemon, failure], cwd=project, env=environment)
            else:
                run(["go", "build", "-ldflags", "-X main.version=" + version, "-o", daemon, "./cmd/codex-chronicle"], cwd=source_project, env=environment)
            if fail_profile:
                # 预检使用空白 profile 时正常；仅本测试的实际 profile 启动时模拟写坏数据库后失败。
                reference = binaries / ("reference-daemon" + suffix)
                shutil.copy2(daemon, reference)
                failure = scratch / "profile-failing-daemon.go"
                failure.write_text('''// 中文说明：仅为隔离演练制造预检通过、真实启动写入失败的候选。
package main
import ("os";"os/exec";"path/filepath")
func main(){
 for i,arg:=range os.Args[1:]{if arg=="--archive-db"&&i+2<len(os.Args){p:=os.Args[i+2];dir,_:=filepath.EvalSymlinks(filepath.Dir(p));if dir==PROFILE{_ = os.WriteFile(p,[]byte("模拟失败迁移"),0600);os.Exit(32)}}}
 cmd:=exec.Command(REFERENCE,os.Args[1:]...);cmd.Stdout=os.Stdout;cmd.Stderr=os.Stderr
 if err:=cmd.Run();err!=nil{os.Exit(33)}
}
'''.replace('PROFILE', json.dumps(str(profile.resolve()))).replace('REFERENCE', json.dumps(str(reference.resolve()))), encoding="utf-8")
                run(["go", "build", "-o", daemon, failure], cwd=project, env=environment)
            run(["go", "build", "-ldflags", f"-X main.version={version} -X main.updateFeedURL={base_url}{feed_path}",
                 "-o", entry, "./cmd/codex-chronicle-desktop"], cwd=source_project, env=environment)
            arguments = [packager, "--version", version, "--daemon", daemon, "--launcher", entry, "--trash-bridge", trash,
                         "--web-dist", project / "web/dist", "--signing-private-key-file", seed, "--signing-key-id", "temporary-test-key"]
            payload, archive = scratch / name, scratch / (name + ".zip")
            run(arguments + ["--format", "directory", "--output", payload], cwd=project)
            run(arguments + ["--format", "zip", "--output", archive], cwd=project)
            return payload, archive, arguments

        old_version, new_version, later_version = "1.0.0-preview.1", "1.0.0", "1.1.0"
        old, _, old_args = package(old_version, "installed", "/preview.json")
        new, new_archive, new_args = package(new_version, "updated", "/stable.json")
        if os.environ.get("CHRONICLE_TEST_BROWSER") == "1":
            print(run([sys.executable, project / "scripts/test-online-update.py", old, new], env=environment), end="", flush=True)
        launcher = old / ("bin/codex-chronicle-desktop" + suffix)
        first = launch(launcher)
        request("/api/v1/update/automatic", {"enabled": False}, method="PUT")
        request("/api/v1/update/proxy", {"proxy_url": base_url}, method="PUT")
        (profile / "startup.json").write_text(json.dumps({"port": urllib.parse.urlsplit(address).port, "auto_fallback": True}), encoding="utf-8")
        state = request("/api/v1/update/status")["status"]
        assert first["version"] == old_version
        assert state["state"]["active_version"] == old_version and state["state"]["installed_version"] == old_version
        assert not state["state"].get("previous_version") and state["signature_required"]
        assert state["online_feed_url"] == base_url + "/preview.json"
        assert state["online_proxy_url"] == base_url
        assert json.loads(run([launcher, "--no-browser", "--data-dir", profile], env=environment))["id"] == first["id"]
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            database.execute("CREATE TABLE installed_update_marker(value TEXT)")
            database.execute("INSERT INTO installed_update_marker VALUES ('跨版本保留')")
            database.commit()
        print("通过：首次启动自动建立签名基线、版本准确、重复打开复用已有后台", flush=True)

        select_release(new_version, new_archive)
        plan = request("/api/v1/update/prepare-online", {}, 201)["plan"]
        assert plan["previous_version"] == old_version
        policy["withdrawn_versions"] = [{"version": new_version, "reason": "隔离演练：撤回已下载候选"}]
        failure = request("/api/v1/update/activate", {"plan_id": plan["id"]}, 409)
        assert "撤回" in failure["error"] and request("/healthz")["version"] == old_version
        policy.clear()
        policy.update(updates_paused=True, pause_reason="隔离演练：暂停安装")
        failure = request("/api/v1/update/activate", {"plan_id": plan["id"]}, 409)
        assert "暂停" in failure["error"] and request("/healthz")["version"] == old_version
        policy.clear()
        print("通过：已下载候选在切换前重新核验撤回和暂停策略，原版本继续可用", flush=True)
        response = request("/api/v1/update/activate", {"plan_id": plan["id"], "restart": True})
        assert response["restart_scheduled"], response
        first_url = address
        second = await_restart(first["id"], new_version)
        assert address == first_url, "更新重启未恢复用户保存的端口"
        state = request("/api/v1/update/status")["status"]
        assert second["version"] == new_version and second["id"] != first["id"]
        assert state["state"]["previous_version"] == old_version
        assert state["online_feed_url"] == base_url + "/stable.json", "旧启动器沿用了旧更新渠道"
        assert state["online_proxy_url"] == base_url, "升级后丢失更新代理"
        assert not request("/api/v1/update/check")["available"]
        assert state["state"]["confirmed_at"] and state["state"]["cleanup_after"], state
        assert list((profile / "updates/bundles").glob("download-*.archive")), "观察期内提前清理了安装包"
        assert new_archive.is_file(), "清理触碰了应用缓存外的源安装包"
        print("通过：应用内更新自动退出并重启到新版本，恢复原端口和新渠道，持久化 24 小时恢复窗口", flush=True)

        _, bad_archive, _ = package(later_version, "bad", "/stable.json", bad=True)
        select_release(later_version, bad_archive)
        bad_plan = request("/api/v1/update/prepare-online", {}, 201)["plan"]
        failure = request("/api/v1/update/activate", {"plan_id": bad_plan["id"]}, 409)
        assert "隔离启动检查失败" in failure["error"]
        assert request("/api/v1/update/status")["status"]["state"]["active_version"] == new_version
        assert request("/healthz")["version"] == new_version
        print("通过：签名正确但无法启动的候选被拒绝，当前版本和运行后台继续可用", flush=True)

        request("/api/v1/update/rollback", {})
        assert request("/api/v1/update/restart", {"version": old_version}, 202)["restart_scheduled"]
        second = await_restart(second["id"], old_version)
        assert second["version"] == old_version
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            assert database.execute("PRAGMA quick_check").fetchone()[0] == "ok"
            assert database.execute("SELECT value FROM installed_update_marker").fetchone()[0] == "跨版本保留"
        print("通过：从原入口重启实际回滚到旧版本，数据库记录保留", flush=True)

        _, startup_bad_archive, _ = package("1.0.1", "startup-failing", "/stable.json", fail_profile=True)
        select_release("1.0.1", startup_bad_archive)
        startup_bad_plan = request("/api/v1/update/prepare-online", {}, 201)["plan"]
        response = request("/api/v1/update/activate", {"plan_id": startup_bad_plan["id"], "restart": True})
        assert response["restart_scheduled"], response
        second = await_restart(second["id"], old_version)
        state = request("/api/v1/update/status")["status"]["state"]
        assert state["last_status"] == "startup_rolled_back" and state["rejected_version"] == "1.0.1", state
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            assert database.execute("PRAGMA quick_check").fetchone()[0] == "ok"
            assert database.execute("SELECT value FROM installed_update_marker").fetchone()[0] == "跨版本保留"
        assert not (profile / "updates/restart-journal.json").exists(), "启动失败恢复后遗留事务"
        print("通过：候选预检通过但真实启动失败，已自动恢复旧版本、数据库和设置，并抑制失败版本", flush=True)

        # 恢复后的服务可先于重启协作进程的最终事件就绪，导出前等待明确的失败完成记录。
        diagnostic_deadline = time.monotonic() + 10
        while time.monotonic() < diagnostic_deadline:
            last = request("/api/v1/update/status")["status"].get("last_diagnostic")
            if last and last["stage"] == "restart" and last.get("target_version") == "1.0.1":
                break
            time.sleep(.05)
        else:
            raise AssertionError("恢复完成后没有记录重启失败的最终事件")
        with opener.open(address + "/api/v1/update/diagnostics", timeout=15) as response:
            assert response.headers["Content-Type"] == "application/zip"
            online_diagnostics = response.read()
        with zipfile.ZipFile(io.BytesIO(online_diagnostics)) as archive:
            assert set(archive.namelist()) == {"report.json", "update-events.json"}
            events = json.loads(archive.read("update-events.json"))
            assert any(e["stage"] == "restart" and e["result"] == "failed" and e.get("id") for e in events), events
            assert any(e["stage"] == "restore_snapshot" and e["result"] == "succeeded" for e in events), events
            assert any(e["stage"] == "restore_version" and e["result"] == "succeeded" for e in events), events
            assert str(profile) not in archive.read("report.json").decode(), "诊断泄露用户路径"
        print("通过：实际启动失败与自动恢复均留下故障编号、阶段与结果，网页可下载脱敏诊断包", flush=True)

        stop()
        offline_diagnostics = scratch / "offline-diagnostics.zip"
        run([launcher, "--data-dir", profile, "--export-diagnostics", offline_diagnostics], env=environment)
        with zipfile.ZipFile(offline_diagnostics) as archive:
            assert json.loads(archive.read("report.json"))["format_version"] == 1
            assert any(e["stage"] == "restart" and e["result"] == "failed" for e in json.loads(archive.read("update-events.json")))
        print("通过：后台已停止时启动器仍能离线导出", flush=True)
        later, _, _ = package(later_version, "new-installer", "/stable.json")
        launcher = later / ("bin/codex-chronicle-desktop" + suffix)
        installed_instance = launch(launcher)
        assert installed_instance["version"] == later_version, {"instance": installed_instance, "update": request("/api/v1/update/status")["status"], "events": json.loads((profile / "updates/diagnostic-events.json").read_text())[-12:]}
        state = request("/api/v1/update/status")["status"]["state"]
        assert state["installed_version"] == later_version and state["previous_version"] == old_version
        assert (profile / "updates/versions" / new_version).exists(), "观察期内提前清理了旧版本"
        assert (profile / "updates/versions" / old_version).is_dir(), "清理破坏了可回滚版本"
        assert (profile / "updates/versions" / later_version).is_dir(), "清理破坏了安装基线"
        assert Path(bad_plan["bundle_root"]).is_dir(), "清理删除了仍待处理的候选版本"
        request("/api/v1/update/rollback", {})
        stop()
        assert launch(launcher)["version"] == old_version, "再次打开新安装器撤销了人工回滚"
        assert hashlib.sha256(sample.read_bytes()).hexdigest() == original
        stop()
        print("通过：新安装器不被旧指针遮蔽，人工回滚不会被下次打开撤销，源文件摘要不变", flush=True)

        # 通过正式设置缩短为一分钟，按真实时间等待后台维护；不修改系统时间或伪造健康确认时间。
        assert launch(launcher)["version"] == old_version
        request("/api/v1/update/automatic", {"enabled": False}, method="PUT")
        state = request("/api/v1/update/status")["status"]["state"]
        confirmed_at = state["confirmed_at"]
        ui_helper = project / ".local/ui/browser.mjs"
        if ui_helper.is_file() and Path("/Applications/Google Chrome.app").exists():
            script = '''// 中文说明：仅在隔离实例验证清理设置的确认、保存、刷新持久化和窄屏布局。
import {openBrowser} from './.local/ui/browser.mjs';
const browser=await openBrowser(1280,1000);
try{
 await browser.navigate(process.env.CHRONICLE_CLEANUP_UI_URL+'/settings/system');
 await browser.waitFor('document.querySelector("#update-cleanup-retention")');
 await browser.evaluate(`window.confirm=(message)=>{window.__cleanupPrompt=message;return false};const select=document.querySelector('#update-cleanup-retention');select.value='1';select.dispatchEvent(new Event('change',{bubbles:true}));`);
 await browser.evaluate(`[...document.querySelectorAll('.update-cleanup-field button')].find(e=>e.textContent.includes('保存清理时间')).click()`);
 const cancelled=await browser.evaluate(`fetch('/api/v1/update/status').then(r=>r.json())`);
 if(cancelled.status.state.cleanup_retention_minutes===1)throw Error('取消确认后仍修改了清理期限');
 if(!await browser.evaluate(`window.__cleanupPrompt?.includes('回退')||window.__cleanupPrompt?.includes('旧版本')`))throw Error('缩短期限没有说明回退影响');
 await browser.evaluate(`window.confirm=()=>true;document.querySelector('.update-cleanup-field > button').click()`);
 await browser.waitFor(`document.querySelector('.update-message')?.textContent.includes('已保存')`);
 await browser.navigate(process.env.CHRONICLE_CLEANUP_UI_URL+'/settings/system');
 await browser.waitFor(`document.querySelector('#update-cleanup-retention')?.value==='1'`);
 await browser.evaluate(`document.querySelector('.update-cleanup-field').scrollIntoView({block:'center'})`);
 if(process.env.CHRONICLE_CLEANUP_SCREENSHOT){const fs=await import('node:fs/promises');const shot=await browser.send('Page.captureScreenshot',{format:'png'});await fs.writeFile(process.env.CHRONICLE_CLEANUP_SCREENSHOT,Buffer.from(shot.data,'base64'));}
 await browser.resize(480,900);
 await browser.evaluate(`document.querySelector('.update-cleanup-field').scrollIntoView({block:'center'})`);
 const fits=await browser.evaluate(`(()=>{const field=document.querySelector('.update-cleanup-field').getBoundingClientRect();const select=document.querySelector('#update-cleanup-retention').getBoundingClientRect();return select.left>=field.left-1&&select.right<=field.right+1})()`);
 if(!fits)throw Error('清理设置控件溢出容器');
 if(browser.events.some(e=>e.method==='Runtime.exceptionThrown'))throw Error('清理设置页面出现运行异常');
}finally{await browser.close()}
'''
            browser_environment = dict(environment, CHRONICLE_CLEANUP_UI_URL=address)
            if os.environ.get("CHRONICLE_CLEANUP_SCREENSHOT"):
                browser_environment["CHRONICLE_CLEANUP_SCREENSHOT"] = os.environ["CHRONICLE_CLEANUP_SCREENSHOT"]
            run(["node", "--input-type=module", "-e", script], cwd=project, env=browser_environment)
            print("通过：浏览器取消确认不修改设置，保存一分钟后刷新保留选择，窄屏控件无溢出", flush=True)
        else:
            request("/api/v1/update/cleanup-settings", {"retention_minutes": 1}, method="PUT")
        state = request("/api/v1/update/status")["status"]
        assert state["state"]["confirmed_at"] == confirmed_at and state["state"]["cleanup_retention_minutes"] == 1
        cleanup_at = datetime.fromisoformat(state["state"]["cleanup_after"].replace("Z", "+00:00"))
        assert cleanup_at-datetime.fromisoformat(confirmed_at.replace("Z", "+00:00")) == timedelta(minutes=1)
        stop()
        assert launch(launcher)["version"] == old_version
        persisted = request("/api/v1/update/status")["status"]["state"]
        assert persisted["confirmed_at"] == confirmed_at and persisted["cleanup_retention_minutes"] == 1
        assert datetime.fromisoformat(persisted["cleanup_after"].replace("Z", "+00:00")) == cleanup_at
        deadline = time.monotonic() + 125
        while time.monotonic() < deadline:
            state = request("/api/v1/update/status")["status"]
            cleanup = state.get("last_cleanup")
            if cleanup:
                assert datetime.fromisoformat(cleanup["completed_at"].replace("Z", "+00:00")) >= cleanup_at, "期限到达前提前清理"
                break
            if datetime.now(timezone.utc) < cleanup_at:
                assert (profile / "updates/versions" / new_version).is_dir(), "期限到达前删除了旧版本"
            time.sleep(.5)
        else:
            raise AssertionError("一分钟保留期已过，后台维护仍未留下清理记录")
        state = request("/api/v1/update/status")["status"]
        assert state["last_cleanup"]["removed_items"] >= 3 and not state["state"].get("previous_version"), state
        assert not (profile / "updates/versions" / new_version).exists(), "观察期结束仍保留无用历史版本"
        assert not (profile / "updates/versions/1.0.1").exists(), "失败版本未在恢复窗口结束后清理"
        assert Path(bad_plan["bundle_root"]).is_dir() and new_archive.is_file(), "清理影响待处理计划或外部安装包"
        retained_events = json.loads((profile / "updates/diagnostic-events.json").read_text())
        assert any(e["stage"] == "restart" and e["result"] == "failed" for e in retained_events), "安装包清理丢失失败诊断"
        assert any(e["stage"] == "cleanup_settings" and e["result"] == "succeeded" for e in retained_events), "修改清理期限没有留下诊断事件"
        assert request("/api/v1/update/cleanup-settings", {"retention_minutes": 1440}, method="PUT")["status"]["state"].get("cleanup_after") is None, "恢复默认时长重新开启了已结束的清理窗口"
        previous_login_id = json.loads((profile / "runtime/instance.json").read_text())["id"]
        stop()
        print("通过：保留时长缩短为真实一分钟并跨重启持久化，后台按期清理历史包和旧版本；关闭自动更新不影响清理，保留当前版本、待处理计划、外部包和失败诊断", flush=True)

        fixed_launcher = profile / ("desktop-launcher/codex-chronicle-desktop" + suffix)
        assert fixed_launcher.is_file(), "未创建清理范围之外的固定登录启动器"
        safe_to_cleanup = False
        run([fixed_launcher, "--launch-active", "--no-browser", "--data-dir", profile], env=environment)
        logged_in = await_restart(previous_login_id, old_version)
        assert logged_in["version"] == old_version
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            assert database.execute("PRAGMA quick_check").fetchone()[0] == "ok"
            assert database.execute("SELECT value FROM installed_update_marker").fetchone()[0] == "跨版本保留"
        stop()
        print("通过：升级、人工回滚和清理历史版本后，固定登录入口仍能验签并启动当前版本，数据记录保留", flush=True)

        legacy_ref = os.environ.get("CHRONICLE_TEST_LEGACY_REF", "")
        if legacy_ref:
            # 旧发布标签使用真实源码与临时测试签名构建；不使用正式签名私钥，也不触碰用户安装。
            legacy_source = scratch / "legacy-source"
            legacy_source.mkdir()
            legacy_archive = scratch / "legacy-source.tar"
            run(["git", "archive", "--format=tar", "--output", legacy_archive, legacy_ref], cwd=project)
            with tarfile.open(legacy_archive) as archive:
                assert all(item.isfile() or item.isdir() for item in archive.getmembers()), "旧源码含链接或特殊文件"
                archive.extractall(legacy_source, filter="data")
            legacy_version = legacy_ref.removeprefix("v")
            legacy, _, _ = package(legacy_version, "legacy-published-source", "/preview.json", source_project=legacy_source)
            profile = scratch / "legacy-profile"
            profile.mkdir()
            (profile / "launcher.json").write_text(json.dumps({"scan_root": str(source)}), encoding="utf-8")
            launcher = legacy / ("bin/codex-chronicle-desktop" + suffix)
            legacy_instance = launch(launcher)
            assert legacy_instance["version"] == legacy_version
            port = urllib.parse.urlsplit(address).port
            (profile / "startup.json").write_text(json.dumps({"port": port, "auto_fallback": True}), encoding="utf-8")
            with closing(sqlite3.connect(profile / "archive.db")) as database:
                database.execute("CREATE TABLE published_legacy_marker(value TEXT)")
                database.execute("INSERT INTO published_legacy_marker VALUES ('旧公开版本数据')")
                database.commit()
            select_release(new_version, new_archive)
            policy.update(updates_paused=True, pause_reason="隔离演练：兼容旧版的暂停")
            assert not request("/api/v1/update/check", expected=502)["available"], "旧版忽略暂停后仍发现可下载版本"
            policy.clear()
            legacy_plan = request("/api/v1/update/prepare-online", {}, 201)["plan"]
            request("/api/v1/update/activate", {"plan_id": legacy_plan["id"]})
            stop()
            updated = launch(launcher, use_saved_address=True)
            assert updated["version"] == new_version, updated
            assert urllib.parse.urlsplit(address).port == port, "旧版升级后端口设置丢失"
            state = request("/api/v1/update/status")["status"]
            assert state["state"].get("confirmed_at") and state["state"].get("cleanup_after"), state
            with closing(sqlite3.connect(profile / "archive.db")) as database:
                assert database.execute("PRAGMA quick_check").fetchone()[0] == "ok"
                assert database.execute("SELECT value FROM published_legacy_marker").fetchone()[0] == "旧公开版本数据"
            assert hashlib.sha256(sample.read_bytes()).hexdigest() == original
            stop()
            print(f"通过：从 {legacy_ref} 发布标签源码升级，原入口交接新版，保留数据库、端口与原始源文件", flush=True)

        if sys.platform == "darwin":
            old_app, new_app = scratch / "old.app", scratch / "new.app"
            for arguments, app in ((old_args, old_app), (new_args, new_app)):
                run(arguments + ["--format", "macos-app", "--macos-installer", project / "packaging/macos/install.sh", "--output", app], cwd=project)
                run(["codesign", "--force", "--sign", "-", app])
            for script in ("test-macos-install.py", "test-macos-install-interruption.py"):
                print(run([sys.executable, project / "scripts" / script, old_app, new_app], env=environment), end="", flush=True)
            print(run(["go", "test", "./internal/desktop", "-run", "^TestMacInstallInterruptedStages$", "-count=1"],
                      cwd=project, env=dict(environment, CHRONICLE_INSTALL_TEST_APP=str(new_app))), end="", flush=True)
        elif sys.platform == "win32":
            # 使用隔离测试签名生成真正的安装器，在 PR 回归阶段验证原生安装、窗口、登录及卸载行为。
            compiler = Path(os.environ["ProgramFiles(x86)"]) / "Inno Setup 6/ISCC.exe"
            installer_root = scratch / "windows-installer"
            installer_root.mkdir()
            installer_environment = dict(environment, CHRONICLE_VERSION=later_version, CHRONICLE_PAYLOAD=str(later), CHRONICLE_ARCH=arch)
            run([compiler, "/O" + str(installer_root), "/FChronicle-Isolated-Test", project / "packaging/windows/installer.iss"], cwd=project, env=installer_environment)
            installer_test = [sys.executable, project / "scripts/test-windows-installer.py", installer_root / "Chronicle-Isolated-Test.exe", later_version]
            if legacy_ref:
                legacy_environment = dict(environment, CHRONICLE_VERSION=legacy_version, CHRONICLE_PAYLOAD=str(legacy), CHRONICLE_ARCH=arch)
                run([compiler, "/O" + str(installer_root), "/FChronicle-Legacy-Test", legacy_source / "packaging/windows/installer.iss"], cwd=project, env=legacy_environment)
                installer_test += ["--legacy-installer", installer_root / "Chronicle-Legacy-Test.exe"]
            print(run(installer_test, env=environment), end="", flush=True)
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
