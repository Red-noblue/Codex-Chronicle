#!/usr/bin/env python3
# 中文说明：在 Xvfb 隔离桌面实际操作 Linux 原生端口设置窗口，不启动后台，不改动真实用户设置。
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--launcher', type=Path, required=True)
    args = parser.parse_args()
    launcher = args.launcher.resolve()
    with tempfile.TemporaryDirectory(prefix='chronicle-linux-ui-') as scratch:
        root = Path(scratch)
        data = root / 'profile'
        environment = dict(os.environ, HOME=str(root), XDG_CONFIG_HOME=str(root / '.config'), GDK_BACKEND='x11')
        process = subprocess.Popen([str(launcher), '--startup-settings', '--data-dir', str(data)], env=environment)
        try:
            window = None
            for _ in range(100):
                if process.poll() is not None:
                    raise AssertionError('端口设置窗口提前退出')
                found = subprocess.run(['xdotool', 'search', '--onlyvisible', '--name', '端口设置.*Codex Chronicle'], capture_output=True, text=True)
                if found.returncode == 0:
                    window = found.stdout.splitlines()[0]
                    break
                time.sleep(0.1)
            assert window, '没有找到原生端口窗口'
            subprocess.run(['xdotool', 'windowfocus', '--sync', window], check=True)
            subprocess.run(['xdotool', 'type', '--clearmodifiers', '9321'], check=True)
            subprocess.run(['xdotool', 'key', '--clearmodifiers', 'Return'], check=True)
            assert process.wait(timeout=15) == 0
            settings = json.loads((data / 'startup.json').read_text())
            assert settings == {'port': 9321, 'auto_fallback': True}, settings
            assert not (data / 'runtime' / 'instance.json').exists(), '设置窗口不应启动服务'
            assert not (root / '.config' / 'autostart').exists(), '端口设置不应启用自启动'
            print('通过：Linux 原生端口窗口真实输入、保存及默认自动回退；无需后台或附加端口')
        finally:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=10)


if __name__ == '__main__':
    main()
