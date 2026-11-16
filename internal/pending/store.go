// 中文说明：持久化运行时生命周期意图、资产指纹和人工确认状态；文件位于原始 .codex 目录之外。
package pending

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

// State 表示暂存操作所处的人工确认阶段。
type State string

const (
	StatePending                  State = "pending"
	StateWaitingForIdle           State = "waiting_for_idle"
	StateIdleDetected             State = "idle_detected"
	StateConfirmationRequired     State = "confirmation_required"
	StateRevalidationFailed       State = "revalidation_failed"
	StateReadyForFixtureExecution State = "ready_for_fixture_execution"
	StateNeedsReview              State = "needs_review"
	StateCompleted                State = "completed"
	StateCancelled                State = "cancelled"
)

// Strategy 是用户选择的生命周期动作；不包含密码或确认数字明文。
type Strategy struct {
	Archive         bool `json:"archive"`
	Encrypted       bool `json:"encrypted"`
	Trash           bool `json:"trash"`
	PermanentDelete bool `json:"permanent_delete"`
}

// SourceAsset 是暂存时捕获的源文件身份和完整内容指纹。
type SourceAsset struct {
	RelativePath     string           `json:"relative_path"`
	Kind             domain.AssetKind `json:"kind"`
	SizeBytes        int64            `json:"size_bytes"`
	ModTime          time.Time        `json:"mod_time"`
	SHA256           string           `json:"sha256,omitempty"`
	ControlPlaneOnly bool             `json:"control_plane_only,omitempty"`
}

// Operation 是可在 daemon 重启后恢复的用户意图；正文和敏感凭证均不保存在这里。
type Operation struct {
	ID                  string                   `json:"operation_id"`
	Workspace           string                   `json:"workspace"`
	SourceRoot          string                   `json:"source_root"`
	ConversationID      string                   `json:"conversation_id"`
	Kind                string                   `json:"kind,omitempty"`
	AssetSet            []SourceAsset            `json:"asset_set"`
	AssetSetHash        string                   `json:"asset_set_hash"`
	Strategy            Strategy                 `json:"strategy"`
	CreatedAt           time.Time                `json:"created_at"`
	ExpiresAt           time.Time                `json:"expires_at,omitempty"`
	State               State                    `json:"state"`
	BlockReasons        []string                 `json:"block_reasons,omitempty"`
	LastProbeAt         time.Time                `json:"last_probe_at,omitempty"`
	LastReconfirmedAt   time.Time                `json:"last_reconfirmed_at,omitempty"`
	ProbeWindowCount    int                      `json:"probe_window_count,omitempty"`
	RequiredWindowCount int                      `json:"required_window_count"`
	LastActivity        *activity.Report         `json:"last_activity,omitempty"`
	Preview             *lifecycle.OperationPlan `json:"preview,omitempty"`
	UpdatedAt           time.Time                `json:"updated_at"`
}

// IsTerminal 判断是否已经不能再自动推进。
func (operation Operation) IsTerminal() bool {
	return operation.State == StateCompleted || operation.State == StateCancelled
}

// IsExpired 判断是否超过人工审核有效期。
func (operation Operation) IsExpired(now time.Time) bool {
	return !operation.ExpiresAt.IsZero() && now.UTC().After(operation.ExpiresAt)
}

