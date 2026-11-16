#!/usr/bin/env bash
# 中文说明：从 DMG 双击此入口，使用应用内经过签名封装的安装脚本安装到当前用户 Applications。
set -euo pipefail
chronicle_here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo '将安装到当前用户的 Applications 文件夹；已有后台会等待任务完成后退出。'
echo '用户设置、凭证和归档会保留。'
read -r -p '继续安装？输入 y 后回车：' chronicle_answer
if [[ "$chronicle_answer" != y && "$chronicle_answer" != Y ]]; then exit 0; fi
bash "$chronicle_here/Codex Chronicle.app/Contents/Resources/install-macos.sh" "$chronicle_here/Codex Chronicle.app"
