// 中文说明：恢复未完成的本地生命周期 journal；恢复只处理可验证的回收站回执，绝不覆盖新文件。
package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/platform"
)

// RecoverLocalSaga 将未完成的回收站隔离操作按逆序恢复到原始根目录。
// 归档目录不会被自动删除；永久删除阶段没有可逆凭据时只返回人工复核错误。
func RecoverLocalSaga(ctx context.Context, journalPath string, trash platform.TrashProvider) (SagaResult, error) {
	return RecoverLocalSagaWithOptions(ctx, journalPath, trash, nil)
}

// RecoverLocalSagaWithOptions 恢复回收站文件，并在 journal 已进入控制面阶段时恢复 SQLite 行。
func RecoverLocalSagaWithOptions(ctx context.Context, journalPath string, trash platform.TrashProvider, mutation compat.ControlPlaneMutation) (SagaResult, error) {
	entry, err := lifecycle.LoadJournal(ctx, journalPath)
	if err != nil {
		return SagaResult{}, err
	}
	result := SagaResult{OperationID: entry.OperationID, JournalPath: journalPath, Phase: entry.Phase}
	if !lifecycle.NeedsRecovery(entry) {
		return result, nil
	}
	if entry.Phase == lifecycle.PhaseSourceRemoved {
		result.NeedsReview = true
		return result, fmt.Errorf("journal 已进入源文件删除阶段，缺少可逆副本，必须人工复核")
	}
	if entry.Mutation != nil {
		if mutation == nil {
			result.NeedsReview = true
			return result, fmt.Errorf("journal 包含 control-plane 快照但未提供恢复 adapter")
		}
		if mutation.Descriptor().ID != entry.Mutation.AdapterID {
			result.NeedsReview = true
			return result, fmt.Errorf("恢复 adapter 与 journal 快照不匹配")
		}
		if entry.ArchivePath != "" {
			if _, statErr := os.Lstat(entry.Mutation.SnapshotRef); os.IsNotExist(statErr) {
				if archiveErr := archive.EnsureMutationSnapshot(ctx, entry.ArchivePath, *entry.Mutation); archiveErr != nil {
					result.NeedsReview = true
					return result, fmt.Errorf("从归档补回 control-plane 快照失败: %w", archiveErr)
				}
			} else if statErr != nil {
				result.NeedsReview = true
				return result, fmt.Errorf("检查 control-plane 快照失败: %w", statErr)
			}
		}
	}
	if len(entry.Receipts) > 0 && trash == nil {
		result.NeedsReview = true
		return result, fmt.Errorf("journal 包含回收站回执但未提供恢复 provider")
	}
	for index := len(entry.Receipts) - 1; index >= 0; index-- {
		receipt := entry.Receipts[index]
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := restoreReceiptIdempotent(ctx, entry, receipt, trash); err != nil {
			_ = recordRecoveryFailure(journalPath, receipt, err)
			result.NeedsReview = true
			return result, fmt.Errorf("恢复 %s 失败: %w", receipt.RelativePath, err)
		}
	}
	if entry.Mutation != nil {
		mutationResult, restoreErr := mutation.Restore(ctx, *entry.Mutation)
		result.Mutation = &mutationResult
		if restoreErr != nil {
			result.NeedsReview = true
			return result, fmt.Errorf("恢复 control-plane 行失败: %w", restoreErr)
		}
	}
	updated, err := lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseRolledBack, "", "", "未完成操作已按回收站回执恢复；归档副本保留待人工处理")
	if err != nil {
		return result, err
	}
	result.Phase = updated.Phase
	return result, nil
}