// CanTransition 检查暂存状态是否允许沿着安全状态机推进。
func (operation Operation) CanTransition(next State) bool {
	if operation.State == next {
		return true
	}
	allowed := map[State]map[State]bool{
		StatePending: {
			StateWaitingForIdle: true, StateIdleDetected: true, StateConfirmationRequired: true,
			StateNeedsReview: true, StateCancelled: true,
		},
		StateWaitingForIdle: {
			StateWaitingForIdle: true, StateIdleDetected: true, StateConfirmationRequired: true,
			StateRevalidationFailed: true, StateNeedsReview: true, StateCancelled: true,
		},
		StateIdleDetected: {
			StateIdleDetected: true, StateConfirmationRequired: true, StateWaitingForIdle: true,
			StateRevalidationFailed: true, StateNeedsReview: true, StateCancelled: true,
		},
		StateConfirmationRequired: {
			StateConfirmationRequired: true, StateReadyForFixtureExecution: true,
			StateWaitingForIdle: true, StateRevalidationFailed: true, StateNeedsReview: true, StateCancelled: true,
		},
		StateRevalidationFailed: {
			StateRevalidationFailed: true, StateWaitingForIdle: true, StateIdleDetected: true,
			StateConfirmationRequired: true, StateNeedsReview: true, StateCancelled: true,
		},
		StateReadyForFixtureExecution: {
			StateReadyForFixtureExecution: true, StateWaitingForIdle: true, StateRevalidationFailed: true,
			StateCompleted: true, StateNeedsReview: true, StateCancelled: true,
		},
		StateNeedsReview: {
			StateNeedsReview: true, StateWaitingForIdle: true, StateIdleDetected: true,
			StateConfirmationRequired: true, StateRevalidationFailed: true, StateCancelled: true,
		},
		StateCompleted: {StateCompleted: true},
		StateCancelled: {StateCancelled: true},
	}
	return allowed[operation.State][next]
}

// Store 使用每个操作一个 JSON 文件，原子替换保证突然终止不会留下半份主记录。
type Store struct {
	root string
	mu   sync.Mutex
}

// NewStore 创建不主动写盘的 pending 存储。root 必须位于源 .codex 之外。
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("pending 存储目录不能为空")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("解析 pending 存储目录失败: %w", err)
	}
	return &Store{root: abs}, nil
}

// DefaultRoot 为工作区生成稳定的、位于源根外部的 pending 目录。
func DefaultRoot(sourceRoot, archivePath string) string {
	base := strings.TrimSpace(archivePath)
	if base != "" && base != ":memory:" {
		archiveDir := filepath.Dir(filepath.Clean(base))
		if !pathWithinOrEqual(sourceRoot, archiveDir) {
			return filepath.Join(archiveDir, "pending-operations")
		}
	}
	abs, err := filepath.Abs(filepath.Clean(sourceRoot))
	if err != nil {
		abs = filepath.Clean(sourceRoot)
	}
	digest := sha256.Sum256([]byte(abs))
	return filepath.Join(filepath.Dir(abs), ".codex-chronicle-pending-"+hex.EncodeToString(digest[:8]))
}

// Root 返回独立 pending 目录，用于资源预算和诊断。
func (store *Store) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

// Save 原子保存操作；取消操作也保留 JSON 记录，不删除用户的审核证据。
func (store *Store) Save(ctx context.Context, operation Operation) error {
	if store == nil {
		return fmt.Errorf("pending 存储未初始化")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(operation.ID) {
		return fmt.Errorf("pending operation ID 无效")
	}
	if !operation.CanTransition(operation.State) && operation.State != StatePending {
		return fmt.Errorf("pending 操作状态转换非法: %s", operation.State)
	}
	if operation.CreatedAt.IsZero() {
		operation.CreatedAt = time.Now().UTC()
	}
	operation.UpdatedAt = time.Now().UTC()
	operation.AssetSet = append([]SourceAsset(nil), operation.AssetSet...)
	operation.BlockReasons = append([]string(nil), operation.BlockReasons...)
	data, err := json.MarshalIndent(operation, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 pending 操作失败: %w", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return fmt.Errorf("创建 pending 存储目录失败: %w", err)
	}
	if existingData, readErr := os.ReadFile(store.path(operation.ID)); readErr == nil {
		var existing Operation
		if unmarshalErr := json.Unmarshal(existingData, &existing); unmarshalErr != nil {
			return fmt.Errorf("解析已有 pending 操作失败: %w", unmarshalErr)
		}
		if existing.State != "" && !existing.CanTransition(operation.State) {
			return fmt.Errorf("pending 操作状态转换非法: %s -> %s", existing.State, operation.State)
		}
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("读取已有 pending 操作失败: %w", readErr)
	}
	tmp, err := os.CreateTemp(store.root, ".pending-*.tmp")
	if err != nil {
		return fmt.Errorf("创建 pending 临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置 pending 临时文件权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入 pending 操作失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("持久化 pending 操作失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭 pending 临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, store.path(operation.ID)); err != nil {
		return fmt.Errorf("原子提交 pending 操作失败: %w", err)
	}
	return nil
}

// NewOperation 创建带随机 ID 的初始意图。
func NewOperation(workspace, sourceRoot, conversationID string, strategy Strategy) (Operation, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Operation{}, fmt.Errorf("生成 pending operation ID 失败: %w", err)
	}
	now := time.Now().UTC()
	return Operation{
		ID: stringID("pending-", random[:]), Workspace: workspace, SourceRoot: sourceRoot,
		ConversationID: conversationID, Strategy: strategy, CreatedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour),
		State: StatePending, RequiredWindowCount: 3, AssetSet: make([]SourceAsset, 0), BlockReasons: make([]string, 0),
	}, nil
}

