#!/usr/bin/env python3
# 中文说明：只用临时复制的休眠程序验证 Linux 安装卸载保护，覆盖更新目录中的后台和启动器，绝不强杀其他进程。
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time


def main():
    if sys.platform != "linux":
        raise SystemExit("此验收必须在 Linux 执行")
    guard = Path(sys.argv[1]).resolve() if len(sys.argv) > 1 else Path(__file__).resolve().parents[1] / "packaging/linux/before-change.sh"
    sleep = Path(shutil.which("sleep")).resolve()
    with tempfile.TemporaryDirectory(prefix="chronicle-package-lifecycle-") as scratch:
        root = Path(scratch) / "profile/updates/versions/0.1.0-preview.20/bin"
        root.mkdir(parents=True)
        for name, expected in (("unrelated-program", 0), ("codex-chronicle", 1), ("codex-chronicle-desktop", 1)):
            executable = root / name
            shutil.copy2(sleep, executable)
            # 兼容将 sleep 实现为多调用核心工具的发行版，同时保持 /proc 中的真实可执行路径。
            process = subprocess.Popen(["sleep", "60"], executable=str(executable))
            try:
                time.sleep(.1)
                assert process.poll() is None, "休眠样本未正常启动"
                result = subprocess.run(["sh", str(guard)], capture_output=True, text=True, timeout=15)
                assert result.returncode == expected, (name, result.returncode, result.stderr)
                assert process.poll() is None, "安装卸载保护强杀了后台"
                if expected:
                    assert "仍在运行" in result.stderr and "--quit" in result.stderr
            finally:
                process.terminate()
                process.wait(timeout=10)
        result = subprocess.run(["sh", str(guard)], capture_output=True, text=True, timeout=15)
        assert result.returncode == 0, result.stderr
    print("通过：Linux 包管理器阻止替换仍运行的更新版后台和启动器，放行无关程序及已退出应用，不跨用户强杀")


if __name__ == "__main__":
    main()
