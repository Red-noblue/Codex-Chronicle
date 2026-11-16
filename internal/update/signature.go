// 中文说明：为离线更新清单提供 Ed25519 签名、信任公钥解析和篡改校验。
package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const manifestSignatureAlgorithm = "ed25519"

const maxSigningPrivateKeyFileBytes = 4096

// ManifestSignature 保存清单的签名材料；公钥只用于标识，验证仍必须命中本地信任根。
type ManifestSignature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

// ManagerOptions 控制更新管理器的供应链校验策略。
type ManagerOptions struct {
	TrustedPublicKeys [][]byte
	RequireSignature  bool
	Online            OnlineConfig
}

// SignManifest 对不含 Signature 字段的规范清单签名，返回带签名副本。
func SignManifest(manifest Manifest, privateKey ed25519.PrivateKey, keyID string) (Manifest, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return Manifest{}, errors.New("Ed25519 私钥长度无效")
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return Manifest{}, errors.New("无法从 Ed25519 私钥取得公钥")
	}
	manifest.Signature = nil
	canonical, err := canonicalManifestBytes(manifest)
	if err != nil {
		return Manifest{}, err
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		keyID = publicKeyID(publicKey)
	}
	manifest.Signature = &ManifestSignature{
		Algorithm: manifestSignatureAlgorithm,
		KeyID:     keyID,
		PublicKey: base64.StdEncoding.EncodeToString(publicKey),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, canonical)),
	}
	return manifest, nil
}

// VerifyManifestSignature 验证清单签名，并要求清单公钥命中调用方提供的信任根。
func VerifyManifestSignature(manifest Manifest, trustedPublicKeys [][]byte) error {
	if manifest.Signature == nil {
		return errors.New("离线清单缺少签名")
	}
	signature := manifest.Signature
	publicKey, signedValue, err := validateManifestSignatureSyntax(signature)
	if err != nil {
		return err
	}
	trusted := false
	for _, candidate := range trustedPublicKeys {
		if len(candidate) == ed25519.PublicKeySize && string(candidate) == string(publicKey) {
			trusted = true
			break
		}
	}
	if !trusted {
		return errors.New("清单公钥不在本地信任根中")
	}
	unsigned := manifest
	unsigned.Signature = nil
	canonical, err := canonicalManifestBytes(unsigned)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), canonical, signedValue) {
		return errors.New("清单签名校验失败")
	}
	return nil
}

func validateManifestSignatureSyntax(signature *ManifestSignature) ([]byte, []byte, error) {
	if signature == nil {
		return nil, nil, errors.New("离线清单缺少签名")
	}
	if strings.ToLower(strings.TrimSpace(signature.Algorithm)) != manifestSignatureAlgorithm {
		return nil, nil, fmt.Errorf("不支持的清单签名算法：%s", signature.Algorithm)
	}
	publicKey, err := decodeKeyMaterial(signature.PublicKey, ed25519.PublicKeySize)
	if err != nil {
		return nil, nil, fmt.Errorf("清单公钥无效：%w", err)
	}
	signedValue, err := decodeKeyMaterial(signature.Signature, ed25519.SignatureSize)
	if err != nil {
		return nil, nil, fmt.Errorf("清单签名值无效：%w", err)
	}
	return publicKey, signedValue, nil
}

// LoadTrustedPublicKeys 读取每行一个 Base64 或十六进制 Ed25519 公钥的文件。
// 文件必须位于普通路径，空行和 # 注释会被忽略。
func LoadTrustedPublicKeys(path string) ([][]byte, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("读取更新信任公钥文件失败：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("更新信任公钥文件必须是普通文件")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	keys := make([][]byte, 0)
	seen := make(map[string]struct{})
	for lineNumber, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, decodeErr := decodeKeyMaterial(line, ed25519.PublicKeySize)
		if decodeErr != nil {
			return nil, fmt.Errorf("第 %d 行公钥无效：%w", lineNumber+1, decodeErr)
		}
		id := publicKeyID(key)
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		keys = append(keys, append([]byte(nil), key...))
	}
	if len(keys) == 0 {
		return nil, errors.New("更新信任公钥文件没有有效公钥")
	}
	return keys, nil
}

// LoadSigningPrivateKey 读取发布签名私钥；支持 32 字节 seed、64 字节私钥以及 Base64/十六进制文本。
// 私钥文件必须是普通文件，读取内容不会写入日志或清单。
func LoadSigningPrivateKey(path string) (ed25519.PrivateKey, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("读取更新签名私钥文件失败：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("更新签名私钥文件必须是普通文件")
	}
	if info.Size() > maxSigningPrivateKeyFileBytes {
		return nil, fmt.Errorf("更新签名私钥文件过大：%d 字节，最多允许 %d 字节", info.Size(), maxSigningPrivateKeyFileBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取更新签名私钥文件失败：%w", err)
	}
	key, err := decodeSigningPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("更新签名私钥无效：%w", err)
	}
	return key, nil
}

func decodeSigningPrivateKey(data []byte) (ed25519.PrivateKey, error) {
	if len(data) == ed25519.PrivateKeySize {
		return append(ed25519.PrivateKey(nil), data...), nil
	}
	if len(data) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(data), nil
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return nil, errors.New("密钥材料为空")
	}
	for _, decode := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		hex.DecodeString,
	} {
		decoded, err := decode(value)
		if err != nil {
			continue
		}
		switch len(decoded) {
		case ed25519.PrivateKeySize:
			return append(ed25519.PrivateKey(nil), decoded...), nil
		case ed25519.SeedSize:
			return ed25519.NewKeyFromSeed(decoded), nil
		}
	}
	return nil, errors.New("不是有效的 32/64 字节、Base64 或十六进制 Ed25519 私钥")
}

func canonicalManifestBytes(manifest Manifest) ([]byte, error) {
	manifest.Signature = nil
	return json.Marshal(manifest)
}

func decodeKeyMaterial(value string, size int) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("密钥材料为空")
	}
	var decodedLengths []int
	for _, decode := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		hex.DecodeString,
	} {
		decoded, err := decode(value)
		if err != nil {
			continue
		}
		if len(decoded) == size {
			return decoded, nil
		}
		decodedLengths = append(decodedLengths, len(decoded))
	}
	if len(decodedLengths) > 0 {
		return nil, fmt.Errorf("长度为 %d，要求 %d", decodedLengths[0], size)
	}
	return nil, errors.New("不是有效 Base64 或十六进制")
}

func publicKeyID(key []byte) string {
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:8])
}
