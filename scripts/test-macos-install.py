#!/usr/bin/env python3
# 中文说明：仅在新建隔离目录验证首次安装、重复启动、覆盖升级、候选启动失败回退及源目录不变，不接触真实 .codex。
import hashlib
from contextlib import closing
import json
import os
import pathlib
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import urllib.request


def run(arguments, *, expected=0, env=None):
    result = subprocess.run([str(arg) for arg in arguments], text=True, capture_output=True, timeout=180, env=env)
    if result.returncode != expected:
        raise AssertionError(f"命令失败：{arguments}\n{result.stdout}\n{result.stderr}")
    return result.stdout


def entry(app):
    return app / "Contents/MacOS/Codex Chronicle"


def instance(app, profile):
    return json.loads(run([entry(app), "--status", "--data-dir", profile]))


def get_json(address, path):
    with urllib.request.urlopen(address + path, timeout=10) as response:
        return json.loads(response.read())


def directory_digest(root):
    return {
        str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest()
        for path in root.rglob("*") if path.is_file()
    }


def main():
    if sys.platform != "darwin" or len(sys.argv) != 3:
        raise SystemExit("用法：python3 scripts/test-macos-install.py '/旧版本.app' '/新版本.app'（仅 macOS）")
    old_app, new_app = [pathlib.Path(value).resolve() for value in sys.argv[1:]]
    installer = pathlib.Path(__file__).resolve().parents[1] / "packaging/macos/install.sh"
    scratch = pathlib.Path(tempfile.mkdtemp(prefix="chronicle-install-smoke-"))
    target, profile, source = scratch / "installed/Codex Chronicle.app", scratch / "profile", scratch / "source"
    profile.mkdir()
    (source / "sessions").mkdir(parents=True)
    (source / "sessions/sentinel.jsonl").write_text('{"safe":"untouched"}\n', encoding="utf-8")
    config = json.dumps({"scan_root": str(source)})
    (profile / "launcher.json").write_text(config, encoding="utf-8")
    source_before = directory_digest(source)
    environment = dict(os.environ, CHRONICLE_NO_OPEN="1")
    safe_to_cleanup = False
    try:
        run(["bash", old_app / "Contents/Resources/install-macos.sh", old_app, target, profile], env=environment)
        run([entry(target), "--no-browser", "--data-dir", profile], env=environment)
        before = instance(target, profile)
        initial_state = get_json(before["url"], "/api/v1/update/status")["status"]["state"]
        assert initial_state["active_version"] == before["version"] and initial_state["installed_version"] == before["version"], "首次安装未自动建立版本基线"
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            database.execute("CREATE TABLE release_smoke_marker(value TEXT NOT NULL)")
            database.execute("INSERT INTO release_smoke_marker VALUES ('保留设置和数据')")
            database.commit()
        duplicate = json.loads(run([entry(new_app), "--no-browser", "--data-dir", profile]))
        assert duplicate["pid"] == before["pid"], "重复启动产生第二个后台"
        assert get_json(before["url"], "/healthz")["version"] == before["version"]
        print("通过：首次安装、真实进程健康检查、重复启动复用已有实例")

        run(["bash", installer, new_app, target, profile], env=environment)
        assert not (profile / "runtime/instance.json").exists(), "安装检查后台仍被安装器保留"
        run([entry(target), "--no-browser", "--data-dir", profile], env=environment)
        after = instance(target, profile)
        expected_version = run([entry(new_app), "--version"]).strip()
        assert after["version"] == expected_version and after["pid"] != before["pid"], "未切换到新版本"
        assert not after.get("startup_paused"), "安装后未开放操作"
        assert get_json(after["url"], "/api/v1/health/preflight")["status"] == "ready"
        upgraded_state = get_json(after["url"], "/api/v1/update/status")["status"]["state"]
        assert upgraded_state["active_version"] == expected_version and upgraded_state["installed_version"] == expected_version, "新应用仍被旧版本指针遮蔽"
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            assert database.execute("SELECT value FROM release_smoke_marker").fetchone()[0] == "保留设置和数据"
        assert (profile / "launcher.json").read_text(encoding="utf-8") == config
        print("通过：覆盖升级、项目数据库与工作区设置保留、候选检查后开放操作")

        # 合成故障只在本次临时应用中注入：候选已启动但仍暂停操作时修改测试设置并返回失败。
        faulty = scratch / "faulty.app"
        shutil.copytree(new_app, faulty)
        wrapper = scratch / "fault-wrapper.go"
        wrapper.write_text('''// 中文说明：仅为隔离安装回退实验制造候选启动失败。
package main
import("os"; "os/exec"; "path/filepath")
func main(){
 exe,_:=os.Executable()
 real:=filepath.Join(filepath.Dir(exe),"..","Resources","app","bin","codex-chronicle-desktop")
 cmd:=exec.Command(real,os.Args[1:]...); cmd.Stdout=os.Stdout; cmd.Stderr=os.Stderr
 if err:=cmd.Run();err!=nil{os.Exit(1)}
 fail:=false; data:=""
 for i,a:=range os.Args{if a=="--startup-check"{fail=true};if a=="--data-dir"&&i+1<len(os.Args){data=os.Args[i+1]}}
 if fail{os.WriteFile(filepath.Join(data,"launcher.json"),[]byte("故障设置"),0600);os.Exit(29)}
}
''', encoding="utf-8")
        run(["go", "build", "-o", entry(faulty), wrapper])
        run(["codesign", "--force", "--sign", "-", faulty])
        failed = subprocess.run(["bash", str(installer), str(faulty), str(target), str(profile)], text=True, capture_output=True, timeout=180, env=environment)
        assert failed.returncode != 0, "合成故障候选被错误接纳"
        assert "已恢复旧应用" in failed.stdout + failed.stderr, failed.stdout + failed.stderr
        assert not (profile / "runtime/instance.json").exists(), "失败恢复在禁止打开应用时留下后台"
        run([entry(target), "--no-browser", "--data-dir", profile], env=environment)
        restored = instance(target, profile)
        assert restored["version"] == expected_version and not restored.get("startup_paused"), "旧后台未恢复"
        assert get_json(restored["url"], "/api/v1/update/status")["status"]["state"] == upgraded_state, "安装失败未恢复原版本指针"
        assert (profile / "launcher.json").read_text(encoding="utf-8") == config, "失败候选覆盖了原设置"
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            assert database.execute("SELECT count(*) FROM release_smoke_marker").fetchone()[0] == 1
        assert directory_digest(source) == source_before, "安装或升级修改了源数据"
        assert not (profile / ".install-in-progress").exists(), "安装标记未清理"
        print("通过：候选启动失败后安全退出、旧应用与原设置回退、源目录逐文件摘要不变")
        run([entry(target), "--quit", "--data-dir", profile])
        assert not (profile / "runtime/instance.json").exists(), "退出后仍留有活动实例"
        safe_to_cleanup = True
        print("通过：退出后台、释放实例锁，完成隔离实验")
    finally:
        if not safe_to_cleanup and entry(target).exists():
            try:
                run([entry(target), "--quit", "--data-dir", profile])
                safe_to_cleanup = True
            except Exception:
                pass
        if safe_to_cleanup:
            shutil.rmtree(scratch)
        else:
            print(f"实验未完成，保留本次材料以便核对：{scratch}", file=sys.stderr)


if __name__ == "__main__":
    main()
