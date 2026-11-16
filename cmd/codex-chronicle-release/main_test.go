// 中文说明：验证离线发布命令的目标平台清单、ZIP 解压内容、覆盖保护和符号链接拒绝。
package main

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"codex-chronicle.local/internal/desktop"
	updatepkg "codex-chronicle.local/internal/update"
)

func TestBuildReleaseArchiveUsesExplicitTargetPlatform(t *testing.T) {
	base := t.TempDir()
	daemon := filepath.Join(base, "daemon")
	if err := os.WriteFile(daemon, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(base, "web-dist")
	if err := os.MkdirAll(filepath.Join(web, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "assets", "app.js"), []byte("console.log('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(base, "release.zip")
	license := filepath.Join(base, "LICENSE")
	if err := os.WriteFile(license, []byte("license fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := releaseOptions{Version: "1.2.3", BuildID: "test-build", Daemon: daemon, WebDist: web, Output: output, TargetOS: "linux", TargetArch: "armv7", LicenseFile: license, ThirdPartyNoticesFile: license}
	if err := buildReleaseArchive(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if _, err := zip.OpenReader(output); err != nil {
		t.Fatalf("输出不是有效 ZIP：%v", err)
	}

	extracted := filepath.Join(base, "extracted")
	if err := updatepkg.ExtractArchive(context.Background(), output, extracted, updatepkg.DefaultExtractionLimits); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(extracted, updatepkg.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest updatepkg.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "1.2.3" || manifest.OS != "linux" || manifest.Arch != "armv7" || manifest.BuildID != "test-build" {
		t.Fatalf("manifest 平台或版本错误：%+v", manifest)
	}
	paths := make(map[string]bool, len(manifest.Files))
	for _, file := range manifest.Files {
		paths[file.Path] = true
	}
	for _, path := range []string{"bin/codex-chronicle", "web/index.html", "web/assets/app.js", "licenses/LICENSE", "licenses/THIRD_PARTY_NOTICES.txt"} {
		if !paths[path] {
			t.Fatalf("manifest 缺少文件：%s", path)
		}
	}
	if err := buildReleaseArchive(context.Background(), options); err == nil || !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatalf("已有输出文件未被拒绝：%v", err)
	}
}

func TestMacAppCarriesLauncherBridgeAndRelativePayload(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "binary")
	if err := os.WriteFile(bin, []byte("test executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(root, "web")
	if err := os.Mkdir(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(root, "install.sh")
	if err := os.WriteFile(installer, []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "Codex Chronicle.app")
	options := releaseOptions{Version: "0.1.0-preview.2", Daemon: bin, Launcher: bin, TrashBridge: bin, WebDist: web,
		TargetOS: "darwin", TargetArch: "arm64", Format: "macos-app", MacInstaller: installer, Output: output}
	if err := buildReleaseArchive(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	payload := desktop.PackageRoot(filepath.Join(output, "Contents", "MacOS", "Codex Chronicle"))
	for _, relative := range []string{"bin/codex-chronicle", "bin/codex-chronicle-desktop", "bin/chronicle-native-trash", "web/index.html", updatepkg.ManifestName} {
		if _, err := os.Stat(filepath.Join(payload, relative)); err != nil {
			t.Fatalf("应用缺失 %s：%v", relative, err)
		}
	}
	if runtime.GOOS != "windows" {
		for _, relative := range []string{".", "Contents/Resources/app", "Contents/Resources/app/" + updatepkg.ManifestName} {
			info, err := os.Stat(filepath.Join(output, relative))
			if err != nil {
				t.Fatal(err)
			}
			required := os.FileMode(0o004)
			if info.IsDir() {
				required |= 0o001
			}
			if info.Mode().Perm()&required != required {
				t.Fatalf("系统安装后的普通用户无法读取 %s：%v", relative, info.Mode())
			}
		}
	}
	if _, err := os.Stat(filepath.Join(output, "Contents", "Resources", "install-macos.sh")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<key>CFBundleVersion</key><string>0.1.0</string>") || !strings.Contains(string(data), "<string>0.1.0-preview.2</string>") {
		t.Fatalf("应用版本元数据不匹配：%s", data)
	}
	if err := buildReleaseArchive(context.Background(), options); err == nil {
		t.Fatal("覆盖了现有 .app")
	}
}

func TestBuildReleaseArchiveRejectsWebSymlink(t *testing.T) {
	base := t.TempDir()
	daemon := filepath.Join(base, "daemon")
	if err := os.WriteFile(daemon, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(base, "web-dist")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.js")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(web, "app.js")); err != nil {
		t.Skipf("当前平台不支持符号链接：%v", err)
	}
	err := buildReleaseArchive(context.Background(), releaseOptions{Version: "1.0.0", Daemon: daemon, WebDist: web, Output: filepath.Join(base, "release.zip"), TargetOS: "linux", TargetArch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("Web 符号链接未被拒绝：%v", err)
	}
}

func TestBuildReleaseArchiveSignsManifest(t *testing.T) {
	base := t.TempDir()
	daemon := filepath.Join(base, "daemon")
	if err := os.WriteFile(daemon, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(base, "web-dist")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(base, "release.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(privateKey.Seed())), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(base, "signed-release.zip")
	options := releaseOptions{
		Version: "1.2.3", BuildID: "signed-build", Daemon: daemon, WebDist: web, Output: output,
		TargetOS: "linux", TargetArch: "arm64", SigningPrivateKeyFile: keyPath, SigningKeyID: "release-key-2026",
	}
	if err := buildReleaseArchive(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(base, "extracted")
	if err := updatepkg.ExtractArchive(context.Background(), output, extracted, updatepkg.DefaultExtractionLimits); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(extracted, updatepkg.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest updatepkg.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Signature == nil || manifest.Signature.KeyID != "release-key-2026" {
		t.Fatalf("signed manifest missing expected signature: %+v", manifest.Signature)
	}
	if err := updatepkg.VerifyManifestSignature(manifest, [][]byte{publicKey}); err != nil {
		t.Fatalf("signed release manifest did not verify: %v", err)
	}
}

func TestValidateReleaseOptionsRequiresSigningKeyForKeyID(t *testing.T) {
	err := validateReleaseOptions(releaseOptions{
		Version: "1.0.0", Daemon: "daemon", WebDist: "web", Output: "release.zip",
		TargetOS: "linux", TargetArch: "amd64", SigningKeyID: "release-key",
	})
	if err == nil || !strings.Contains(err.Error(), "signing-private-key-file") {
		t.Fatalf("key ID without private key was accepted: %v", err)
	}
}
