#!/usr/bin/env python3
# 中文说明：在 Xvfb 隔离桌面通过 AT-SPI 操作原生端口输入框和保存按钮，不启动后台，不改动真实用户设置。
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

import pyatspi
from gi.repository import GLib


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--launcher', type=Path, required=True)
    args = parser.parse_args()
    launcher = args.launcher.resolve()
    with tempfile.TemporaryDirectory(prefix='chronicle-linux-ui-') as scratch:
        root = Path(scratch)
        data = root / 'profile'
        environment = dict(os.environ, HOME=str(root), XDG_CONFIG_HOME=str(root / '.config'), GDK_BACKEND='x11', GTK_A11Y='atspi')
        process = subprocess.Popen([str(launcher), '--startup-settings', '--data-dir', str(data)], env=environment)
        try:
            entry = save = None
            for _ in range(100):
                if process.poll() is not None:
                    raise AssertionError('端口设置窗口提前退出')
                while GLib.MainContext.default().pending():
                    GLib.MainContext.default().iteration(False)
                desktop = pyatspi.Registry.getDesktop(0)
                entry = pyatspi.findDescendant(desktop, lambda node: 'EditableText' in node.get_interfaces())
                save = pyatspi.findDescendant(desktop, lambda node: node.getRole() == pyatspi.ROLE_PUSH_BUTTON and node.name == '保存')
                if entry is not None and save is not None:
                    break
                time.sleep(0.1)
            assert entry is not None and save is not None, '没有找到原生端口输入框和保存按钮'
            assert entry.queryEditableText().setTextContents('9321'), '无法输入端口'
            assert entry.queryText().getText(0, -1) == '9321', '端口输入未生效'
            assert save.queryAction().doAction(0), '无法点击保存按钮'
            assert process.wait(timeout=15) == 0
            settings = json.loads((data / 'startup.json').read_text(encoding='utf-8'))
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
