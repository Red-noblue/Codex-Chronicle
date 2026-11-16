// 中文说明：实现本地归档包的安全创建和校验；归档过程不删除或修改源 Codex 资产。
package archive

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

const manifestName = "manifest.json"
const controlPlaneSnapshotPayload = "control-plane/mutation-snapshot.json"

const (
	encryptionKDF        = "PBKDF2-HMAC-SHA256"
	encryptionCipher     = "AES-256-GCM"
	encryptionIterations = 310000
	encryptionChunkBytes = 4 * 1024 * 1024
)

var encryptedPayloadMagic = []byte("CCARCHIVE1\x00")

// Manifest 是归档包的可验证目录，不保存任何额外正文摘要。
type Manifest struct {
	Version              int                           `json:"version"`
	OperationID          string                        `json:"operation_id"`
	CreatedAt            time.Time                     `json:"created_at"`
	ConversationID       string                        `json:"conversation_id"`
	AssetSetHash         string                        `json:"asset_set_hash"`
	Encrypted            bool                          `json:"encrypted"`
	Encryption           *EncryptionMetadata           `json:"encryption,omitempty"`
	Assets               []lifecycle.AssetFingerprint  `json:"assets"`
	ControlPlaneAssets   []lifecycle.AssetFingerprint  `json:"control_plane_assets,omitempty"`
	ControlPlaneSnapshot *ControlPlaneSnapshotManifest `json:"control_plane_snapshot,omitempty"`
	CloudBackup          *CloudBackupMetadata          `json:"cloud_backup,omitempty"`
}

// ControlPlaneSnapshotManifest 描述归档内的控制面行快照；完整恢复绑定仍由 journal 保存。
type ControlPlaneSnapshotManifest struct {
	Version          int                    `json:"version"`
	AdapterID        string                 `json:"adapter_id"`
	ConversationID   string                 `json:"conversation_id"`
	SourceID         string                 `json:"source_id,omitempty"`
	AssetSetHash     string                 `json:"asset_set_hash,omitempty"`
	SnapshotHash     string                 `json:"snapshot_hash"`
	Assets           []compat.MutationAsset `json:"assets"`
	PayloadPath      string                 `json:"payload_path"`
	PayloadSizeBytes int64                  `json:"payload_size_bytes"`
	PayloadSHA256    string                 `json:"payload_sha256"`
}

// EncryptionMetadata 描述客户端加密参数；归档中不保存密码或明文密钥。
type EncryptionMetadata struct {
	KDF        string `json:"kdf"`
	Cipher     string `json:"cipher"`
	Salt       string `json:"salt"`
	Iterations int    `json:"iterations"`
}

// Result 是归档创建后的外部路径和 manifest 校验摘要。
type Result struct {
	Directory       string `json:"directory"`
	ManifestPath    string `json:"manifest_path"`
	AssetCount      int    `json:"asset_count"`
	Verified        bool   `json:"verified"`
	SourceUnchanged bool   `json:"source_unchanged"`
	Encrypted       bool   `json:"encrypted"`
}

// CreateLocal 将计划内资产复制到 destination 下的新归档目录，拒绝覆盖已有归档。
func CreateLocal(ctx context.Context, plan lifecycle.OperationPlan, destination string) (Result, error) {
	return createLocal(ctx, plan, destination, "", nil)
}

// CreateLocalWithMutationSnapshot 创建包含控制面行快照的本地归档。
func CreateLocalWithMutationSnapshot(ctx context.Context, plan lifecycle.OperationPlan, destination string, snapshot compat.MutationSnapshot) (Result, error) {
	return createLocal(ctx, plan, destination, "", &snapshot)
}

// CreateLocalEncrypted 使用用户密码在客户端加密每个 payload 文件后创建本地归档。
// 密码只在当前调用链中使用，manifest 仅保存 KDF 参数和随机盐。
func CreateLocalEncrypted(ctx context.Context, plan lifecycle.OperationPlan, destination, password string) (Result, error) {
	if strings.TrimSpace(password) == "" {
		return Result{}, fmt.Errorf("加密归档密码不能为空")
	}
	return createLocal(ctx, plan, destination, password, nil)
}

