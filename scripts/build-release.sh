#!/usr/bin/env bash
# 中文说明：构建不依赖源码工作目录的发布载荷；macOS 原生生成 .app、ZIP 和 DMG，不安装或改动真实 .codex。
set -euo pipefail

chronicle_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chronicle_version="${1:?用法：scripts/build-release.sh 0.1.0-preview.1 [darwin|windows|linux] [arm64|amd64]}"
chronicle_os="${2:-$(go env GOOS)}"
chronicle_arch="${3:-$(go env GOARCH)}"
chronicle_default_feed_url="$(python3 "$chronicle_root/scripts/release-feed.py" feed-url "$chronicle_version" --repository "${GITHUB_REPOSITORY:-Red-noblue/Codex-Chronicle}")"
chronicle_update_feed_url="${CHRONICLE_UPDATE_FEED_URL:-$chronicle_default_feed_url}"
chronicle_build_id="${CHRONICLE_BUILD_ID:-}"
case "$chronicle_os/$chronicle_arch" in
  darwin/arm64|darwin/amd64|windows/amd64|windows/arm64|linux/amd64|linux/arm64) ;;
  *) echo '当前发布脚本尚未支持此系统和架构' >&2; exit 1 ;;
esac
if [[ "$chronicle_os" == darwin && "$(go env GOOS)" != darwin ]]; then
  echo 'macOS 安装包需要在 macOS 上原生构建与签名' >&2
  exit 1
fi
chronicle_output="$chronicle_root/dist/$chronicle_version/$chronicle_os-$chronicle_arch"
if [[ -e "$chronicle_output" ]]; then
  echo "输出已存在，拒绝覆盖：$chronicle_output" >&2
  exit 1
fi
mkdir -p "$chronicle_output"
chronicle_temp="$(mktemp -d "$chronicle_output/.build-XXXXXX")"
trap 'rm -rf "$chronicle_temp"' EXIT
cd "$chronicle_root"
if [[ "${CHRONICLE_SKIP_WEB_BUILD:-0}" != 1 ]]; then
  (cd web && npm run check && npm run build)
fi
[[ -f web/dist/index.html ]] || { echo '缺少 web/dist/index.html，请先构建前端' >&2; exit 1; }
chronicle_suffix=''
chronicle_cgo=0
if [[ "$chronicle_os" == windows ]]; then chronicle_suffix='.exe'; fi
if [[ "$chronicle_os" == darwin ]]; then chronicle_cgo=1; fi
for chronicle_command in codex-chronicle codex-chronicle-desktop chronicle-native-trash; do
  chronicle_ldflags="-s -w -X main.version=$chronicle_version"
  if [[ "$chronicle_command" == codex-chronicle-desktop ]]; then
    chronicle_ldflags="$chronicle_ldflags -X main.updateFeedURL=$chronicle_update_feed_url"
  fi
  if [[ "$chronicle_os" == windows && "$chronicle_command" == codex-chronicle-desktop ]]; then
    chronicle_ldflags="$chronicle_ldflags -H=windowsgui"
  fi
  env GOOS="$chronicle_os" GOARCH="$chronicle_arch" CGO_ENABLED="$chronicle_cgo" \
    go build -trimpath -ldflags "$chronicle_ldflags" \
    -o "$chronicle_temp/$chronicle_command$chronicle_suffix" "./cmd/$chronicle_command"
done

