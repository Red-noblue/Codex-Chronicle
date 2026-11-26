// 中文说明：验证最终 ZIP 必须命中仓库公钥、目标版本和源码提交，错误身份不能通过发布验收。
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVerifyFinalArchiveBindsTrustedKeyVersionAndCommit(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "test-signing.key")
	info, err := signingKeyCommand("keygen", []string{"--output", key})
	if err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(root, "trusted-public-key")
	if err := os.WriteFile(public, []byte(info.PublicKey), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "binary")
	if err := os.WriteFile(binary, []byte("隔离测试程序"), 0o700); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(root, "web")
	if err := os.Mkdir(web, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("测试界面"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "final.zip")
	commit := strings.Repeat("a", 40)
	options := releaseOptions{Version: "1.0.0-preview.1", BuildID: commit, Daemon: binary, Launcher: binary, WebDist: web,
		Output: archive, Format: "zip", TargetOS: runtime.GOOS, TargetArch: runtime.GOARCH, SigningPrivateKeyFile: key, SigningKeyID: info.KeyID}
	if err := buildReleaseArchive(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	args := []string{"--archive", archive, "--trusted-public-key-file", public, "--version", options.Version, "--build-id", commit}
	manifest, err := verifyArchiveCommand(t.Context(), args)
	if err != nil || manifest.BuildID != commit {
		t.Fatalf("最终 ZIP 验证失败：%v", err)
	}
	wrong := append([]string(nil), args...)
	wrong[len(wrong)-1] = strings.Repeat("b", 40)
	if _, err := verifyArchiveCommand(t.Context(), wrong); err == nil {
		t.Fatal("错误源码提交被接纳")
	}
	wrong = append([]string(nil), args...)
	wrong[5] = "9.9.9"
	if _, err := verifyArchiveCommand(t.Context(), wrong); err == nil {
		t.Fatal("错误版本被接纳")
	}
	otherKey, err := signingKeyCommand("keygen", []string{"--output", filepath.Join(root, "other.key")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(public, []byte(otherKey.PublicKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyArchiveCommand(t.Context(), args); err == nil {
		t.Fatal("其他公钥接纳了最终安装包")
	}
}
