#!/usr/bin/env python3
# 中文说明：在 Windows 原生运行器验证每用户安装器、真实设置窗口、托盘窗口、固定登录命令与卸载保留数据；拒绝覆盖已有安装。
import argparse
from contextlib import closing
import ctypes
from ctypes import wintypes
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def main():
    if sys.platform != "win32":
        raise SystemExit("此验收必须在 Windows 原生运行器执行")
    import winreg
    parser = argparse.ArgumentParser()
    parser.add_argument("installer", type=Path)
    parser.add_argument("version")
    args = parser.parse_args()
    installer = args.installer.resolve(strict=True)
    uninstall_key = r"Software\Microsoft\Windows\CurrentVersion\Uninstall\{5FA52307-E1AC-4396-8C46-3F2D56214CF4}_is1"
    login_key = r"Software\Microsoft\Windows\CurrentVersion\Run"

    def registry_value(key, name):
        try:
            with winreg.OpenKey(winreg.HKEY_CURRENT_USER, key, access=winreg.KEY_READ | winreg.KEY_WOW64_64KEY) as handle:
                return winreg.QueryValueEx(handle, name)[0]
        except FileNotFoundError:
            return None

    try:
        with winreg.OpenKey(winreg.HKEY_CURRENT_USER, uninstall_key, access=winreg.KEY_READ | winreg.KEY_WOW64_64KEY):
            raise AssertionError("已有本应用安装，拒绝覆盖")
    except FileNotFoundError:
        pass
    assert registry_value(login_key, "CodexChronicle") is None, "已有本应用登录项，拒绝修改"
    scratch = Path(tempfile.mkdtemp(prefix="chronicle-windows-installer-")).resolve()
    application = scratch / "应用程序 with space"
    local_data = scratch / "隔离数据 with space"
    profile = local_data / "Codex Chronicle"
    source = scratch / "source"
    source.mkdir()
    sentinel = source / "sentinel.jsonl"
    sentinel.write_text('{"safe":"安装不修改对话源"}\n', encoding="utf-8")
    original = hashlib.sha256(sentinel.read_bytes()).hexdigest()
    environment = dict(os.environ, LOCALAPPDATA=str(local_data))
    launcher = application / "bin/codex-chronicle-desktop.exe"
    uninstaller = application / "unins000.exe"
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    gui = settings = None
    installed = False
    safe = True
    reservation = socket.socket()
    reservation.bind(("127.0.0.1", 0))
    reservation.listen()
    selected_port = reservation.getsockname()[1]

    def run(arguments, timeout=180):
        result = subprocess.run([str(arg) for arg in arguments], env=environment, capture_output=True, text=True, timeout=timeout)
        assert result.returncode == 0, (arguments, result.returncode, result.stderr[-2000:])
        return result.stdout

    def status(instance):
        with opener.open(instance["url"] + "/healthz", timeout=5) as response:
            assert json.load(response)["version"] == args.version
        with opener.open(instance["url"] + "/api/v1/update/status", timeout=5) as response:
            data = json.load(response)
        assert data["running_version"] == args.version and not data["status"]["recovery_required"]
        return data["status"]

    def wait_instance(previous=""):
        for _ in range(150):
            try:
                instance = json.loads((profile / "runtime/instance.json").read_text())
                if instance["id"] != previous:
                    status(instance)
                    return instance
            except (OSError, ValueError, KeyError, urllib.error.URLError):
                pass
            time.sleep(.2)
        raise AssertionError("后台没有按期健康启动")

    user32 = ctypes.WinDLL("user32", use_last_error=True)
    user32.FindWindowW.argtypes = [wintypes.LPCWSTR, wintypes.LPCWSTR]
    user32.FindWindowW.restype = wintypes.HWND
    user32.GetWindowThreadProcessId.argtypes = [wintypes.HWND, ctypes.POINTER(wintypes.DWORD)]
    user32.GetDlgItem.argtypes = [wintypes.HWND, ctypes.c_int]
    user32.GetDlgItem.restype = wintypes.HWND
    user32.SetWindowTextW.argtypes = [wintypes.HWND, wintypes.LPCWSTR]
    user32.SetWindowTextW.restype = wintypes.BOOL
    user32.SendMessageW.argtypes = [wintypes.HWND, wintypes.UINT, wintypes.WPARAM, wintypes.LPARAM]
    user32.SendMessageW.restype = ctypes.c_ssize_t

    def window(title, process=None):
        for _ in range(100):
            handle = user32.FindWindowW("CodexChronicleTrayWindow", title)
            if handle:
                owner = wintypes.DWORD()
                user32.GetWindowThreadProcessId(handle, ctypes.byref(owner))
                if process is None or owner.value == process.pid:
                    return handle
            if process is not None:
                assert process.poll() is None, "原生窗口进程提前退出"
            time.sleep(.1)
        raise AssertionError("没有找到原生窗口：" + title)

    try:
        run([installer, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-", "/DIR=" + str(application), "/GROUP=Codex Chronicle 隔离验收", "/LOG=" + str(scratch / "install.log")])
        installed = True
        manifest = json.loads((application / "codex-chronicle-release.json").read_text())
        assert manifest["version"] == args.version and manifest["os"] == "windows" and manifest["signature"]
        run([launcher, "--no-browser", "--data-dir", profile, "--scan-root", source, "--address", "127.0.0.1:0"])
        safe = False
        first = wait_instance()
        assert status(first)["signature_required"]
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            database.execute("CREATE TABLE installer_marker(value TEXT)")
            database.execute("INSERT INTO installer_marker VALUES ('安装卸载保留')")
            database.commit()
        print("通过：Windows 每用户安装器实际安装并启动签名载荷，路径含中文和空格", flush=True)

        settings = subprocess.Popen([str(launcher), "--startup-settings", "--data-dir", str(profile)], env=environment)
        dialog = window("端口设置 · Codex Chronicle", settings)
        editor, save = user32.GetDlgItem(dialog, 201), user32.GetDlgItem(dialog, 1)
        assert editor and save and user32.SetWindowTextW(editor, str(selected_port))
        user32.SendMessageW(save, 0x00F5, 0, 0)  # BM_CLICK：使用原生按钮消息执行实际保存。
        assert settings.wait(timeout=15) == 0
        saved = json.loads((profile / "startup.json").read_text())
        assert saved == {"port": selected_port, "auto_fallback": True}
        assert json.loads((profile / "runtime/instance.json").read_text())["id"] == first["id"]
        gui = subprocess.Popen([str(launcher), "--background", "--data-dir", str(profile)], env=environment)
        window("Codex Chronicle", gui)
        run([launcher, "--quit", "--data-dir", profile])
        assert gui.wait(timeout=20) == 0
        safe = True
        print("通过：Windows 原生端口窗口实际输入和保存，托盘窗口创建并随安全退出关闭", flush=True)

        fixed = profile / "desktop-launcher/codex-chronicle-desktop.exe"
        assert fixed.is_file()
        login = subprocess.list2cmdline([str(fixed), "--background", "--data-dir", str(profile), "--launch-active"])
        with winreg.CreateKeyEx(winreg.HKEY_CURRENT_USER, login_key, access=winreg.KEY_SET_VALUE | winreg.KEY_WOW64_64KEY) as key:
            winreg.SetValueEx(key, "CodexChronicle", 0, winreg.REG_SZ, login)
        safe = False
        result = subprocess.run(login + " --no-browser", env=environment, capture_output=True, text=True, timeout=30)
        assert result.returncode == 0, result.stderr
        restarted = wait_instance(first["id"])
        assert urllib.parse.urlsplit(restarted["url"]).port != selected_port, "端口占用后未自动回退"
        assert registry_value(login_key, "CodexChronicle") == login
        print("通过：Windows 当前用户登录命令经固定入口重新启动，端口冲突时自动回退", flush=True)

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
        kernel32.OpenProcess.restype = wintypes.HANDLE
        kernel32.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
        kernel32.CloseHandle.argtypes = [wintypes.HANDLE]
        backend = kernel32.OpenProcess(0x00100000, False, restarted["pid"])
        assert backend, "无法核对本轮后台的退出状态"
        try:
            run([uninstaller, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/LOG=" + str(scratch / "uninstall.log")])
            assert kernel32.WaitForSingleObject(backend, 20000) == 0, "卸载后后台进程未退出"
        finally:
            kernel32.CloseHandle(backend)
        installed = False
        for _ in range(100):
            if not (profile / "runtime/instance.json").exists():
                break
            time.sleep(.1)
        assert not launcher.exists() and not (profile / "runtime/instance.json").exists(), "卸载未安全退出后台或仍保留安装入口"
        assert registry_value(login_key, "CodexChronicle") is None, "卸载后仍留有登录启动项"
        with closing(sqlite3.connect(profile / "archive.db")) as database:
            assert database.execute("PRAGMA quick_check").fetchone()[0] == "ok"
            assert database.execute("SELECT value FROM installer_marker").fetchone()[0] == "安装卸载保留"
        run([fixed, "--launch-active", "--no-browser", "--data-dir", profile])
        assert not (profile / "runtime/instance.json").exists(), "卸载后固定入口从保留缓存重新启动了后台"
        assert hashlib.sha256(sentinel.read_bytes()).hexdigest() == original
        safe = True
        print("通过：Windows 实际卸载等待后台退出，清除登录项，保留健康数据库和原始源文件，固定入口不从缓存重启", flush=True)
    finally:
        reservation.close()
        if installed and uninstaller.exists():
            try:
                run([uninstaller, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART"], timeout=150)
                safe = not (profile / "runtime/instance.json").exists()
            except Exception:
                safe = False
        if safe:
            shutil.rmtree(scratch)
        else:
            print("保留尚未退出或卸载的 Windows 隔离验收目录：" + str(scratch), file=sys.stderr)


if __name__ == "__main__":
    main()
