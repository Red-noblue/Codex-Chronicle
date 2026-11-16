// 中文说明：验证离线更新清单的 Ed25519 信任根、签名篡改检测和公钥文件解析。
package update

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignedManifestRequiresTrustedKeyAndActivates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "codex-chronicle"), []byte("signed"), 0o700); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildManifest(context.Background(), root, "1.0.0", "signed-build")
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignManifest(manifest, privateKey, "release-key-2026")
	if err != nil {
		t.Fatal(err)
	}
	if signed.Signature == nil || signed.Signature.KeyID != "release-key-2026" {
		t.Fatalf("signature was not attached: %+v", signed.Signature)
	}
	if err := WriteManifest(context.Background(), root, signed); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManagerWithOptions(filepath.Join(t.TempDir(), "install"), ManagerOptions{RequireSignature: true, TrustedPublicKeys: [][]byte{publicKey}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := manager.Prepare(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActivateByID(context.Background(), plan.ID, manager.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.SignatureRequired || status.TrustedKeyCount != 1 || status.State.ActiveVersion != "1.0.0" {
		t.Fatalf("unexpected signed update status: %+v", status)
	}
}

func TestSignedManifestRejectsMissingUnknownAndTamperedTrust(t *testing.T) {
	root := makeSignedBundle(t)
	unsignedManager, err := NewManagerWithOptions(filepath.Join(t.TempDir(), "unsigned"), ManagerOptions{RequireSignature: true, TrustedPublicKeys: [][]byte{makePublicKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	unsignedRoot := makeBundle(t, "1.0.1", "unsigned")
	if _, err := unsignedManager.Prepare(context.Background(), unsignedRoot, false); err == nil || !strings.Contains(err.Error(), "缺少签名") {
		t.Fatalf("unsigned bundle was accepted: %v", err)
	}

	unknownManager, err := NewManagerWithOptions(filepath.Join(t.TempDir(), "unknown"), ManagerOptions{RequireSignature: true, TrustedPublicKeys: [][]byte{makePublicKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unknownManager.Prepare(context.Background(), root, false); err == nil || !strings.Contains(err.Error(), "信任根") {
		t.Fatalf("unknown signing key was accepted: %v", err)
	}

	manifestPath := filepath.Join(root, ManifestName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "signed-build", "tampered-build", 1)
	if updated == string(data) {
		t.Fatal("test manifest did not contain expected build ID")
	}
	if err := os.WriteFile(manifestPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	trustedManager, err := NewManagerWithOptions(filepath.Join(t.TempDir(), "tampered"), ManagerOptions{RequireSignature: true, TrustedPublicKeys: [][]byte{readBundlePublicKey(t, root)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trustedManager.Prepare(context.Background(), root, false); err == nil || !strings.Contains(err.Error(), "签名校验失败") {
		t.Fatalf("tampered manifest was accepted: %v", err)
	}
}

func TestLoadTrustedPublicKeysSupportsBase64HexAndComments(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "trusted.keys")
	content := "# Codex Chronicle update key\n" + base64.StdEncoding.EncodeToString(publicKey) + "\n" + hex.EncodeToString(publicKey) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadTrustedPublicKeys(path)
	if err != nil || len(keys) != 1 || string(keys[0]) != string(publicKey) {
		t.Fatalf("unexpected trusted keys: keys=%d err=%v", len(keys), err)
	}
}

func TestLoadSigningPrivateKeySupportsSeedPrivateKeyAndText(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		data []byte
	}{
		{name: "raw seed", data: append([]byte(nil), privateKey.Seed()...)},
		{name: "raw private key", data: append([]byte(nil), privateKey...)},
		{name: "base64 seed", data: []byte(base64.StdEncoding.EncodeToString(privateKey.Seed()) + "\n")},
		{name: "hex private key", data: []byte(hex.EncodeToString(privateKey))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "signing.key")
			if err := os.WriteFile(path, testCase.data, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadSigningPrivateKey(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(loaded) != string(privateKey) {
				t.Fatalf("loaded private key does not match")
			}
			if string(loaded.Public().(ed25519.PublicKey)) != string(publicKey) {
				t.Fatalf("loaded public key does not match")
			}
		})
	}
}

func TestLoadSigningPrivateKeyRejectsSymlinkAndInvalidMaterial(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside.key")
	if err := os.WriteFile(outside, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link.key")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := LoadSigningPrivateKey(link); err == nil || !strings.Contains(err.Error(), "普通文件") {
			t.Fatalf("symlink signing key was accepted: %v", err)
		}
	}
	invalid := filepath.Join(base, "invalid.key")
	if err := os.WriteFile(invalid, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigningPrivateKey(invalid); err == nil || !strings.Contains(err.Error(), "私钥无效") {
		t.Fatalf("invalid signing key was accepted: %v", err)
	}
}

func makeSignedBundle(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "signed-bundle")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "codex-chronicle"), []byte("signed"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, privateKey, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildManifest(context.Background(), root, "1.0.0", "signed-build")
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignManifest(manifest, privateKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(context.Background(), root, signed); err != nil {
		t.Fatal(err)
	}
	return root
}

func readBundlePublicKey(t *testing.T, root string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	key, err := decodeKeyMaterial(manifest.Signature.PublicKey, ed25519.PublicKeySize)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func makePublicKey(t *testing.T) []byte {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}
