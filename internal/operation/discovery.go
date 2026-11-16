// 中文说明：启动时发现并分类未完成生命周期 journal；分类只依据持久化证据，不猜测外部副作用。
package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"codex-chronicle.local/internal/lifecycle"
)

// RecoveryDisposition 是重启后允许展示给用户的恢复结论。
type RecoveryDisposition string

const (
	RecoveryNotStarted      RecoveryDisposition = "not_started"
	RecoveryRetryable       RecoveryDisposition = "retryable"
	RecoveryRollbackable    RecoveryDisposition = "rollbackable"
	RecoveryCommitted       RecoveryDisposition = "committed"
	RecoveryRolledBack      RecoveryDisposition = "rolled_back"
	RecoveryExternalUnknown RecoveryDisposition = "external_state_unknown"
	RecoveryNeedsReview     RecoveryDisposition = "needs_review"
)

// RecoveryCandidate 包含恢复 UI 所需的最小 journal 证据，不包含正文。
type RecoveryCandidate struct {
	JournalPath          string                 `json:"journal_path"`
	Entry                lifecycle.JournalEntry `json:"entry"`
	Disposition          RecoveryDisposition    `json:"disposition"`
	CanRetry             bool                   `json:"can_retry"`
	CanRollback          bool                   `json:"can_rollback"`
	ExternalStateUnknown bool                   `json:"external_state_unknown"`
	Reason               string                 `json:"reason,omitempty"`
}

// ClassifyJournal 根据阶段、摘要和可见外部对象判断重启后的安全边界。
func ClassifyJournal(ctx context.Context, journalPath string, entry lifecycle.JournalEntry) RecoveryCandidate {
	candidate := RecoveryCandidate{JournalPath: journalPath, Entry: entry, Disposition: RecoveryNeedsReview}
	if digest, err := lifecycle.PlanDigest(entry.Plan); err != nil || (entry.InputPlanSHA256 != "" && digest != entry.InputPlanSHA256) {
		candidate.Reason = "journal 中的输入计划摘要无法与计划本体匹配"
		return candidate
	}
	if entry.Mutation != nil && entry.ControlPlaneSnapshotSHA256 != "" && entry.Mutation.SnapshotHash != entry.ControlPlaneSnapshotSHA256 {
		candidate.Reason = "control-plane 快照摘要不一致"
		return candidate
	}
	if err := ctx.Err(); err != nil {
		candidate.Reason = err.Error()
		return candidate
	}
	for _, stage := range entry.Stages {
		if (stage.Name == "archive" || stage.Name == "encrypted_archive") && stage.State == "started" && strings.TrimSpace(stage.Path) != "" {
			staging := filepath.Join(filepath.Dir(stage.Path), "."+filepath.Base(stage.Path)+".tmp")
			if pathExists(stage.Path) || pathExists(staging) {
				candidate.Disposition = RecoveryExternalUnknown
				candidate.ExternalStateUnknown = true
				candidate.Reason = "归档 staging 或最终目录已存在，但 journal 尚未收到完整归档回执"
				return candidate
			}
		}
	}
	switch entry.Phase {
	case lifecycle.PhaseCommitted:
		candidate.Disposition = RecoveryCommitted
		candidate.Reason = "journal 已明确提交，不自动撤销外部副作用"
	case lifecycle.PhaseRolledBack:
		candidate.Disposition = RecoveryRolledBack
		candidate.Reason = "journal 已明确回滚"
	case lifecycle.PhasePrepared:
		if len(entry.Receipts) > 0 {
			candidate.Disposition = RecoveryRollbackable
			candidate.CanRollback = true
			candidate.Reason = "prepared journal 已有回收站回执，可先按逆序补偿"
		} else if entry.ArchivePath == "" && entry.Mutation == nil {
			candidate.Disposition = RecoveryNotStarted
			candidate.CanRetry = true
			candidate.Reason = "只有 prepared journal，尚未发现外部副作用"
		} else {
			candidate.Disposition = RecoveryRetryable
			candidate.CanRetry = true
			candidate.Reason = "准备阶段已留下快照，可重新验证后重试"
		}
	case lifecycle.PhaseArchived:
		if archiveReceiptValid(entry) {
			candidate.Disposition = RecoveryRollbackable
			candidate.CanRollback = true
			candidate.Reason = "归档回执和 manifest 可见，源资产尚未确认移动"
		} else {
			candidate.Disposition = RecoveryExternalUnknown
			candidate.ExternalStateUnknown = true
			candidate.Reason = "归档路径或 manifest 回执不可验证"
		}
	case lifecycle.PhaseIsolated, lifecycle.PhaseControlDB:
		if len(entry.Receipts) > 0 {
			candidate.Disposition = RecoveryRollbackable
			candidate.CanRollback = true
			if entry.Mutation != nil {
				candidate.Reason = "回收站回执和 control-plane 快照均存在，可按逆序补偿"
			} else {
				candidate.Reason = "回收站回执存在，可先按逆序恢复文件；控制面未绑定快照"
			}
		} else {
			candidate.Disposition = RecoveryExternalUnknown
			candidate.ExternalStateUnknown = true
			candidate.Reason = "隔离或控制面阶段缺少完整补偿证据"
		}
	case lifecycle.PhaseSourceRemoved:
		candidate.Disposition = RecoveryNeedsReview
		candidate.Reason = "源文件已移除，当前 journal 没有自动可逆凭据"
	case lifecycle.PhaseNeedsReview:
		if len(entry.Receipts) > 0 {
			candidate.Disposition = RecoveryRollbackable
			candidate.CanRollback = true
			candidate.Reason = "人工复核阶段仍保留回收站回执，可先执行可验证的文件补偿"
		} else {
			candidate.Disposition = RecoveryNeedsReview
		}
		if entry.RecoveryNote != "" {
			candidate.Reason = entry.RecoveryNote
		} else if entry.ErrorMessage != "" {
			candidate.Reason = entry.ErrorMessage
		} else {
			candidate.Reason = "操作已进入人工复核"
		}
	default:
		candidate.Disposition = RecoveryNeedsReview
		candidate.Reason = fmt.Sprintf("未知 journal 阶段：%s", entry.Phase)
	}
	return candidate
}