// restoreReceiptIdempotent 在回收站移动和 journal 终态写入之间发生中断时，
// 允许确认“目标已恢复且内容仍与计划一致”的回执继续推进；任何未知或冲突状态仍然 fail-closed。
func restoreReceiptIdempotent(ctx context.Context, entry lifecycle.JournalEntry, receipt lifecycle.ReceiptRecord, trash platform.TrashProvider) error {
	target, err := safeRecoveryTarget(entry.Plan.Root, receipt.RelativePath)
	if err != nil {
		return err
	}
	trashPath, err := filepath.Abs(receipt.IsolatedPath)
	if err != nil || strings.TrimSpace(receipt.IsolatedPath) == "" {
		return fmt.Errorf("恢复回执路径无效: %s", receipt.RelativePath)
	}
	trashExists := false
	if info, statErr := os.Lstat(trashPath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("回收站回执不是普通文件: %s", receipt.RelativePath)
		}
		trashExists = true
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("检查回收站回执失败: %w", statErr)
	}
	targetExists := false
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("恢复目标不是普通文件: %s", receipt.RelativePath)
		}
		targetExists = true
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("检查恢复目标失败: %w", statErr)
	}
	if targetExists && !trashExists {
		asset, found := recoveryAsset(entry.Plan.Assets, receipt.RelativePath)
		expectedIdentity := receipt.Identity
		expectedSHA := receipt.SHA256
		if found {
			if expectedIdentity == "" {
				expectedIdentity = asset.Identity
			}
			if expectedSHA == "" {
				expectedSHA = asset.SHA256
			}
		}
		if expectedIdentity != "" && recoveryFileIdentity(target) != expectedIdentity {
			return fmt.Errorf("恢复目标是其他文件，即使内容可能相同: %s", receipt.RelativePath)
		}
		if validSHA256(expectedSHA) {
			digest, hashErr := hashRecoveryFile(ctx, target)
			if hashErr == nil && digest == expectedSHA {
				return nil
			}
		}
		return fmt.Errorf("恢复目标已存在但无法证明是本次操作恢复的内容: %s", receipt.RelativePath)
	}
	if targetExists && trashExists {
		return fmt.Errorf("恢复目标和回收站同时存在，外部状态不确定: %s", receipt.RelativePath)
	}
	if !trashExists {
		return fmt.Errorf("回收站源不存在，无法确认是否已恢复: %s", receipt.RelativePath)
	}
	return trash.Restore(ctx, platform.TrashReceipt{
		OperationID: entry.OperationID, RelativePath: receipt.RelativePath, TrashPath: receipt.IsolatedPath,
	}, entry.Plan.Root)
}

func recoveryAsset(assets []lifecycle.AssetFingerprint, relative string) (lifecycle.AssetFingerprint, bool) {
	wanted := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	for _, asset := range assets {
		if filepath.ToSlash(filepath.Clean(filepath.FromSlash(asset.RelativePath))) == wanted {
			return asset, true
		}
	}
	return lifecycle.AssetFingerprint{}, false
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func hashRecoveryFile(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
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

func recoveryFileIdentity(path string) string {
	info, err := os.Lstat(path)
	if err != nil || info.Sys() == nil {
		return ""
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return ""
	}
	dev, devOK := recoveryStatField(value, "Dev")
	ino, inoOK := recoveryStatField(value, "Ino")
	if devOK && inoOK {
		return fmt.Sprintf("%T:%s:%s", info.Sys(), dev, ino)
	}
	return ""
}

func recoveryStatField(value reflect.Value, name string) (string, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() || !field.CanInterface() {
		return "", false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprintf("%d", field.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return fmt.Sprintf("%d", field.Uint()), true
	default:
		return "", false
	}
}

func safeRecoveryTarget(root, relative string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("解析恢复根失败: %w", err)
	}
	rootInfo, err := os.Lstat(absRoot)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", fmt.Errorf("恢复根不是安全普通目录")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if strings.TrimSpace(relative) == "" || clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("恢复资产路径不安全: %s", relative)
	}
	target := filepath.Join(absRoot, clean)
	if !pathWithinRecoveryRoot(absRoot, target) {
		return "", fmt.Errorf("恢复资产路径越出源根: %s", relative)
	}
	current := absRoot
	for _, part := range strings.Split(filepath.Dir(clean), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("恢复资产父路径不安全: %s", relative)
		}
	}
	return target, nil
}

func pathWithinRecoveryRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func recordRecoveryFailure(journalPath string, receipt lifecycle.ReceiptRecord, restoreErr error) error {
	entry, err := lifecycle.LoadJournal(context.Background(), journalPath)
	if err != nil {
		return err
	}
	entry.RecoveryNote = fmt.Sprintf("恢复 %s 失败：%v；请检查回收站中的 %s", receipt.RelativePath, restoreErr, receipt.IsolatedPath)
	entry.ErrorCode = "RECOVERY_CONFLICT"
	entry.ErrorMessage = restoreErr.Error()
	// 通过允许的同阶段转换原子落盘错误，不改变人工复核状态。
	_, err = lifecycle.TransitionJournal(context.Background(), journalPath, lifecycle.PhaseNeedsReview, entry.ErrorCode, entry.ErrorMessage, entry.RecoveryNote)
	return err
}
