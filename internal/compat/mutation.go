// 中文说明：冻结版本化 control-plane mutation 的快照、校验和恢复契约；本文件不连接真实 Codex 数据库。
package compat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"codex-chronicle.local/internal/domain"
)

// MutationAction 是 control-plane adapter 可以实现的破坏性或补偿动作。
type MutationAction string

const (
	MutationDelete  MutationAction = "delete"
	MutationRestore MutationAction = "restore"
	MutationRepair  MutationAction = "repair"
)

// MutationAsset 是 adapter 需要核对的物理资产摘要，不包含正文和数据库行内容。
type MutationAsset struct {
	RelativePath string             `json:"relative_path"`
	Kind         domain.AssetKind   `json:"kind"`
	SizeBytes    int64              `json:"size_bytes"`
	SHA256       string             `json:"sha256"`
	Status       domain.AssetStatus `json:"status"`
}

// MutationRequest 是一次计划绑定的 control-plane 输入；adapter 必须把它当作只读快照来源。
type MutationRequest struct {
	Root           string          `json:"root"`
	ConversationID string          `json:"conversation_id"`
	SourceID       string          `json:"source_id,omitempty"`
	AssetSetHash   string          `json:"asset_set_hash,omitempty"`
	SnapshotRef    string          `json:"snapshot_ref,omitempty"`
	SnapshotSHA256 string          `json:"snapshot_sha256,omitempty"`
	Assets         []MutationAsset `json:"assets"`
}

// MutationSnapshot 是执行前由 adapter 生成的可验证身份；它不保存正文。
type MutationSnapshot struct {
	Version        int             `json:"version"`
	AdapterID      string          `json:"adapter_id"`
	Root           string          `json:"root"`
	ConversationID string          `json:"conversation_id"`
	SourceID       string          `json:"source_id,omitempty"`
	AssetSetHash   string          `json:"asset_set_hash,omitempty"`
	SnapshotRef    string          `json:"snapshot_ref,omitempty"`
	SnapshotSHA256 string          `json:"snapshot_sha256,omitempty"`
	Assets         []MutationAsset `json:"assets"`
	SnapshotHash   string          `json:"snapshot_hash"`
}

// MutationOutcome 是 adapter 应写入 journal 的最小结果摘要。
type MutationOutcome struct {
	Action       MutationAction `json:"action"`
	AdapterID    string         `json:"adapter_id"`
	SnapshotHash string         `json:"snapshot_hash"`
	ChangedFiles int            `json:"changed_files"`
	ChangedRows  int            `json:"changed_rows"`
	Verified     bool           `json:"verified"`
	NeedsReview  bool           `json:"needs_review"`
	Verification []string       `json:"verification,omitempty"`
}

// ControlPlaneMutation 是版本化 adapter 的写入边界；未知版本不得实现为“尽力删除”。
type ControlPlaneMutation interface {
	Descriptor() Descriptor
	Prepare(ctx context.Context, request MutationRequest) (MutationSnapshot, error)
	Apply(ctx context.Context, snapshot MutationSnapshot, action MutationAction) (MutationOutcome, error)
	Verify(ctx context.Context, snapshot MutationSnapshot, outcome MutationOutcome) error
	Restore(ctx context.Context, snapshot MutationSnapshot) (MutationOutcome, error)
}

// MutationError 是可展示、可测试且默认只读的 adapter 错误。
type MutationError struct {
	Code       string     `json:"code"`
	Capability Capability `json:"capability,omitempty"`
	Message    string     `json:"message"`
	ReadOnly   bool       `json:"read_only"`
}

func (e *MutationError) Error() string {
	if e == nil {
		return "未知 mutation 错误"
	}
	return e.Message
}

// ValidateMutationRequest 在 adapter 打开数据库或获取锁之前拒绝不安全输入。
func ValidateMutationRequest(ctx context.Context, request MutationRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(request.Root) == "" || !filepath.IsAbs(request.Root) {
		return &MutationError{Code: "MUTATION_ROOT_NOT_ABSOLUTE", Message: "mutation 根目录必须是绝对路径", ReadOnly: true}
	}
	if strings.TrimSpace(request.ConversationID) == "" {
		return &MutationError{Code: "MUTATION_CONVERSATION_MISSING", Message: "mutation 请求缺少对话 ID", ReadOnly: true}
	}
	if len(request.Assets) == 0 {
		return &MutationError{Code: "MUTATION_ASSETS_MISSING", Message: "mutation 请求没有资产快照", ReadOnly: true}
	}
	if strings.TrimSpace(request.SnapshotRef) != "" {
		if len(request.SnapshotSHA256) != 64 {
			return &MutationError{Code: "MUTATION_SNAPSHOT_HASH_INVALID", Message: "mutation 快照文件摘要无效", ReadOnly: true}
		}
		if _, err := hex.DecodeString(request.SnapshotSHA256); err != nil {
			return &MutationError{Code: "MUTATION_SNAPSHOT_HASH_INVALID", Message: "mutation 快照文件摘要不是十六进制", ReadOnly: true}
		}
	}
	seen := make(map[string]struct{}, len(request.Assets))
	for _, asset := range request.Assets {
		path := filepath.Clean(filepath.FromSlash(asset.RelativePath))
		if strings.TrimSpace(asset.RelativePath) == "" || strings.Contains(asset.RelativePath, "\x00") || filepath.IsAbs(path) || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return &MutationError{Code: "MUTATION_ASSET_PATH_INVALID", Message: fmt.Sprintf("资产路径不安全：%q", asset.RelativePath), ReadOnly: true}
		}
		canonical := filepath.ToSlash(path)
		if _, exists := seen[canonical]; exists {
			return &MutationError{Code: "MUTATION_ASSET_DUPLICATE", Message: fmt.Sprintf("资产路径重复：%s", canonical), ReadOnly: true}
		}
		seen[canonical] = struct{}{}
		if asset.SizeBytes < 0 {
			return &MutationError{Code: "MUTATION_ASSET_SIZE_INVALID", Message: fmt.Sprintf("资产大小无效：%s", canonical), ReadOnly: true}
		}
		if len(asset.SHA256) != 64 {
			return &MutationError{Code: "MUTATION_ASSET_HASH_INVALID", Message: fmt.Sprintf("资产摘要无效：%s", canonical), ReadOnly: true}
		}
		if _, err := hex.DecodeString(asset.SHA256); err != nil {
			return &MutationError{Code: "MUTATION_ASSET_HASH_INVALID", Message: fmt.Sprintf("资产摘要不是十六进制：%s", canonical), ReadOnly: true}
		}
		if asset.Status != domain.StatusStable {
			return &MutationError{Code: "MUTATION_ASSET_NOT_STABLE", Message: fmt.Sprintf("资产 %s 当前状态为 %s", canonical, asset.Status), ReadOnly: true}
		}
	}
	return nil
}