// DiscoverJournals 在 daemon 启动或 UI 刷新时扫描独立 journal 目录；不会创建、修改或删除文件。
func DiscoverJournals(ctx context.Context, directory string) ([]RecoveryCandidate, []string) {
	candidates := make([]RecoveryCandidate, 0)
	errors := make([]string, 0)
	if strings.TrimSpace(directory) == "" {
		return candidates, errors
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return candidates, errors
		}
		return candidates, []string{fmt.Sprintf("读取 journal 目录失败：%v", err)}
	}
	for _, item := range entries {
		if item.IsDir() || filepath.Ext(item.Name()) != ".json" {
			continue
		}
		path := filepath.Join(directory, item.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			errors = append(errors, fmt.Sprintf("读取 %s 失败：%v", item.Name(), statErr))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			errors = append(errors, fmt.Sprintf("跳过非普通 journal：%s", item.Name()))
			continue
		}
		entry, loadErr := lifecycle.LoadJournal(ctx, path)
		if loadErr != nil {
			errors = append(errors, fmt.Sprintf("解析 %s 失败：%v", item.Name(), loadErr))
			continue
		}
		candidates = append(candidates, ClassifyJournal(ctx, path, entry))
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Entry.UpdatedAt.After(candidates[j].Entry.UpdatedAt) })
	return candidates, errors
}

func archiveReceiptValid(entry lifecycle.JournalEntry) bool {
	if strings.TrimSpace(entry.ArchivePath) == "" {
		return false
	}
	manifest := filepath.Join(entry.ArchivePath, "manifest.json")
	info, err := os.Lstat(manifest)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false
	}
	if entry.ArchiveManifestSHA256 == "" {
		return true
	}
	digest, err := hashFile(manifest)
	return err == nil && digest == entry.ArchiveManifestSHA256
}

func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Lstat(path)
	return err == nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
