// 中文说明：持久化本地归档恢复的独立 journal；不保存归档密码、正文或临时明文。
package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-chronicle.local/internal/compat"
)

// RestorePhase 表示一次归档恢复的持久化阶段。
type RestorePhase string

const (
	RestorePhasePrepared     RestorePhase = "prepared"
	RestorePhaseFiles        RestorePhase = "files_restored"
	RestorePhaseControlPlane RestorePhase = "control_plane_restored"
	RestorePhaseCommitted    RestorePhase = "committed"
	RestorePhaseNeedsReview  RestorePhase = "needs_review"
)

// RestoreAsset 是恢复 journal 中保存的最小资产身份，不包含正文。
type RestoreAsset struct {
	RelativePath string `json:"relative_path"`
	TargetPath   string `json:"target_path"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
}

// RestoreJournalEntry 是归档恢复的独立状态；原删除 journal 只作为绑定来源保留路径。
type RestoreJournalEntry struct {
	Version            int                      `json:"version"`
	RestoreOperationID string                   `json:"restore_operation_id"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	Phase              RestorePhase             `json:"phase"`
	SourceJournalPath  string                   `json:"source_journal_path,omitempty"`
	SourceOperationID  string                   `json:"source_operation_id,omitempty"`
	ArchivePath        string                   `json:"archive_path"`
	TargetRoot         string                   `json:"target_root"`
	Plan               OperationPlan            `json:"plan"`
	Assets             []RestoreAsset           `json:"assets"`
	Mutation           *compat.MutationSnapshot `json:"control_plane_snapshot,omitempty"`
	ErrorCode          string                   `json:"error_code,omitempty"`
	ErrorMessage       string                   `json:"error_message,omitempty"`
	RecoveryNote       string                   `json:"recovery_note,omitempty"`
}

// NewRestoreOperationID 生成不会与删除 operation ID 混淆的恢复 ID。
func NewRestoreOperationID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("生成恢复操作 ID 失败: %w", err)
	}
	return "restore-" + hex.EncodeToString(random[:]), nil
}

// CreateRestoreJournal 创建新的 prepared 恢复 journal。
func CreateRestoreJournal(ctx context.Context, directory string, entry RestoreJournalEntry) (RestoreJournalEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return RestoreJournalEntry{}, "", err
	}
	if strings.TrimSpace(entry.RestoreOperationID) == "" || entry.ArchivePath == "" || entry.TargetRoot == "" || entry.Plan.OperationID == "" {
		return RestoreJournalEntry{}, "", fmt.Errorf("恢复 journal 缺少操作 ID、归档路径、目标根或来源计划")
	}
	path, err := restoreJournalPath(directory, entry.RestoreOperationID)
	if err != nil {
		return RestoreJournalEntry{}, "", err
	}
	now := time.Now().UTC()
	entry.Version = 1
	entry.CreatedAt = now
	entry.UpdatedAt = now
	entry.Phase = RestorePhasePrepared
	if entry.Assets == nil {
		entry.Assets = make([]RestoreAsset, 0)
	}
	if err := writeRestoreJournal(ctx, path, entry, true); err != nil {
		return RestoreJournalEntry{}, "", err
	}
	return entry, path, nil
}

// LoadRestoreJournal 读取并校验恢复 journal 的基础身份。
func LoadRestoreJournal(ctx context.Context, path string) (RestoreJournalEntry, error) {
	if err := ctx.Err(); err != nil {
		return RestoreJournalEntry{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RestoreJournalEntry{}, err
	}
	var entry RestoreJournalEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return RestoreJournalEntry{}, fmt.Errorf("解析恢复 journal 失败: %w", err)
	}
	if entry.Version != 1 || strings.TrimSpace(entry.RestoreOperationID) == "" || entry.ArchivePath == "" || entry.TargetRoot == "" || entry.Plan.OperationID == "" {
		return RestoreJournalEntry{}, fmt.Errorf("恢复 journal 版本或身份无效")
	}
	return entry, nil
}