// CreateLocalEncryptedWithMutationSnapshot 创建包含加密控制面行快照的本地归档。
func CreateLocalEncryptedWithMutationSnapshot(ctx context.Context, plan lifecycle.OperationPlan, destination, password string, snapshot compat.MutationSnapshot) (Result, error) {
	if strings.TrimSpace(password) == "" {
		return Result{}, fmt.Errorf("加密归档密码不能为空")
	}
	return createLocal(ctx, plan, destination, password, &snapshot)
}

func createLocal(ctx context.Context, plan lifecycle.OperationPlan, destination, password string, mutationSnapshot *compat.MutationSnapshot) (Result, error) {
	if plan.Status != lifecycle.PlanPrepared && plan.Status != lifecycle.PlanConfirmed {
		return Result{}, fmt.Errorf("计划状态 %s 不可归档", plan.Status)
	}
	if len(plan.Assets) == 0 {
		return Result{}, fmt.Errorf("计划没有资产")
	}
	fileAssets := plan.FileAssets()
	if len(fileAssets) == 0 {
		return Result{}, fmt.Errorf("计划没有可归档的文件资产，控制面资产必须由 adapter 单独快照")
	}
	controlSnapshot, err := prepareControlPlaneSnapshot(ctx, plan, mutationSnapshot)
	if err != nil {
		return Result{}, err
	}
	root, err := filepath.Abs(plan.Root)
	if err != nil {
		return Result{}, fmt.Errorf("解析源根失败: %w", err)
	}
	archiveRoot, err := filepath.Abs(destination)
	if err != nil {
		return Result{}, fmt.Errorf("解析归档目录失败: %w", err)
	}
	if pathWithin(root, archiveRoot) || pathWithin(archiveRoot, root) {
		return Result{}, fmt.Errorf("归档目录不能位于源根或源根不能位于归档目录")
	}
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		return Result{}, fmt.Errorf("创建归档根失败: %w", err)
	}
	staging := filepath.Join(archiveRoot, "."+plan.OperationID+".tmp")
	final := filepath.Join(archiveRoot, plan.OperationID)
	if _, err := os.Lstat(final); err == nil {
		return Result{}, fmt.Errorf("归档目录已存在: %s", final)
	} else if !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("检查归档目录失败: %w", err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return Result{}, fmt.Errorf("创建归档临时目录失败: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	encrypted := password != ""
	var encryption *EncryptionMetadata
	var key []byte
	if encrypted {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return Result{}, fmt.Errorf("生成归档盐失败: %w", err)
		}
		encryption = &EncryptionMetadata{KDF: encryptionKDF, Cipher: encryptionCipher, Salt: hex.EncodeToString(salt), Iterations: encryptionIterations}
		key = deriveKey([]byte(password), salt, encryptionIterations, 32)
	}
	for _, asset := range fileAssets {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		source, err := application.ResolveReadOnlyPath(root, asset.RelativePath)
		if err != nil {
			return Result{}, fmt.Errorf("定位归档源 %s 失败: %w", asset.RelativePath, err)
		}
		target := filepath.Join(staging, "payload", filepath.FromSlash(asset.RelativePath))
		if err := ensureParent(target); err != nil {
			return Result{}, err
		}
		var copyErr error
		if encrypted {
			copyErr = copyAndEncryptAndVerify(ctx, source, target, asset, key)
		} else {
			copyErr = copyAndVerify(ctx, source, target, asset)
		}
		if copyErr != nil {
			return Result{}, copyErr
		}
	}
	if controlSnapshot != nil {
		sourceInfo, statErr := os.Lstat(mutationSnapshot.SnapshotRef)
		if statErr != nil {
			return Result{}, fmt.Errorf("读取控制面快照失败: %w", statErr)
		}
		rawHash, hashErr := hashFile(ctx, mutationSnapshot.SnapshotRef)
		if hashErr != nil {
			return Result{}, fmt.Errorf("计算控制面快照摘要失败: %w", hashErr)
		}
		if rawHash != controlSnapshot.PayloadSHA256 || sourceInfo.Size() != controlSnapshot.PayloadSizeBytes {
			return Result{}, fmt.Errorf("控制面快照在归档时发生变化")
		}
		controlTarget := filepath.Join(staging, "payload", filepath.FromSlash(controlSnapshot.PayloadPath))
		if err := ensureParent(controlTarget); err != nil {
			return Result{}, err
		}
		asset := lifecycle.AssetFingerprint{RelativePath: controlSnapshot.PayloadPath, SizeBytes: controlSnapshot.PayloadSizeBytes, SHA256: controlSnapshot.PayloadSHA256}
		var copyErr error
		if encrypted {
			copyErr = copyAndEncryptAndVerify(ctx, mutationSnapshot.SnapshotRef, controlTarget, asset, key)
		} else {
			copyErr = copyAndVerify(ctx, mutationSnapshot.SnapshotRef, controlTarget, asset)
		}
		if copyErr != nil {
			return Result{}, fmt.Errorf("复制控制面快照失败: %w", copyErr)
		}
	}
	manifest := Manifest{
		Version: 1, OperationID: plan.OperationID, CreatedAt: time.Now().UTC(),
		ConversationID: plan.ConversationID, AssetSetHash: plan.AssetSetHash,
		Encrypted: encrypted, Encryption: encryption, Assets: fileAssets, ControlPlaneAssets: controlPlaneAssets(plan), ControlPlaneSnapshot: controlSnapshot,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("生成归档 manifest 失败: %w", err)
	}
	if err := writeExclusive(filepath.Join(staging, manifestName), append(data, '\n')); err != nil {
		return Result{}, err
	}
	if err := verifyDirectory(ctx, staging, manifest, password); err != nil {
		return Result{}, err
	}
	if err := os.Rename(staging, final); err != nil {
		return Result{}, fmt.Errorf("发布归档目录失败: %w", err)
	}
	cleanup = false
	return Result{Directory: final, ManifestPath: filepath.Join(final, manifestName), AssetCount: len(fileAssets), Verified: true, SourceUnchanged: true, Encrypted: encrypted}, nil
}