# 先签可执行文件，再计算发布清单，保证清单包含最终字节的摘要。
if [[ "$chronicle_os" == darwin ]]; then
  for chronicle_binary in "$chronicle_temp"/*; do
    if [[ -n "${CHRONICLE_SIGN_IDENTITY:-}" ]]; then
      codesign --force --options runtime --timestamp --sign "$CHRONICLE_SIGN_IDENTITY" "$chronicle_binary"
    else
      codesign --force --sign - "$chronicle_binary"
    fi
  done
fi
chronicle_release_args=(--version "$chronicle_version" --os "$chronicle_os" --arch "$chronicle_arch"
  --daemon "$chronicle_temp/codex-chronicle$chronicle_suffix"
  --launcher "$chronicle_temp/codex-chronicle-desktop$chronicle_suffix"
  --trash-bridge "$chronicle_temp/chronicle-native-trash$chronicle_suffix" --web-dist web/dist
  --license-file LICENSE --third-party-notices-file THIRD_PARTY_NOTICES.txt)
if [[ -n "$chronicle_build_id" ]]; then chronicle_release_args+=(--build-id "$chronicle_build_id"); fi
if [[ -n "${CHRONICLE_SIGNING_PRIVATE_KEY_FILE:-}" ]]; then
  chronicle_release_args+=(--signing-private-key-file "$CHRONICLE_SIGNING_PRIVATE_KEY_FILE" --signing-key-id "${CHRONICLE_SIGNING_KEY_ID:-release}")
fi
go run ./cmd/codex-chronicle-release "${chronicle_release_args[@]}" --format directory --output "$chronicle_output/payload"
go run ./cmd/codex-chronicle-release "${chronicle_release_args[@]}" --format zip \
  --output "$chronicle_output/Codex-Chronicle-$chronicle_version-$chronicle_os-$chronicle_arch.zip"
if [[ -n "${CHRONICLE_SIGNING_PRIVATE_KEY_FILE:-}" && -n "$chronicle_build_id" && "$chronicle_os" == "$(go env GOOS)" && "$chronicle_arch" == "$(go env GOARCH)" ]]; then
  go run ./cmd/codex-chronicle-release verify-archive \
    --archive "$chronicle_output/Codex-Chronicle-$chronicle_version-$chronicle_os-$chronicle_arch.zip" \
    --trusted-public-key-file packaging/update-signing.pub --version "$chronicle_version" --build-id "$chronicle_build_id" --startup-check > "$chronicle_temp/final-archive.json"
fi
if [[ "$chronicle_os" == linux ]]; then
  tar -czf "$chronicle_output/Codex-Chronicle-$chronicle_version-linux-$chronicle_arch.tar.gz" -C "$chronicle_output/payload" .
fi
if [[ "$chronicle_os" == "$(go env GOOS)" && "$chronicle_arch" == "$(go env GOARCH)" ]]; then
  "$chronicle_output/payload/bin/codex-chronicle-desktop$chronicle_suffix" --verify
fi
if [[ "$chronicle_os" == darwin ]]; then
  chronicle_app="$chronicle_output/Codex Chronicle.app"
  go run ./cmd/codex-chronicle-release "${chronicle_release_args[@]}" --format macos-app --macos-installer packaging/macos/install.sh --output "$chronicle_app"
  if [[ -n "${CHRONICLE_SIGN_IDENTITY:-}" ]]; then
    codesign --force --options runtime --timestamp --sign "$CHRONICLE_SIGN_IDENTITY" "$chronicle_app"
  else
    codesign --force --sign - "$chronicle_app"
  fi
  codesign --verify --deep --strict "$chronicle_app"
  plutil -lint "$chronicle_app/Contents/Info.plist"
  if [[ "$chronicle_arch" == "$(go env GOARCH)" ]]; then
    "$chronicle_app/Contents/MacOS/Codex Chronicle" --verify
  fi
  mkdir "$chronicle_temp/dmg"
  ditto "$chronicle_app" "$chronicle_temp/dmg/Codex Chronicle.app"
  cp packaging/macos/安装或更新.command "$chronicle_temp/dmg/安装或更新.command"
  chmod +x "$chronicle_temp/dmg/安装或更新.command"
  chronicle_dmg="$chronicle_output/Codex-Chronicle-$chronicle_version-macOS-$chronicle_arch.dmg"
  hdiutil create -quiet -volname 'Codex Chronicle' -srcfolder "$chronicle_temp/dmg" -format UDZO "$chronicle_dmg"
  if [[ -n "${CHRONICLE_NOTARY_PROFILE:-}" ]]; then
    [[ -n "${CHRONICLE_SIGN_IDENTITY:-}" ]] || { echo '公证必须先配置 Developer ID 签名' >&2; exit 1; }
    xcrun notarytool submit "$chronicle_dmg" --keychain-profile "$CHRONICLE_NOTARY_PROFILE" --wait
    xcrun stapler staple "$chronicle_dmg"
  fi
  hdiutil verify -quiet "$chronicle_dmg"
fi
(
  cd "$chronicle_output"
  chronicle_packages=()
  for chronicle_artifact in *.zip *.tar.gz *.dmg; do
    if [[ -f "$chronicle_artifact" ]]; then chronicle_packages+=("$chronicle_artifact"); fi
  done
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${chronicle_packages[@]}" > SHA256SUMS
  else
    shasum -a 256 "${chronicle_packages[@]}" > SHA256SUMS
  fi
)
echo "发布构建完成：$chronicle_output"