// TransitionRestoreJournal 原子更新恢复阶段和可选错误说明。
func TransitionRestoreJournal(ctx context.Context, path string, phase RestorePhase, errorCode, errorMessage, recoveryNote string) (RestoreJournalEntry, error) {
	entry, err := LoadRestoreJournal(ctx, path)
	if err != nil {
		return RestoreJournalEntry{}, err
	}
	if !validRestorePhaseTransition(entry.Phase, phase) {
		return RestoreJournalEntry{}, fmt.Errorf("禁止恢复 journal 阶段从 %s 转为 %s", entry.Phase, phase)
	}
	entry.Phase = phase
	entry.ErrorCode = errorCode
	entry.ErrorMessage = errorMessage
	entry.RecoveryNote = recoveryNote
	entry.UpdatedAt = time.Now().UTC()
	if err := writeRestoreJournal(ctx, path, entry, false); err != nil {
		return RestoreJournalEntry{}, err
	}
	return entry, nil
}

// RestoreNeedsRecovery 返回尚未进入终态的恢复 journal。
func RestoreNeedsRecovery(entry RestoreJournalEntry) bool {
	return entry.Phase != RestorePhaseCommitted
}

func restoreJournalPath(directory, operationID string) (string, error) {
	if strings.TrimSpace(directory) == "" || strings.TrimSpace(operationID) == "" || filepath.Base(operationID) != operationID || strings.ContainsAny(operationID, `/\\`) || strings.ContainsRune(operationID, 0) {
		return "", fmt.Errorf("恢复 journal 目录或操作 ID 无效")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("解析恢复 journal 目录失败: %w", err)
	}
	for _, part := range strings.Split(filepath.Clean(abs), string(filepath.Separator)) {
		if strings.EqualFold(part, ".codex") {
			return "", fmt.Errorf("拒绝把恢复 journal 放入原始 .codex 目录")
		}
	}
	if err := ensureJournalDirectory(abs); err != nil {
		return "", err
	}
	return filepath.Join(abs, "restore-"+operationID+".json"), nil
}

func writeRestoreJournal(ctx context.Context, path string, entry RestoreJournalEntry, exclusive bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化恢复 journal 失败: %w", err)
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if exclusive {
			return fmt.Errorf("恢复 journal 已存在: %s", path)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("恢复 journal 目标不是普通文件: %s", path)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("检查恢复 journal 目标失败: %w", statErr)
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("创建恢复 journal 临时文件失败: %w", err)
	}
	tmp := file.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("写入恢复 journal 失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("同步恢复 journal 失败: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭恢复 journal 失败: %w", err)
	}
	if exclusive {
		if _, err := os.Lstat(path); err == nil {
			cleanup()
			return fmt.Errorf("恢复 journal 已存在: %s", path)
		} else if !os.IsNotExist(err) {
			cleanup()
			return fmt.Errorf("检查恢复 journal 目标失败: %w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("发布恢复 journal 失败: %w", err)
	}
	if err := syncJournalDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("同步恢复 journal 目录失败: %w", err)
	}
	return nil
}

func validRestorePhaseTransition(from, to RestorePhase) bool {
	if from == to {
		return true
	}
	switch from {
	case RestorePhasePrepared:
		return to == RestorePhaseFiles || to == RestorePhaseNeedsReview
	case RestorePhaseFiles:
		return to == RestorePhaseControlPlane || to == RestorePhaseCommitted || to == RestorePhaseNeedsReview
	case RestorePhaseControlPlane:
		return to == RestorePhaseCommitted || to == RestorePhaseNeedsReview
	case RestorePhaseNeedsReview:
		return to == RestorePhaseFiles || to == RestorePhaseControlPlane || to == RestorePhaseCommitted || to == RestorePhaseNeedsReview
	default:
		return false
	}
}