func controlPlaneAssets(plan lifecycle.OperationPlan) []lifecycle.AssetFingerprint {
	assets := make([]lifecycle.AssetFingerprint, 0)
	for _, asset := range plan.Assets {
		if asset.ControlPlaneOnly || asset.Kind == domain.AssetKindSQLite || asset.Kind == domain.AssetKindSessionIndex {
			assets = append(assets, asset)
		}
	}
	return assets
}

func prepareControlPlaneSnapshot(ctx context.Context, plan lifecycle.OperationPlan, snapshot *compat.MutationSnapshot) (*ControlPlaneSnapshotManifest, error) {
	if snapshot == nil {
		return nil, nil
	}
	if err := compat.ValidateMutationSnapshot(ctx, *snapshot); err != nil {
		return nil, fmt.Errorf("控制面快照身份无效: %w", err)
	}
	if filepath.Clean(snapshot.Root) != filepath.Clean(plan.Root) || snapshot.ConversationID != plan.ConversationID {
		return nil, fmt.Errorf("控制面快照与操作计划不匹配")
	}
	if snapshot.AssetSetHash != "" && plan.AssetSetHash != "" && snapshot.AssetSetHash != plan.AssetSetHash {
		return nil, fmt.Errorf("控制面快照与操作计划资产集合不匹配")
	}
	if strings.TrimSpace(snapshot.SnapshotRef) == "" {
		return nil, fmt.Errorf("控制面快照缺少 payload 引用")
	}
	info, err := os.Lstat(snapshot.SnapshotRef)
	if err != nil {
		return nil, fmt.Errorf("检查控制面快照 payload 失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("控制面快照 payload 不是普通文件")
	}
	hash, err := hashFile(ctx, snapshot.SnapshotRef)
	if err != nil {
		return nil, fmt.Errorf("计算控制面快照 payload 摘要失败: %w", err)
	}
	return &ControlPlaneSnapshotManifest{
		Version: 1, AdapterID: snapshot.AdapterID, ConversationID: snapshot.ConversationID,
		SourceID: snapshot.SourceID, AssetSetHash: snapshot.AssetSetHash, SnapshotHash: snapshot.SnapshotHash,
		Assets: append([]compat.MutationAsset(nil), snapshot.Assets...), PayloadPath: controlPlaneSnapshotPayload,
		PayloadSizeBytes: info.Size(), PayloadSHA256: hash,
	}, nil
}

// VerifyLocal 重新校验归档 manifest 和所有 payload 文件。
func VerifyLocal(ctx context.Context, directory string) (Manifest, error) {
	return verifyLocal(ctx, directory, "")
}

// VerifyLocalEncrypted 使用密码解密并校验客户端加密归档的每个 payload。
func VerifyLocalEncrypted(ctx context.Context, directory, password string) (Manifest, error) {
	if strings.TrimSpace(password) == "" {
		return Manifest{}, fmt.Errorf("验证加密归档需要密码")
	}
	return verifyLocal(ctx, directory, password)
}

func verifyLocal(ctx context.Context, directory, password string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(directory, manifestName))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("解析归档 manifest 失败: %w", err)
	}
	if manifest.Version != 1 || manifest.OperationID == "" {
		return Manifest{}, fmt.Errorf("归档 manifest 版本或操作 ID 无效")
	}
	if manifest.Encrypted && strings.TrimSpace(password) == "" {
		return Manifest{}, fmt.Errorf("这是加密归档，请使用 VerifyLocalEncrypted 提供密码")
	}
	if err := verifyDirectory(ctx, directory, manifest, password); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func verifyDirectory(ctx context.Context, directory string, manifest Manifest, password string) error {
	var key []byte
	if manifest.Encrypted {
		if manifest.Encryption == nil || manifest.Encryption.KDF != encryptionKDF || manifest.Encryption.Cipher != encryptionCipher || manifest.Encryption.Iterations <= 0 {
			return fmt.Errorf("归档加密参数不受支持")
		}
		salt, err := hex.DecodeString(manifest.Encryption.Salt)
		if err != nil || len(salt) == 0 {
			return fmt.Errorf("归档加密盐无效")
		}
		key = deriveKey([]byte(password), salt, manifest.Encryption.Iterations, 32)
	}
	seen := make(map[string]struct{}, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := safePayloadRelative(asset.RelativePath)
		if err != nil {
			return fmt.Errorf("归档资产路径无效 %s: %w", asset.RelativePath, err)
		}
		if _, exists := seen[relative]; exists {
			return fmt.Errorf("归档包含重复资产路径 %s", relative)
		}
		seen[relative] = struct{}{}
		path := filepath.Join(directory, "payload", filepath.FromSlash(relative))
		if err := ensureNoSymlinkComponents(filepath.Join(directory, "payload"), path); err != nil {
			return fmt.Errorf("归档资产 %s 路径不安全: %w", asset.RelativePath, err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("归档缺少资产 %s: %w", asset.RelativePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("归档资产 %s 不是普通文件", asset.RelativePath)
		}
		var actual string
		var verifyErr error
		if manifest.Encrypted {
			actual, verifyErr = decryptHashFile(ctx, path, key)
		} else {
			actual, verifyErr = hashFile(ctx, path)
		}
		if verifyErr != nil {
			return verifyErr
		}
		if actual != asset.SHA256 || (!manifest.Encrypted && info.Size() != asset.SizeBytes) {
			return fmt.Errorf("归档资产 %s 哈希或大小不匹配", asset.RelativePath)
		}
	}
	if manifest.ControlPlaneSnapshot != nil {
		snapshot := manifest.ControlPlaneSnapshot
		if snapshot.Version != 1 || strings.TrimSpace(snapshot.AdapterID) == "" || strings.TrimSpace(snapshot.ConversationID) == "" || len(snapshot.SnapshotHash) != 64 || len(snapshot.PayloadSHA256) != 64 || snapshot.PayloadSizeBytes < 0 {
			return fmt.Errorf("归档控制面快照清单无效")
		}
		if _, err := hex.DecodeString(snapshot.SnapshotHash); err != nil {
			return fmt.Errorf("归档控制面快照身份摘要无效")
		}
		if _, err := hex.DecodeString(snapshot.PayloadSHA256); err != nil {
			return fmt.Errorf("归档控制面快照摘要无效")
		}
		relative, err := safePayloadRelative(snapshot.PayloadPath)
		if err != nil {
			return fmt.Errorf("归档控制面快照路径无效: %w", err)
		}
		if _, exists := seen[relative]; exists {
			return fmt.Errorf("归档控制面快照路径与普通资产重复")
		}
		seen[relative] = struct{}{}
		path := filepath.Join(directory, "payload", filepath.FromSlash(relative))
		if err := ensureNoSymlinkComponents(filepath.Join(directory, "payload"), path); err != nil {
			return fmt.Errorf("归档控制面快照路径不安全: %w", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("归档缺少控制面快照: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("归档控制面快照不是普通文件")
		}
		var actual string
		if manifest.Encrypted {
			actual, err = decryptHashFile(ctx, path, key)
		} else {
			actual, err = hashFile(ctx, path)
		}
		if err != nil {
			return fmt.Errorf("读取归档控制面快照失败: %w", err)
		}
		if actual != snapshot.PayloadSHA256 || (!manifest.Encrypted && info.Size() != snapshot.PayloadSizeBytes) {
			return fmt.Errorf("归档控制面快照哈希或大小不匹配")
		}
	}
	return nil
}

// EnsureMutationSnapshot 从已校验的未加密本地归档补回缺失的 adapter 快照文件。
// journal 仍是恢复绑定来源，归档 manifest 只负责验证 payload 身份和完整性。
func EnsureMutationSnapshot(ctx context.Context, directory string, snapshot compat.MutationSnapshot) error {
	manifest, err := VerifyLocal(ctx, directory)
	if err != nil {
		return fmt.Errorf("校验控制面归档失败: %w", err)
	}
	return ensureMutationSnapshotFromManifest(ctx, directory, manifest, "", snapshot)
}

// EnsureMutationSnapshotWithPassword 从已校验的加密本地归档补回 adapter 快照；密码只存在于当前调用栈。
func EnsureMutationSnapshotWithPassword(ctx context.Context, directory, password string, snapshot compat.MutationSnapshot) error {
	manifest, err := VerifyLocalEncrypted(ctx, directory, password)
	if err != nil {
		return fmt.Errorf("校验加密控制面归档失败: %w", err)
	}
	return ensureMutationSnapshotFromManifest(ctx, directory, manifest, password, snapshot)
}

func ensureMutationSnapshotFromManifest(ctx context.Context, directory string, manifest Manifest, password string, snapshot compat.MutationSnapshot) error {
	archived := manifest.ControlPlaneSnapshot
	if archived == nil {
		return fmt.Errorf("归档不包含控制面快照")
	}
	if archived.AdapterID != snapshot.AdapterID || archived.SnapshotHash != snapshot.SnapshotHash || archived.ConversationID != snapshot.ConversationID || archived.SourceID != snapshot.SourceID {
		return fmt.Errorf("归档控制面快照与 journal 不匹配")
	}
	if archived.AssetSetHash != snapshot.AssetSetHash || !mutationAssetsEqual(archived.Assets, snapshot.Assets) {
		return fmt.Errorf("归档控制面快照资产与 journal 不匹配")
	}
	if filepath.Clean(snapshot.SnapshotRef) == "." || strings.TrimSpace(snapshot.SnapshotRef) == "" {
		return fmt.Errorf("journal 控制面快照引用无效")
	}
	relative, err := safePayloadRelative(archived.PayloadPath)
	if err != nil {
		return fmt.Errorf("归档控制面快照路径无效: %w", err)
	}
	source := filepath.Join(directory, "payload", filepath.FromSlash(relative))
	var data []byte
	if manifest.Encrypted {
		if strings.TrimSpace(password) == "" {
			return fmt.Errorf("解密控制面归档需要密码")
		}
		staging, stageErr := os.MkdirTemp("", ".codex-chronicle-snapshot-*")
		if stageErr != nil {
			return fmt.Errorf("创建控制面快照解密临时目录失败: %w", stageErr)
		}
		defer func() { _ = os.RemoveAll(staging) }()
		staged := filepath.Join(staging, "snapshot.json")
		expected := lifecycle.AssetFingerprint{RelativePath: relative, SizeBytes: archived.PayloadSizeBytes, SHA256: archived.PayloadSHA256}
		if decryptErr := decryptAndVerifyToFile(ctx, source, staged, expected, manifest, password); decryptErr != nil {
			return fmt.Errorf("解密控制面快照失败: %w", decryptErr)
		}
		data, err = os.ReadFile(staged)
	} else {
		data, err = os.ReadFile(source)
	}
	if err != nil {
		return fmt.Errorf("读取归档控制面快照失败: %w", err)
	}
	if sha256Hex(data) != archived.PayloadSHA256 || int64(len(data)) != archived.PayloadSizeBytes {
		return fmt.Errorf("归档控制面快照 payload 摘要或大小不匹配")
	}
	target := filepath.Clean(snapshot.SnapshotRef)
	if hasCodexPathComponent(target) {
		return fmt.Errorf("拒绝把控制面快照写入 .codex 路径")
	}
	if err := ensureSafeSnapshotParent(target); err != nil {
		return err
	}
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("adapter 快照目标不是普通文件")
		}
		current, readErr := os.ReadFile(target)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(current, data) {
			return fmt.Errorf("adapter 快照目标已存在且内容冲突")
		}
		return nil
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("检查 adapter 快照目标失败: %w", statErr)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("创建 adapter 快照目录失败: %w", err)
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("写入 adapter 快照失败: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(target)
		return fmt.Errorf("写入 adapter 快照失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(target)
		return fmt.Errorf("同步 adapter 快照失败: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("关闭 adapter 快照失败: %w", err)
	}
	return nil
}

func mutationAssetsEqual(left, right []compat.MutationAsset) bool {
	if len(left) != len(right) {
		return false
	}
	leftByPath := make(map[string]compat.MutationAsset, len(left))
	for _, asset := range left {
		leftByPath[asset.RelativePath] = asset
	}
	for _, asset := range right {
		other, ok := leftByPath[asset.RelativePath]
		if !ok || other.Kind != asset.Kind || other.SizeBytes != asset.SizeBytes || other.SHA256 != asset.SHA256 || other.Status != asset.Status {
			return false
		}
	}
	return true
}

func hasCodexPathComponent(value string) bool {
	for _, part := range strings.Split(filepath.Clean(value), string(filepath.Separator)) {
		if strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return false
}

func ensureSafeSnapshotParent(path string) error {
	current := filepath.Dir(filepath.Clean(path))
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				resolved, resolveErr := filepath.EvalSymlinks(current)
				if resolveErr != nil {
					return fmt.Errorf("解析 adapter 快照父路径符号链接失败: %w", resolveErr)
				}
				resolvedInfo, statErr := os.Stat(resolved)
				if statErr != nil || resolvedInfo == nil || !resolvedInfo.IsDir() {
					return fmt.Errorf("adapter 快照父路径符号链接目标不是目录: %s", current)
				}
			} else if !info.IsDir() {
				return fmt.Errorf("adapter 快照父路径包含非目录: %s", current)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查 adapter 快照父路径失败: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

func safePayloadRelative(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\x00") {
		return "", fmt.Errorf("路径为空或包含 NUL")
	}
	if filepath.IsAbs(filepath.FromSlash(value)) {
		return "", fmt.Errorf("路径不能是绝对路径")
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越出 payload 根")
	}
	return filepath.ToSlash(clean), nil
}

func ensureNoSymlinkComponents(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("路径越出归档根")
	}
	current := root
	for _, part := range strings.Split(filepath.Dir(relative), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("路径包含符号链接")
		}
	}
	return nil
}

func copyAndVerify(ctx context.Context, source, target string, expected lifecycle.AssetFingerprint) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("打开归档源 %s 失败: %w", expected.RelativePath, err)
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建归档文件 %s 失败: %w", expected.RelativePath, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	closeErr := output.Close()
	if copyErr != nil {
		return fmt.Errorf("复制归档资产 %s 失败: %w", expected.RelativePath, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭归档文件 %s 失败: %w", expected.RelativePath, closeErr)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("源资产 %s 在归档时发生变化", expected.RelativePath)
	}
	return nil
}

func copyAndEncryptAndVerify(ctx context.Context, source, target string, expected lifecycle.AssetFingerprint, key []byte) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("打开加密归档源 %s 失败: %w", expected.RelativePath, err)
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建加密归档文件 %s 失败: %w", expected.RelativePath, err)
	}
	closeOutput := func() error {
		if closeErr := output.Close(); closeErr != nil {
			return fmt.Errorf("关闭加密归档文件 %s 失败: %w", expected.RelativePath, closeErr)
		}
		return nil
	}
	gcm, err := newGCM(key)
	if err != nil {
		_ = output.Close()
		return err
	}
	if _, err := output.Write(encryptedPayloadMagic); err != nil {
		_ = output.Close()
		return fmt.Errorf("写入加密归档头失败: %w", err)
	}
	hash := sha256.New()
	buffer := make([]byte, encryptionChunkBytes)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			_ = output.Close()
			return err
		}
		read, readErr := input.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			copied += int64(read)
			nonce := make([]byte, gcm.NonceSize())
			if _, err := rand.Read(nonce); err != nil {
				_ = output.Close()
				return fmt.Errorf("生成加密归档 nonce 失败: %w", err)
			}
			ciphertext := gcm.Seal(nil, nonce, buffer[:read], nil)
			var length [4]byte
			binary.BigEndian.PutUint32(length[:], uint32(read))
			if _, err := output.Write(length[:]); err != nil {
				_ = output.Close()
				return fmt.Errorf("写入加密归档分块长度失败: %w", err)
			}
			if _, err := output.Write(nonce); err != nil {
				_ = output.Close()
				return fmt.Errorf("写入加密归档 nonce 失败: %w", err)
			}
			if _, err := output.Write(ciphertext); err != nil {
				_ = output.Close()
				return fmt.Errorf("写入加密归档分块失败: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = output.Close()
			return fmt.Errorf("读取加密归档源 %s 失败: %w", expected.RelativePath, readErr)
		}
	}
	// 零长度块是结束标记，允许空文件安全表示且能检测尾部截断。
	var end [4]byte
	if _, err := output.Write(end[:]); err != nil {
		_ = output.Close()
		return fmt.Errorf("写入加密归档结束标记失败: %w", err)
	}
	if err := closeOutput(); err != nil {
		return err
	}
	if copied != expected.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("源资产 %s 在加密归档时发生变化", expected.RelativePath)
	}
	return nil
}