// BuildMutationSnapshot 对资产排序后生成稳定摘要，保证预览和执行可绑定同一集合。
func BuildMutationSnapshot(ctx context.Context, adapterID string, request MutationRequest) (MutationSnapshot, error) {
	if strings.TrimSpace(adapterID) == "" {
		return MutationSnapshot{}, &MutationError{Code: "MUTATION_ADAPTER_MISSING", Message: "mutation adapter ID 不能为空", ReadOnly: true}
	}
	if err := ValidateMutationRequest(ctx, request); err != nil {
		return MutationSnapshot{}, err
	}
	assets := append([]MutationAsset(nil), request.Assets...)
	sort.Slice(assets, func(i, j int) bool {
		return filepath.ToSlash(assets[i].RelativePath) < filepath.ToSlash(assets[j].RelativePath)
	})
	for index := range assets {
		assets[index].RelativePath = filepath.ToSlash(filepath.Clean(filepath.FromSlash(assets[index].RelativePath)))
	}
	canonical := struct {
		Version        int             `json:"version"`
		AdapterID      string          `json:"adapter_id"`
		Root           string          `json:"root"`
		ConversationID string          `json:"conversation_id"`
		SourceID       string          `json:"source_id,omitempty"`
		AssetSetHash   string          `json:"asset_set_hash,omitempty"`
		SnapshotRef    string          `json:"snapshot_ref,omitempty"`
		SnapshotSHA256 string          `json:"snapshot_sha256,omitempty"`
		Assets         []MutationAsset `json:"assets"`
	}{1, adapterID, filepath.Clean(request.Root), request.ConversationID, request.SourceID, request.AssetSetHash, request.SnapshotRef, request.SnapshotSHA256, assets}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return MutationSnapshot{}, fmt.Errorf("生成 mutation 快照失败: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return MutationSnapshot{
		Version: 1, AdapterID: adapterID, Root: canonical.Root, ConversationID: request.ConversationID, SourceID: request.SourceID,
		AssetSetHash: request.AssetSetHash, SnapshotRef: request.SnapshotRef, SnapshotSHA256: request.SnapshotSHA256,
		Assets: assets, SnapshotHash: hex.EncodeToString(digest[:]),
	}, nil
}

// ValidateMutationSnapshot 在 Apply/Restore 前再次校验快照身份和资产摘要。
func ValidateMutationSnapshot(ctx context.Context, snapshot MutationSnapshot) error {
	request := MutationRequest{
		Root: snapshot.Root, ConversationID: snapshot.ConversationID, AssetSetHash: snapshot.AssetSetHash,
		SourceID:    snapshot.SourceID,
		SnapshotRef: snapshot.SnapshotRef, SnapshotSHA256: snapshot.SnapshotSHA256, Assets: snapshot.Assets,
	}
	if snapshot.Version != 1 {
		return &MutationError{Code: "MUTATION_SNAPSHOT_VERSION_UNSUPPORTED", Message: "mutation 快照版本不受支持", ReadOnly: true}
	}
	if strings.TrimSpace(snapshot.AdapterID) == "" || strings.TrimSpace(snapshot.SnapshotHash) == "" {
		return &MutationError{Code: "MUTATION_SNAPSHOT_IDENTITY_INVALID", Message: "mutation 快照缺少 adapter ID 或摘要", ReadOnly: true}
	}
	expected, err := BuildMutationSnapshot(ctx, snapshot.AdapterID, request)
	if err != nil {
		return err
	}
	if expected.SnapshotHash != snapshot.SnapshotHash {
		return &MutationError{Code: "MUTATION_SNAPSHOT_CHANGED", Message: "mutation 快照摘要已变化", ReadOnly: true}
	}
	return nil
}
