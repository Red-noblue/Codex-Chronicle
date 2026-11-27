#!/usr/bin/env python3
# 中文说明：只读挂载 DMG，验证拖放入口、镜像内启动阻止及复制后的空白资料启动，不触碰现有应用。
import json
import os
from pathlib import Path
import plistlib
import shutil
import subprocess
import sys
import tempfile


def run(arguments, **options):
    result = subprocess.run([str(arg) for arg in arguments], capture_output=True, text=True, timeout=120, **options)
    if result.returncode:
        raise AssertionError(f"命令失败：{arguments}\n{result.stdout}\n{result.stderr}")
    return result.stdout


def main():
    if sys.platform != "darwin" or len(sys.argv) not in (3, 4) or len(sys.argv) == 4 and sys.argv[3] != "--require-installed-entry":
        raise SystemExit("用法：python3 scripts/test-macos-dmg.py '/发布包.dmg' '预期版本' [--require-installed-entry]")
    dmg, version = Path(sys.argv[1]).resolve(), sys.argv[2]
    scratch = Path(tempfile.mkdtemp(prefix="chronicle-dmg-drag-")).resolve()
    mount, source = scratch / "volume", scratch / "empty-source"
    mount.mkdir()
    source.mkdir()
    profiles = [scratch / "blocked-profile", scratch / "profile"]
    target = scratch / "Applications/Codex Chronicle.app"
    entry = mount / "Codex Chronicle.app/Contents/MacOS/Codex Chronicle"
    mounted, safe = False, True
    environment = dict(os.environ)
    environment.pop("CODEX_HOME", None)
    try:
        attached = run(["hdiutil", "attach", "-readonly", "-nobrowse", "-plist", "-mountpoint", mount, dmg])
        mounted = True
        info = plistlib.loads(attached.encode())
        assert any(Path(item["mount-point"]).resolve() == mount for item in info["system-entities"] if item.get("mount-point"))
        visible = {path.name for path in mount.iterdir() if not path.name.startswith(".")}
        assert visible == {"Codex Chronicle.app", "应用程序"}, f"DMG 拖放入口不完整或仍有命令脚本：{visible}"
        applications = mount / "应用程序"
        assert applications.is_symlink() and os.readlink(applications) == "/Applications", "拖放目标不是系统应用程序文件夹"
        run(["codesign", "--verify", "--deep", "--strict", mount / "Codex Chronicle.app"])
        assert run([entry, "--version"]).strip() == version

        blocked = subprocess.run([str(entry), "--no-browser", "--data-dir", str(profiles[0]), "--scan-root", str(source)], env=environment, capture_output=True, text=True, timeout=60)
        assert blocked.returncode != 0 and "应用程序" in blocked.stdout + blocked.stderr, "直接从镜像启动未提示先安装"
        assert not profiles[0].exists(), "从镜像启动创建了用户资料或后台"

        target.parent.mkdir()
        run(["ditto", mount / "Codex Chronicle.app", target])
        run(["codesign", "--verify", "--deep", "--strict", target])
        copied_entry = target / "Contents/MacOS/Codex Chronicle"
        instance = json.loads(run([copied_entry, "--no-browser", "--data-dir", profiles[1], "--scan-root", source], env=environment))
        assert instance["version"] == version and not instance.get("startup_paused")
        trusted = target / "Contents/Resources/app/update/trusted-public-key"
        if os.environ.get("CHRONICLE_SIGNING_PRIVATE_KEY_FILE"):
            assert trusted.is_file(), "已签名发布包缺少更新信任根"
        if trusted.is_file():
            state = json.loads((profiles[1] / "updates/install-state.json").read_text())
            assert state["active_version"] == version and state["installed_version"] == version, "复制后首次启动没有登记正确版本"
        duplicate = json.loads(run([copied_entry, "--no-browser", "--data-dir", profiles[1]], env=environment))
        assert duplicate["id"] == instance["id"], "重复打开产生第二个后台"
        origin_path = profiles[1] / "desktop-launcher/installed-entry.json"
        if len(sys.argv) == 4:
            assert origin_path.is_file(), "已安装应用没有登记固定登录入口的安装位置"
        if origin_path.is_file():
            origin = json.loads(origin_path.read_text())
            assert Path(origin["package_root"]).resolve() == (target / "Contents/Resources/app").resolve()
            assert origin["installed_version"] == version
        run([copied_entry, "--quit", "--data-dir", profiles[1]], env=environment)
        if origin_path.is_file():
            fixed = profiles[1] / "desktop-launcher/codex-chronicle-desktop"
            assert fixed.is_file()
            shutil.rmtree(target)
            run([fixed, "--launch-active", "--no-browser", "--data-dir", profiles[1]], env=environment)
            assert not (profiles[1] / "runtime/instance.json").exists(), "应用移除后固定入口从保留缓存重新启动了后台"
            print("通过：已安装应用登记真实位置，移除应用后固定登录入口安静退出，不从保留缓存重启", flush=True)
        assert not list(source.iterdir()), "拖放启动修改了扫描源"
        print("通过：DMG 仅含应用与应用程序入口；镜像内启动被阻止且未建资料；复制后首次启动及单实例核验通过", flush=True)
    finally:
        if mounted:
            cleanup_entry = target / "Contents/MacOS/Codex Chronicle" if target.exists() else entry
            for profile in profiles:
                if not profile.exists():
                    continue
                try:
                    run([cleanup_entry, "--quit", "--data-dir", profile], env=environment)
                except Exception:
                    safe = False
            try:
                run(["hdiutil", "detach", mount])
                mounted = False
            except Exception:
                safe = False
        if safe and not mounted:
            shutil.rmtree(scratch)
        else:
            print(f"隔离实验尚未释放，保留本轮材料：{scratch}", file=sys.stderr)


if __name__ == "__main__":
    main()