// Get 读取单个暂存操作。
func (store *Store) Get(ctx context.Context, id string) (Operation, error) {
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	if !validID(id) {
		return Operation{}, fmt.Errorf("pending operation ID 无效")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	data, err := os.ReadFile(store.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return Operation{}, fmt.Errorf("pending 操作不存在: %s", id)
		}
		return Operation{}, fmt.Errorf("读取 pending 操作失败: %w", err)
	}
	var operation Operation
	if err := json.Unmarshal(data, &operation); err != nil {
		return Operation{}, fmt.Errorf("解析 pending 操作失败: %w", err)
	}
	return operation, nil
}

// List 返回全部可解析的暂存操作，按最近更新时间倒序排列。
func (store *Store) List(ctx context.Context) ([]Operation, error) {
	if store == nil {
		return nil, fmt.Errorf("pending 存储未初始化")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := os.ReadDir(store.root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Operation{}, nil
		}
		return nil, fmt.Errorf("读取 pending 存储目录失败: %w", err)
	}
	items := make([]Operation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(store.root, entry.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("读取 pending 文件 %s 失败: %w", entry.Name(), readErr)
		}
		var operation Operation
		if unmarshalErr := json.Unmarshal(data, &operation); unmarshalErr != nil {
			return nil, fmt.Errorf("解析 pending 文件 %s 失败: %w", entry.Name(), unmarshalErr)
		}
		items = append(items, operation)
	}
	sort.SliceStable(items, func(left, right int) bool {
		return items[left].UpdatedAt.After(items[right].UpdatedAt)
	})
	return items, nil
}

func (store *Store) path(id string) string { return filepath.Join(store.root, id+".json") }

// HashAssetSet 为没有 canonical 资产图哈希的兼容版本生成稳定资产集合哈希。
func HashAssetSet(assets []SourceAsset) string {
	copyAssets := append([]SourceAsset(nil), assets...)
	sort.Slice(copyAssets, func(left, right int) bool { return copyAssets[left].RelativePath < copyAssets[right].RelativePath })
	hash := sha256.New()
	for _, asset := range copyAssets {
		_, _ = hash.Write([]byte(asset.RelativePath))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(asset.Kind))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(asset.SHA256))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validID(id string) bool {
	return strings.HasPrefix(id, "pending-") && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`) && len(id) <= 96
}

func stringID(prefix string, bytes []byte) string { return prefix + hex.EncodeToString(bytes) }

func pathWithin(root, candidate string) bool {
	rootAbs, rootErr := filepath.Abs(filepath.Clean(root))
	candidateAbs, candidateErr := filepath.Abs(filepath.Clean(candidate))
	if rootErr != nil || candidateErr != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func pathWithinOrEqual(root, candidate string) bool {
	rootAbs, rootErr := filepath.Abs(filepath.Clean(root))
	candidateAbs, candidateErr := filepath.Abs(filepath.Clean(candidate))
	if rootErr != nil || candidateErr != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