func decryptHashFile(ctx context.Context, path string, key []byte) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	header := make([]byte, len(encryptedPayloadMagic))
	if _, err := io.ReadFull(input, header); err != nil {
		return "", fmt.Errorf("读取加密归档头失败: %w", err)
	}
	if string(header) != string(encryptedPayloadMagic) {
		return "", fmt.Errorf("加密归档头无效")
	}
	hash := sha256.New()
	ended := false
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var length [4]byte
		if _, err := io.ReadFull(input, length[:]); err != nil {
			return "", fmt.Errorf("读取加密归档分块长度失败: %w", err)
		}
		plainLength := binary.BigEndian.Uint32(length[:])
		if plainLength == 0 {
			ended = true
			break
		}
		if plainLength > encryptionChunkBytes {
			return "", fmt.Errorf("加密归档分块过大")
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err := io.ReadFull(input, nonce); err != nil {
			return "", fmt.Errorf("读取加密归档 nonce 失败: %w", err)
		}
		ciphertext := make([]byte, int(plainLength)+gcm.Overhead())
		if _, err := io.ReadFull(input, ciphertext); err != nil {
			return "", fmt.Errorf("读取加密归档分块失败: %w", err)
		}
		plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return "", fmt.Errorf("解密归档分块失败: %w", err)
		}
		if len(plaintext) != int(plainLength) {
			return "", fmt.Errorf("解密归档分块长度不匹配")
		}
		_, _ = hash.Write(plaintext)
	}
	if !ended {
		return "", fmt.Errorf("加密归档缺少结束标记")
	}
	var trailing [1]byte
	if read, err := input.Read(trailing[:]); err != io.EOF || read != 0 {
		return "", fmt.Errorf("加密归档存在结束标记后的额外数据")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("创建 AES-256 cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 AES-GCM 失败: %w", err)
	}
	return gcm, nil
}

func deriveKey(password, salt []byte, iterations, length int) []byte {
	key := make([]byte, 0, length)
	for block := uint32(1); len(key) < length; block++ {
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		var index [4]byte
		binary.BigEndian.PutUint32(index[:], block)
		_, _ = mac.Write(index[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for round := 1; round < iterations; round++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for position := range t {
				t[position] ^= u[position]
			}
		}
		key = append(key, t...)
	}
	return key[:length]
}

func hashFile(ctx context.Context, path string) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		read, readErr := input.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ensureParent(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建归档父目录失败: %w", err)
	}
	return nil
}

func writeExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("写入归档 manifest 失败: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入归档 manifest 失败: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭归档 manifest 失败: %w", err)
	}
	return nil
}

func containsEncrypted(plan lifecycle.OperationPlan) bool {
	for _, action := range plan.Actions {
		if action == lifecycle.ActionEncryptedArchive {
			return true
		}
	}
	return false
}

func pathWithin(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
