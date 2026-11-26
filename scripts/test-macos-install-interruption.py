#!/usr/bin/env python3
# 中文说明：在隔离应用和用户目录内强制中断安装，验证再次打开后回退未提交安装、保留已提交数据，不接触真实 .codex。
from contextlib import closing
import hashlib
import json
import os
import pathlib
import shutil
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time


def run(arguments, **kwargs):
    result = subprocess.run([str(arg) for arg in arguments], capture_output=True, text=True, timeout=120, **kwargs)
    if result.returncode:
        raise AssertionError(f"命令失败：{arguments}\n{result.stdout}\n{result.stderr}")
    return result.stdout


def entry(app):
    return app / "Contents/MacOS/Codex Chronicle"


def main():
    if sys.platform != "darwin" or len(sys.argv) != 3:
        raise SystemExit("用法：python3 scripts/test-macos-install-interruption.py '/旧版本.app' '/新版本.app'")
    old_app, new_app = [pathlib.Path(arg).resolve() for arg in sys.argv[1:]]
    installer = pathlib.Path(__file__).resolve().parents[1] / "packaging/macos/install.sh"
    scratch = pathlib.Path(tempfile.mkdtemp(prefix="chronicle-install-interruption-"))
    environment = dict(os.environ, CHRONICLE_NO_OPEN="1")
    environment.pop("CODEX_HOME", None)
    profiles = []
    running = None
    try:
        faulty = scratch / "interruptible.app"
        shutil.copytree(new_app, faulty)
        wrapper = scratch / "interrupt.go"
        wrapper.write_text('''// 中文说明：仅在隔离实验里于候选检查或开放后暂停，供父进程实施 SIGKILL。
package main
import("os"; "os/exec"; "path/filepath"; "time")
func main(){
 exe,_:=os.Executable()
 real:=filepath.Join(filepath.Dir(exe),"..","Resources","app","bin","codex-chronicle-desktop")
 cmd:=exec.Command(real,os.Args[1:]...);cmd.Stdout=os.Stdout;cmd.Stderr=os.Stderr
 if err:=cmd.Run();err!=nil{os.Exit(1)}
 phase:=os.Getenv("CHRONICLE_INSTALL_TEST_PHASE"); data:=""; pause:=false
 for i,a:=range os.Args[1:]{if a==phase&&phase!=""{pause=true};if a=="--data-dir"{data=os.Args[i+2]}}
 if pause{
  if phase=="--startup-check"{os.WriteFile(filepath.Join(data,"launcher.json"),[]byte("中断候选的未提交设置"),0600)}
  os.WriteFile(os.Getenv("CHRONICLE_INSTALL_TEST_MARKER"),[]byte("ready"),0600)
  time.Sleep(time.Hour)
 }
}
''', encoding="utf-8")
        run(["go", "build", "-o", entry(faulty), wrapper])
        run(["codesign", "--force", "--sign", "-", faulty])
        for phase in ["--startup-check", "--activate"]:
            case = scratch / phase.removeprefix("--")
            target, profile, source = case / "apps/Codex Chronicle.app", case / "profile", case / "source"
            profile.mkdir(parents=True)
            source.mkdir()
            profiles.append(profile)
            (source / "sample.jsonl").write_text('{"safe":"unchanged"}\n', encoding="utf-8")
            original = hashlib.sha256((source / "sample.jsonl").read_bytes()).hexdigest()
            config = json.dumps({"scan_root": str(source)})
            (profile / "launcher.json").write_text(config, encoding="utf-8")
            run(["bash", old_app / "Contents/Resources/install-macos.sh", old_app, target, profile], env=environment)
            with closing(sqlite3.connect(profile / "archive.db")) as db:
                db.execute("CREATE TABLE interruption_marker(value TEXT)")
                db.execute("INSERT INTO interruption_marker VALUES ('升级前记录')")
                db.commit()
            marker = case / "paused"
            fault_env = dict(environment, CHRONICLE_INSTALL_TEST_PHASE=phase, CHRONICLE_INSTALL_TEST_MARKER=str(marker))
            running = subprocess.Popen(["bash", str(installer), str(faulty), str(target), str(profile)],
                                       env=fault_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
            deadline = time.monotonic() + 60
            while not marker.exists():
                if running.poll() is not None or time.monotonic() > deadline:
                    raise AssertionError("未抵达中断边界：" + str(running.communicate(timeout=5)))
                time.sleep(.05)
            if phase == "--activate":
                with closing(sqlite3.connect(profile / "archive.db")) as db:
                    db.execute("INSERT INTO interruption_marker VALUES ('开放操作后的新记录')")
                    db.commit()
            os.killpg(running.pid, signal.SIGKILL)
            running.communicate(timeout=10)
            running = None
            recovered = json.loads(run([entry(target), "--no-browser", "--data-dir", profile]))
            assert not recovered.get("startup_paused"), "恢复后仍处于安装暂停状态"
            assert (profile / "launcher.json").read_text(encoding="utf-8") == config, "设置未恢复"
            with closing(sqlite3.connect(profile / "archive.db")) as db:
                assert db.execute("PRAGMA quick_check").fetchone()[0] == "ok"
                values = db.execute("SELECT value FROM interruption_marker ORDER BY rowid").fetchall()
                assert len(values) == (2 if phase == "--activate" else 1), values
            expected_app = faulty if phase == "--activate" else old_app
            assert entry(target).read_bytes() == entry(expected_app).read_bytes(), "安装恢复选择了错误版本"
            expected_version = run([entry(expected_app), "--version"]).strip()
            state = json.loads((profile / "updates/install-state.json").read_text())["active_version"]
            assert state == expected_version and recovered["version"] == expected_version, "安装恢复未同步恢复版本指针"
            assert hashlib.sha256((source / "sample.jsonl").read_bytes()).hexdigest() == original
            assert not (profile / ".install-in-progress").exists(), "恢复后仍有安装标记"
            assert not list(target.parent.glob(".chronicle-install-*")), "中断的工作目录未清理"
            duplicate = json.loads(run([entry(target), "--no-browser", "--data-dir", profile]))
            assert duplicate["id"] == recovered["id"], "重复恢复产生额外后台"
            run([entry(target), "--quit", "--data-dir", profile])
            print("通过：" + ("候选检查后强制中断，再次打开自动还原旧应用、原设置和数据库" if phase == "--startup-check" else "开放操作后强制中断，保留新应用和开放后的已提交数据"), flush=True)
    finally:
        if running and running.poll() is None:
            os.killpg(running.pid, signal.SIGKILL)
            running.communicate(timeout=10)
        safe = True
        for profile in profiles:
            try:
                run([entry(new_app), "--quit", "--data-dir", profile])
            except Exception:
                safe = False
        if safe:
            shutil.rmtree(scratch)
        else:
            print("保留未退出的隔离实验材料：" + str(scratch), file=sys.stderr)


if __name__ == "__main__":
    main()
