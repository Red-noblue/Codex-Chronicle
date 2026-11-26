// 中文说明：验证更新密钥只生成一次、输出不含私钥、文件权限正确，并能对项目清单签名验签。
package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	updatepkg "codex-chronicle.local/internal/update"
)

func TestSigningKeyCommandsGenerateVerifyAndNeverOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "update-signing.key")
	info, err := signingKeyCommand("keygen", []string{"--output", path})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := json.Marshal(info)
	if info.KeyID == "" || info.PublicKey == "" || strings.Contains(string(output), strings.TrimSpace(string(content))) {
		t.Fatal("公钥信息缺失或输出包含私钥")
	}
	if file, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && file.Mode().Perm() != 0o600 {
		t.Fatal("私钥文件权限不正确")
	}
	private, err := updatepkg.LoadSigningPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := updatepkg.SignManifest(updatepkg.Manifest{Version: "1.0.0"}, private, info.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	public, err := base64.StdEncoding.DecodeString(info.PublicKey)
	if err != nil || updatepkg.VerifyManifestSignature(signed, [][]byte{public}) != nil {
		t.Fatal("生成的密钥不能通过项目签名校验")
	}
	if _, err := signingKeyCommand("keygen", []string{"--output", path}); err == nil {
		t.Fatal("重复生成覆盖了已有密钥")
	}
	again, err := signingKeyCommand("key-info", []string{"--output", path})
	after, _ := os.ReadFile(path)
	if err != nil || again != info || string(after) != string(content) {
		t.Fatal("只读查询或重复生成改变了已有密钥")
	}
}

func TestSigningKeyCommandRejectsMissingPathAndInvalidKey(t *testing.T) {
	if _, err := signingKeyCommand("keygen", nil); err == nil {
		t.Fatal("缺失路径仍生成了密钥")
	}
	path := filepath.Join(t.TempDir(), "invalid.key")
	if err := os.WriteFile(path, []byte("不是密钥"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := signingKeyCommand("key-info", []string{"--output", path}); err == nil {
		t.Fatal("无效密钥被接纳")
	}
}
