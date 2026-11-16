#!/usr/bin/env bash
# 中文说明：委托签名应用内的启动器执行可恢复安装；强制中断后再次安装或打开应用可恢复事务。
set -euo pipefail

chronicle_source="${1:?用法：install.sh '/来源/Codex Chronicle.app' ['/目标/Codex Chronicle.app'] ['用户数据目录']}"
chronicle_target="${2:-$HOME/Applications/Codex Chronicle.app}"
chronicle_data="${3:-$HOME/Library/Application Support/Codex Chronicle}"
[[ "$chronicle_source" == /*.app && ! -L "$chronicle_source" ]] || { echo '应用来源必须为普通 .app 绝对路径' >&2; exit 1; }
/usr/bin/codesign --verify --deep --strict "$chronicle_source"
"$chronicle_source/Contents/MacOS/Codex Chronicle" --no-browser --install-app "$chronicle_source" --install-target "$chronicle_target" --data-dir "$chronicle_data"
if [[ "${CHRONICLE_NO_OPEN:-0}" != 1 ]]; then
  /usr/bin/open "$chronicle_target" --args --data-dir "$chronicle_data"
fi
