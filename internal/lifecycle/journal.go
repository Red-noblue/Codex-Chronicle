// 中文说明：持久化生命周期操作阶段和补偿信息；只写本项目指定的 journal 目录，不写源 .codex。
package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-chronicle.local/internal/compat"
)

// JournalPhase 表示一次操作 saga 的持久化阶段。
type JournalPhase string

const (
	PhasePrepared      JournalPhase = "prepared"
	PhaseArchived      JournalPhase = "archived"
	PhaseIsolated      JournalPhase = "isolated"
	PhaseSourceRemoved JournalPhase = "source_removed"
	PhaseControlDB     JournalPhase = "control_db_updated"
	PhaseCommitted     JournalPhase = "committed"
	PhaseRolledBack    JournalPhase = "rolled_back"
	PhaseNeedsReview   JournalPhase = "needs_review"
)

// CommitStatus 描述 journal 对外部副作用的最终判断；未知状态不能被当作可自动重试。
type CommitStatus string

const (
	CommitPending         CommitStatus = "pending"
	CommitCommitted       CommitStatus = "committed"
	CommitRolledBack      CommitStatus = "rolled_back"
	CommitNeedsReview     CommitStatus = "needs_review"
	CommitExternalUnknown CommitStatus = "external_state_unknown"
)

