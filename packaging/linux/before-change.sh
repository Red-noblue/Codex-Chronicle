#!/bin/sh
# 中文说明：包管理器更新或卸载前拒绝替换仍被使用的程序；不跨用户强杀后台或数据任务。
set -eu
for chronicle_process in /proc/[0-9]*/exe; do
  chronicle_executable=$(readlink "$chronicle_process" 2>/dev/null || true)
  case "$chronicle_executable" in
    /usr/lib/codex-chronicle/bin/codex-chronicle*|*/updates/versions/*/bin/codex-chronicle|*/updates/versions/*/bin/codex-chronicle-desktop)
      echo 'Codex Chronicle 后台仍在运行。请各用户等待任务完成后执行 /usr/lib/codex-chronicle/bin/codex-chronicle-desktop --quit，再重试安装或卸载。' >&2
      exit 1 ;;
  esac
done