// StageRecord 记录一个阶段的输入、输出和 staging 位置；不保存正文内容。
type StageRecord struct {
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Path         string    `json:"path,omitempty"`
	InputSHA256  string    `json:"input_sha256,omitempty"`
	OutputSHA256 string    `json:"output_sha256,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
	Note         string    `json:"note,omitempty"`
}

// JournalEntry 是恢复流程使用的完整操作状态，不包含正文。
type JournalEntry struct {
	Version                    int                      `json:"version"`
	OperationID                string                   `json:"operation_id"`
	CreatedAt                  time.Time                `json:"created_at"`
	UpdatedAt                  time.Time                `json:"updated_at"`
	Phase                      JournalPhase             `json:"phase"`
	CommitStatus               CommitStatus             `json:"commit_status"`
	InputPlanSHA256            string                   `json:"input_plan_sha256"`
	InputAssetSetHash          string                   `json:"input_asset_set_hash,omitempty"`
	Plan                       OperationPlan            `json:"plan"`
	Stages                     []StageRecord            `json:"stages,omitempty"`
	Receipts                   []ReceiptRecord          `json:"receipts,omitempty"`
	ArchivePath                string                   `json:"archive_path,omitempty"`
	ArchiveManifestSHA256      string                   `json:"archive_manifest_sha256,omitempty"`
	RemoteArchiveID            string                   `json:"remote_archive_id,omitempty"`
	RemoteArchiveManifestPath  string                   `json:"remote_archive_manifest_path,omitempty"`
	ErrorCode                  string                   `json:"error_code,omitempty"`
	ErrorMessage               string                   `json:"error_message,omitempty"`
	RecoveryNote               string                   `json:"recovery_note,omitempty"`
	Mutation                   *compat.MutationSnapshot `json:"control_plane_snapshot,omitempty"`
	ControlPlaneSnapshotSHA256 string                   `json:"control_plane_snapshot_sha256,omitempty"`
}

// PlanDigest 计算计划的稳定摘要，用于 journal 输入绑定和重启后的完整性检查。
func PlanDigest(plan OperationPlan) (string, error) {
	data, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("序列化操作计划失败: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// AttachArchiveReceipt 记录本地归档和可选远端归档位置；不保存凭证或正文。
func AttachArchiveReceipt(ctx context.Context, path, archivePath, remoteArchiveID, remoteManifestPath string) (JournalEntry, error) {
	entry, err := LoadJournal(ctx, path)
	if err != nil {
		return JournalEntry{}, err
	}
	if entry.Phase == PhaseCommitted || entry.Phase == PhaseRolledBack {
		return JournalEntry{}, fmt.Errorf("终态 journal 不可绑定归档回执")
	}
	if strings.TrimSpace(archivePath) == "" && strings.TrimSpace(remoteArchiveID) == "" {
		return JournalEntry{}, fmt.Errorf("归档回执不能为空")
	}
	if strings.TrimSpace(archivePath) != "" {
		entry.ArchivePath = archivePath
		if manifestHash, hashErr := hashRegularFile(ctx, filepath.Join(archivePath, "manifest.json")); hashErr == nil {
			entry.ArchiveManifestSHA256 = manifestHash
		} else {
			return JournalEntry{}, fmt.Errorf("计算归档 manifest 摘要失败: %w", hashErr)
		}
	}
	if strings.TrimSpace(remoteArchiveID) != "" {
		entry.RemoteArchiveID = remoteArchiveID
	}
	if strings.TrimSpace(remoteManifestPath) != "" {
		entry.RemoteArchiveManifestPath = remoteManifestPath
	}
	entry.UpdatedAt = time.Now().UTC()
	if err := writeJournal(ctx, path, entry, false); err != nil {
		return JournalEntry{}, err
	}
	return entry, nil
}

// AttachMutationSnapshot 将 control-plane adapter 的预写快照绑定到 prepared journal。
func AttachMutationSnapshot(ctx context.Context, path string, snapshot compat.MutationSnapshot) (JournalEntry, error) {
	entry, err := LoadJournal(ctx, path)
	if err != nil {
		return JournalEntry{}, err
	}
	if entry.Phase != PhasePrepared {
		return JournalEntry{}, fmt.Errorf("只有 prepared journal 可以绑定 control-plane 快照")
	}
	if snapshot.Version != 1 || strings.TrimSpace(snapshot.AdapterID) == "" || strings.TrimSpace(snapshot.SnapshotHash) == "" {
		return JournalEntry{}, fmt.Errorf("control-plane 快照身份无效")
	}
	entry.Mutation = &snapshot
	entry.ControlPlaneSnapshotSHA256 = snapshot.SnapshotHash
	entry.UpdatedAt = time.Now().UTC()
	if err := writeJournal(ctx, path, entry, false); err != nil {
		return JournalEntry{}, err
	}
	return entry, nil
}

// ReceiptRecord 将 provider 回执保存为通用 JSON，避免 journal 依赖平台实现。
type ReceiptRecord struct {
	RelativePath string `json:"relative_path"`
	OriginalPath string `json:"original_path"`
	IsolatedPath string `json:"isolated_path"`
	Provider     string `json:"provider"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	Identity     string `json:"identity,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
}

// CreateJournal 创建新的 prepared journal；目标目录必须是本项目外部控制面目录。
func CreateJournal(ctx context.Context, directory string, plan OperationPlan) (JournalEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return JournalEntry{}, "", err
	}
	if plan.OperationID == "" || plan.Status == PlanInvalid {
		return JournalEntry{}, "", fmt.Errorf("无效操作计划不能创建 journal")
	}
	path, err := journalPath(directory, plan.OperationID)
	if err != nil {
		return JournalEntry{}, "", err
	}
	planHash, err := PlanDigest(plan)
	if err != nil {
		return JournalEntry{}, "", err
	}
	entry := JournalEntry{
		Version: 1, OperationID: plan.OperationID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Phase: PhasePrepared, CommitStatus: CommitPending, InputPlanSHA256: planHash, InputAssetSetHash: plan.AssetSetHash,
		Plan: plan, Stages: make([]StageRecord, 0), Receipts: make([]ReceiptRecord, 0),
	}
	if err := writeJournal(ctx, path, entry, true); err != nil {
		return JournalEntry{}, "", err
	}
	return entry, path, nil
}

// TransitionJournal 原子更新阶段和可选错误/恢复说明。
func TransitionJournal(ctx context.Context, path string, phase JournalPhase, errorCode, errorMessage, recoveryNote string) (JournalEntry, error) {
	entry, err := LoadJournal(ctx, path)
	if err != nil {
		return JournalEntry{}, err
	}
	if !validPhaseTransition(entry.Phase, phase) {
		return JournalEntry{}, fmt.Errorf("禁止 journal 阶段从 %s 转为 %s", entry.Phase, phase)
	}
	entry.Phase = phase
	entry.CommitStatus = commitStatusForPhase(phase)
	entry.UpdatedAt = time.Now().UTC()
	entry.ErrorCode = errorCode
	entry.ErrorMessage = errorMessage
	entry.RecoveryNote = recoveryNote
	if err := writeJournal(ctx, path, entry, false); err != nil {
		return JournalEntry{}, err
	}
	return entry, nil
}

// RecordStage 原子记录阶段的 staging 路径和输入/输出摘要；同名阶段会被更新而不会重复追加。
func RecordStage(ctx context.Context, path string, stage StageRecord) (JournalEntry, error) {
	entry, err := LoadJournal(ctx, path)
	if err != nil {
		return JournalEntry{}, err
	}
	if strings.TrimSpace(stage.Name) == "" || strings.TrimSpace(stage.State) == "" {
		return JournalEntry{}, fmt.Errorf("journal 阶段记录必须包含名称和状态")
	}
	if stage.StartedAt.IsZero() {
		stage.StartedAt = time.Now().UTC()
	}
	updated := false
	for index := range entry.Stages {
		if entry.Stages[index].Name == stage.Name {
			entry.Stages[index] = stage
			updated = true
			break
		}
	}
	if !updated {
		entry.Stages = append(entry.Stages, stage)
	}
	entry.UpdatedAt = time.Now().UTC()
	if err := writeJournal(ctx, path, entry, false); err != nil {
		return JournalEntry{}, err
	}
	return entry, nil
}

// AppendReceipt 原子追加一个隔离/回收站回执。
func AppendReceipt(ctx context.Context, path string, receipt ReceiptRecord) (JournalEntry, error) {
	entry, err := LoadJournal(ctx, path)
	if err != nil {
		return JournalEntry{}, err
	}
	if entry.Phase == PhaseCommitted || entry.Phase == PhaseRolledBack {
		return JournalEntry{}, fmt.Errorf("终态 journal 不可追加回执")
	}
	entry.Receipts = append(entry.Receipts, receipt)
	entry.UpdatedAt = time.Now().UTC()
	if err := writeJournal(ctx, path, entry, false); err != nil {
		return JournalEntry{}, err
	}
	return entry, nil
}

// LoadJournal 读取并校验 journal 版本和操作 ID。
func LoadJournal(ctx context.Context, path string) (JournalEntry, error) {
	if err := ctx.Err(); err != nil {
		return JournalEntry{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return JournalEntry{}, err
	}
	var entry JournalEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return JournalEntry{}, fmt.Errorf("解析 journal 失败: %w", err)
	}
	if entry.Version != 1 || strings.TrimSpace(entry.OperationID) == "" {
		return JournalEntry{}, fmt.Errorf("journal 版本或操作 ID 无效")
	}
	if entry.CommitStatus == "" {
		entry.CommitStatus = commitStatusForPhase(entry.Phase)
	}
	return entry, nil
}

// NeedsRecovery 返回未进入终态、需要启动恢复检查的 journal。
func NeedsRecovery(entry JournalEntry) bool {
	return entry.Phase != PhaseCommitted && entry.Phase != PhaseRolledBack
}

func commitStatusForPhase(phase JournalPhase) CommitStatus {
	switch phase {
	case PhaseCommitted:
		return CommitCommitted
	case PhaseRolledBack:
		return CommitRolledBack
	case PhaseNeedsReview:
		return CommitNeedsReview
	default:
		return CommitPending
	}
}

func hashRegularFile(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("目标不是普通文件: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func journalPath(directory, operationID string) (string, error) {
	if strings.TrimSpace(directory) == "" || strings.TrimSpace(operationID) == "" {
		return "", fmt.Errorf("journal 目录和操作 ID 不能为空")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	for _, part := range strings.Split(filepath.Clean(abs), string(filepath.Separator)) {
		if strings.EqualFold(part, ".codex") {
			return "", fmt.Errorf("拒绝把 journal 放入原始 .codex 目录")
		}
	}
	if err := ensureJournalDirectory(abs); err != nil {
		return "", err
	}
	return filepath.Join(abs, operationID+".json"), nil
}

func writeJournal(ctx context.Context, path string, entry JournalEntry, exclusive bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 journal 失败: %w", err)
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if exclusive {
			return fmt.Errorf("journal 已存在: %s", path)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("journal 目标不是普通文件: %s", path)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("检查 journal 目标失败: %w", statErr)
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("创建 journal 临时文件失败: %w", err)
	}
	tmp := file.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("写入 journal 失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("同步 journal 失败: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭 journal 失败: %w", err)
	}
	if exclusive {
		if _, err := os.Lstat(path); err == nil {
			cleanup()
			return fmt.Errorf("journal 已存在: %s", path)
		} else if !os.IsNotExist(err) {
			cleanup()
			return fmt.Errorf("检查 journal 目标失败: %w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("发布 journal 失败: %w", err)
	}
	if err := syncJournalDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("同步 journal 目录失败: %w", err)
	}
	return nil
}

func ensureJournalDirectory(path string) error {
	current := filepath.Clean(path)
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("journal 目录包含符号链接或非目录: %s", current)
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return fmt.Errorf("检查 journal 目录失败: %w", statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("创建 journal 目录失败: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("读取 journal 目录失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("journal 目录包含符号链接或非目录: %s", path)
	}
	return nil
}

func syncJournalDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		// Windows 和部分网络文件系统不允许打开目录；文件级 Sync 仍已执行。
		return nil
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return err
	}
	return nil
}

func validPhaseTransition(from, to JournalPhase) bool {
	if from == to {
		return true
	}
	switch from {
	case PhasePrepared:
		return to == PhaseArchived || to == PhaseIsolated || to == PhaseSourceRemoved || to == PhaseRolledBack || to == PhaseNeedsReview
	case PhaseArchived:
		return to == PhaseIsolated || to == PhaseSourceRemoved || to == PhaseCommitted || to == PhaseRolledBack || to == PhaseNeedsReview
	case PhaseIsolated:
		return to == PhaseControlDB || to == PhaseSourceRemoved || to == PhaseRolledBack || to == PhaseNeedsReview
	case PhaseSourceRemoved:
		return to == PhaseControlDB || to == PhaseCommitted || to == PhaseNeedsReview
	case PhaseControlDB:
		return to == PhaseCommitted || to == PhaseRolledBack || to == PhaseNeedsReview
	case PhaseNeedsReview:
		return to == PhaseRolledBack || to == PhaseCommitted || to == PhaseNeedsReview
	default:
		return false
	}
}
