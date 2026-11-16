// 中文说明：编排本地归档的两阶段恢复；文件恢复和 control-plane adapter 恢复共享独立 restore journal。
package operation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

// ArchiveRestorePlan 是只读恢复预览；密码不会复制到此结构或 journal。
type ArchiveRestorePlan struct {
	RestoreOperationID string                   `json:"restore_operation_id"`
	SourceJournalPath  string                   `json:"source_journal_path,omitempty"`
	SourceOperationID  string                   `json:"source_operation_id,omitempty"`
	ArchivePath        string                   `json:"archive_path"`
	TargetRoot         string                   `json:"target_root"`
	SourcePlan         lifecycle.OperationPlan  `json:"source_plan"`
	Preview            archive.RestorePreview   `json:"preview"`
	Mutation           *compat.MutationSnapshot `json:"control_plane_snapshot,omitempty"`
	ControlPlaneBound  bool                     `json:"control_plane_bound"`
}

// RestoreExecutionOptions 保存恢复时一次性输入的密码和 adapter。
type RestoreExecutionOptions struct {
	ArchivePassword      string
	ControlPlaneMutation compat.ControlPlaneMutation
}

// ArchiveRestorePreviewOptions 控制来源 journal 与下载后归档副本的绑定方式。
// 默认要求 journal 中的归档路径完全一致；远端下载恢复只放宽路径，仍严格校验 manifest 身份和资产摘要。
type ArchiveRestorePreviewOptions struct {
	AllowSourceArchivePathAlias bool
}

// ArchiveRestoreResult 是恢复 journal、文件和 control-plane 状态的汇总。
type ArchiveRestoreResult struct {
	RestoreOperationID string                  `json:"restore_operation_id"`
	RestoreJournalPath string                  `json:"restore_journal_path"`
	SourceJournalPath  string                  `json:"source_journal_path,omitempty"`
	ArchivePath        string                  `json:"archive_path"`
	TargetRoot         string                  `json:"target_root"`
	Files              *archive.RestoreResult  `json:"files,omitempty"`
	Mutation           *compat.MutationOutcome `json:"control_plane,omitempty"`
	Phase              lifecycle.RestorePhase  `json:"phase"`
	NeedsReview        bool                    `json:"needs_review"`
}

// PreviewArchiveRestore 校验归档、来源 journal 绑定和目标冲突，不写入任何文件。
// sourceJournalPath 可以为空；为空时仅允许文件恢复，归档中的控制面快照不会被猜测使用。
func PreviewArchiveRestore(ctx context.Context, archivePath, targetRoot, sourceJournalPath, password string) (ArchiveRestorePlan, error) {
	return previewArchiveRestore(ctx, archivePath, targetRoot, sourceJournalPath, password, ArchiveRestorePreviewOptions{})
}

// PreviewDownloadedArchiveRestore 预览已下载到本地的远端归档；只允许归档路径变化，不放宽来源身份校验。
func PreviewDownloadedArchiveRestore(ctx context.Context, archivePath, targetRoot, sourceJournalPath, password string) (ArchiveRestorePlan, error) {
	return previewArchiveRestore(ctx, archivePath, targetRoot, sourceJournalPath, password, ArchiveRestorePreviewOptions{AllowSourceArchivePathAlias: true})
}

func previewArchiveRestore(ctx context.Context, archivePath, targetRoot, sourceJournalPath, password string, options ArchiveRestorePreviewOptions) (ArchiveRestorePlan, error) {
	absArchive, err := filepath.Abs(archivePath)
	if err != nil {
		return ArchiveRestorePlan{}, fmt.Errorf("解析归档路径失败: %w", err)
	}
	absArchive = filepath.Clean(absArchive)
	if strings.TrimSpace(targetRoot) == "" && strings.TrimSpace(sourceJournalPath) == "" {
		return ArchiveRestorePlan{}, fmt.Errorf("独立归档恢复必须提供目标根")
	}
	previewTarget := targetRoot
	var sourceEntry lifecycle.JournalEntry
	var sourcePlan lifecycle.OperationPlan
	var sourcePath string
	if strings.TrimSpace(sourceJournalPath) != "" {
		sourcePath, err = filepath.Abs(sourceJournalPath)
		if err != nil {
			return ArchiveRestorePlan{}, fmt.Errorf("解析来源 journal 失败: %w", err)
		}
		sourcePath = filepath.Clean(sourcePath)
		sourceEntry, err = lifecycle.LoadJournal(ctx, sourcePath)
		if err != nil {
			return ArchiveRestorePlan{}, fmt.Errorf("读取来源 journal 失败: %w", err)
		}
		if sourceEntry.Phase != lifecycle.PhaseCommitted {
			return ArchiveRestorePlan{}, fmt.Errorf("来源 journal 尚未提交，拒绝从未完成操作恢复")
		}
		if !options.AllowSourceArchivePathAlias && !sameRestorePath(sourceEntry.ArchivePath, absArchive) {
			return ArchiveRestorePlan{}, fmt.Errorf("归档路径与来源 journal 不匹配")
		}
		sourcePlan = sourceEntry.Plan
		if strings.TrimSpace(previewTarget) == "" {
			previewTarget = sourcePlan.Root
		}
	}
	manifest, verifyErr := verifyRestoreArchive(ctx, absArchive, password)
	if verifyErr != nil {
		return ArchiveRestorePlan{}, verifyErr
	}
	if sourcePlan.OperationID != "" {
		if manifest.OperationID != sourcePlan.OperationID || manifest.ConversationID != sourcePlan.ConversationID || manifest.AssetSetHash != sourcePlan.AssetSetHash {
			return ArchiveRestorePlan{}, fmt.Errorf("归档 manifest 与来源计划身份不匹配")
		}
	}
	preview, err := archive.PreviewLocalRestore(ctx, absArchive, previewTarget, password)
	if err != nil {
		return ArchiveRestorePlan{}, err
	}
	if sourcePlan.OperationID == "" {
		sourcePlan = standaloneRestorePlan(manifest, previewTarget, preview.Files)
	}
	var mutation *compat.MutationSnapshot
	if sourceEntry.Mutation != nil {
		if manifest.ControlPlaneSnapshot == nil || !manifestMutationMatches(manifest.ControlPlaneSnapshot, sourceEntry.Mutation) {
			return ArchiveRestorePlan{}, fmt.Errorf("归档控制面快照与来源 journal 不匹配")
		}
		if !sameRestorePath(preview.TargetRoot, sourcePlan.Root) {
			return ArchiveRestorePlan{}, fmt.Errorf("包含控制面快照的恢复必须使用来源计划根目录")
		}
		copySnapshot := *sourceEntry.Mutation
		mutation = &copySnapshot
	} else if manifest.ControlPlaneSnapshot != nil {
		// 没有原删除 journal 就没有可靠的控制面绑定；文件恢复可预览，但执行会进入 needs_review。
	}
	operationID, err := lifecycle.NewRestoreOperationID()
	if err != nil {
		return ArchiveRestorePlan{}, err
	}
	return ArchiveRestorePlan{
		RestoreOperationID: operationID, SourceJournalPath: sourcePath, SourceOperationID: sourcePlan.OperationID,
		ArchivePath: absArchive, TargetRoot: preview.TargetRoot, SourcePlan: sourcePlan, Preview: preview,
		Mutation: mutation, ControlPlaneBound: mutation != nil || manifest.ControlPlaneSnapshot == nil,
	}, nil
}

// ExecuteArchiveRestore 执行已预览的本地归档恢复，并为每次恢复创建独立 journal。
func ExecuteArchiveRestore(ctx context.Context, plan ArchiveRestorePlan, journalDirectory string, options RestoreExecutionOptions) (ArchiveRestoreResult, error) {
	if plan.RestoreOperationID == "" || plan.ArchivePath == "" || plan.TargetRoot == "" || plan.SourcePlan.OperationID == "" {
		return ArchiveRestoreResult{}, fmt.Errorf("恢复计划身份不完整")
	}
	assets := make([]lifecycle.RestoreAsset, 0, len(plan.Preview.Files))
	for _, file := range plan.Preview.Files {
		assets = append(assets, lifecycle.RestoreAsset{RelativePath: file.RelativePath, TargetPath: file.TargetPath, SizeBytes: file.SizeBytes, SHA256: file.SHA256})
	}
	entry := lifecycle.RestoreJournalEntry{
		RestoreOperationID: plan.RestoreOperationID, SourceJournalPath: plan.SourceJournalPath, SourceOperationID: plan.SourceOperationID,
		ArchivePath: plan.ArchivePath, TargetRoot: plan.TargetRoot, Plan: plan.SourcePlan, Assets: assets, Mutation: plan.Mutation,
	}
	_, journalPath, err := lifecycle.CreateRestoreJournal(ctx, journalDirectory, entry)
	if err != nil {
		return ArchiveRestoreResult{}, err
	}
	result := ArchiveRestoreResult{RestoreOperationID: plan.RestoreOperationID, RestoreJournalPath: journalPath, SourceJournalPath: plan.SourceJournalPath, ArchivePath: plan.ArchivePath, TargetRoot: plan.TargetRoot, Phase: lifecycle.RestorePhasePrepared}
	fail := func(code, message, note string) (ArchiveRestoreResult, error) {
		updated, transitionErr := lifecycle.TransitionRestoreJournal(context.Background(), journalPath, lifecycle.RestorePhaseNeedsReview, code, message, note)
		if transitionErr == nil {
			result.Phase = updated.Phase
			result.NeedsReview = true
		}
		return result, fmt.Errorf("归档恢复进入人工复核: %s", message)
	}
	if !plan.ControlPlaneBound {
		return fail("CONTROL_PLANE_BINDING_MISSING", "归档包含控制面快照但未绑定来源 journal", "文件恢复未开始；请提供原删除 journal 和匹配 adapter")
	}
	if plan.Mutation != nil {
		if options.ControlPlaneMutation == nil {
			return fail("CONTROL_PLANE_ADAPTER_MISSING", "恢复计划包含控制面快照但未提供 adapter", "文件恢复未开始；绑定匹配版本的 adapter 后重新执行")
		}
		if options.ControlPlaneMutation.Descriptor().ID != plan.Mutation.AdapterID {
			return fail("CONTROL_PLANE_ADAPTER_MISMATCH", "恢复 adapter 与来源快照不匹配", "文件恢复未开始；不要用其他版本 adapter 覆盖控制面")
		}
	}
	files, restoreErr := archive.RestoreLocalFiles(ctx, plan.ArchivePath, plan.TargetRoot, options.ArchivePassword)
	if restoreErr != nil {
		return fail("RESTORE_FILES_FAILED", restoreErr.Error(), "目标文件未覆盖；检查冲突清单和归档完整性")
	}
	result.Files = &files
	updated, transitionErr := lifecycle.TransitionRestoreJournal(ctx, journalPath, lifecycle.RestorePhaseFiles, "", "", "归档普通文件已恢复并通过哈希校验")
	if transitionErr != nil {
		return fail("RESTORE_FILES_JOURNAL_FAILED", transitionErr.Error(), "文件已恢复但 journal 未能推进，必须人工核对目标目录")
	}
	result.Phase = updated.Phase
	if plan.Mutation != nil {
		var snapshotErr error
		if files.Encrypted {
			snapshotErr = archive.EnsureMutationSnapshotWithPassword(ctx, plan.ArchivePath, options.ArchivePassword, *plan.Mutation)
		} else {
			snapshotErr = archive.EnsureMutationSnapshot(ctx, plan.ArchivePath, *plan.Mutation)
		}
		if snapshotErr != nil {
			return fail("CONTROL_PLANE_SNAPSHOT_FAILED", snapshotErr.Error(), "普通文件已恢复，控制面未修改；保留 restore journal 等待人工处理")
		}
		mutationResult, mutationErr := options.ControlPlaneMutation.Restore(ctx, *plan.Mutation)
		result.Mutation = &mutationResult
		if mutationErr != nil || !mutationResult.Verified || mutationResult.NeedsReview {
			message := "control-plane adapter 恢复未通过验证"
			if mutationErr != nil {
				message = mutationErr.Error()
			}
			return fail("CONTROL_PLANE_RESTORE_FAILED", message, "普通文件已恢复但控制面保持人工复核，不自动覆盖新行")
		}
		updated, transitionErr = lifecycle.TransitionRestoreJournal(ctx, journalPath, lifecycle.RestorePhaseControlPlane, "", "", "control-plane adapter 已按快照恢复并验证")
		if transitionErr != nil {
			return fail("CONTROL_PLANE_JOURNAL_FAILED", transitionErr.Error(), "控制面已恢复但 journal 未能推进")
		}
		result.Phase = updated.Phase
	}
	updated, transitionErr = lifecycle.TransitionRestoreJournal(ctx, journalPath, lifecycle.RestorePhaseCommitted, "", "", "文件和已绑定控制面均恢复完成")
	if transitionErr != nil {
		return fail("RESTORE_COMMIT_FAILED", transitionErr.Error(), "恢复结果已写入目标，检查 restore journal")
	}
	result.Phase = updated.Phase
	return result, nil
}

// RecoverArchiveRestore 续接进程中断或人工复核后的恢复 journal；密码仍由本次请求一次性提供。
func RecoverArchiveRestore(ctx context.Context, journalPath string, options RestoreExecutionOptions) (ArchiveRestoreResult, error) {
	entry, err := lifecycle.LoadRestoreJournal(ctx, journalPath)
	if err != nil {
		return ArchiveRestoreResult{}, err
	}
	result := ArchiveRestoreResult{
		RestoreOperationID: entry.RestoreOperationID, RestoreJournalPath: journalPath, SourceJournalPath: entry.SourceJournalPath,
		ArchivePath: entry.ArchivePath, TargetRoot: entry.TargetRoot, Phase: entry.Phase,
	}
	if entry.Phase == lifecycle.RestorePhaseCommitted {
		return result, nil
	}
	fail := func(code, message, note string) (ArchiveRestoreResult, error) {
		updated, transitionErr := lifecycle.TransitionRestoreJournal(context.Background(), journalPath, lifecycle.RestorePhaseNeedsReview, code, message, note)
		if transitionErr == nil {
			result.Phase = updated.Phase
			result.NeedsReview = true
		}
		return result, fmt.Errorf("恢复 journal 进入人工复核: %s", message)
	}
	manifest, err := verifyRestoreArchive(ctx, entry.ArchivePath, options.ArchivePassword)
	if err != nil {
		return fail("RESTORE_ARCHIVE_INVALID", err.Error(), "归档校验失败，未继续写入目标或控制面")
	}
	preview, err := archive.PreviewLocalRestore(ctx, entry.ArchivePath, entry.TargetRoot, options.ArchivePassword)
	if err != nil {
		return fail("RESTORE_PREVIEW_FAILED", err.Error(), "恢复预览失败，检查归档和目标路径")
	}
	if err := restoreJournalAssetsMatch(entry, preview); err != nil {
		return fail("RESTORE_JOURNAL_ASSET_MISMATCH", err.Error(), "恢复 journal 与归档 manifest 不一致，拒绝猜测恢复范围")
	}
	plan := ArchiveRestorePlan{
		RestoreOperationID: entry.RestoreOperationID, SourceJournalPath: entry.SourceJournalPath, SourceOperationID: entry.SourceOperationID,
		ArchivePath: entry.ArchivePath, TargetRoot: entry.TargetRoot, SourcePlan: entry.Plan, Preview: preview,
		Mutation: entry.Mutation, ControlPlaneBound: entry.Mutation != nil || manifest.ControlPlaneSnapshot == nil,
	}
	if !plan.ControlPlaneBound {
		return fail("CONTROL_PLANE_BINDING_MISSING", "归档包含控制面快照但恢复 journal 未绑定来源快照", "提供原删除 journal 后重新生成恢复计划")
	}
	if plan.Mutation != nil {
		if options.ControlPlaneMutation == nil {
			return fail("CONTROL_PLANE_ADAPTER_MISSING", "恢复 journal 包含控制面快照但未提供 adapter", "绑定匹配版本的 adapter 后重试")
		}
		if options.ControlPlaneMutation.Descriptor().ID != plan.Mutation.AdapterID {
			return fail("CONTROL_PLANE_ADAPTER_MISMATCH", "恢复 adapter 与 journal 快照不匹配", "不要用其他版本 adapter 覆盖控制面")
		}
	}
	files := makeRestoreResult(manifest, preview)
	if entry.Phase == lifecycle.RestorePhasePrepared || entry.Phase == lifecycle.RestorePhaseNeedsReview {
		allAbsent, absentErr := restoreTargetsAbsent(preview.Files)
		if absentErr != nil {
			if verifyErr := archive.VerifyRestoredFiles(ctx, preview.Files); verifyErr != nil {
				return fail("RESTORE_TARGET_CONFLICT", absentErr.Error(), "目标存在部分文件或内容冲突，未继续写入")
			}
		} else if allAbsent {
			restored, restoreErr := archive.RestoreLocalFiles(ctx, entry.ArchivePath, entry.TargetRoot, options.ArchivePassword)
			if restoreErr != nil {
				return fail("RESTORE_FILES_FAILED", restoreErr.Error(), "目标文件未覆盖；检查冲突清单")
			}
			files = restored
		} else if verifyErr := archive.VerifyRestoredFiles(ctx, preview.Files); verifyErr != nil {
			return fail("RESTORE_TARGET_CONFLICT", verifyErr.Error(), "部分目标文件存在或内容冲突，未继续写入")
		}
		updated, transitionErr := lifecycle.TransitionRestoreJournal(ctx, journalPath, lifecycle.RestorePhaseFiles, "", "", "恢复 journal 续接后确认普通文件已恢复")
		if transitionErr != nil {
			return fail("RESTORE_FILES_JOURNAL_FAILED", transitionErr.Error(), "目标文件状态已核对，检查恢复 journal")
		}
		result.Phase = updated.Phase
	}
	if entry.Phase == lifecycle.RestorePhaseFiles || entry.Phase == lifecycle.RestorePhaseControlPlane {
		if verifyErr := archive.VerifyRestoredFiles(ctx, preview.Files); verifyErr != nil {
			return fail("RESTORE_TARGET_INVALID", verifyErr.Error(), "恢复 journal 标记的目标文件未通过哈希核对")
		}
	}
	result.Files = &files
	if plan.Mutation != nil {
		var snapshotErr error
		if manifest.Encrypted {
			snapshotErr = archive.EnsureMutationSnapshotWithPassword(ctx, entry.ArchivePath, options.ArchivePassword, *plan.Mutation)
		} else {
			snapshotErr = archive.EnsureMutationSnapshot(ctx, entry.ArchivePath, *plan.Mutation)
		}
		if snapshotErr != nil {
			return fail("CONTROL_PLANE_SNAPSHOT_FAILED", snapshotErr.Error(), "普通文件状态已恢复，控制面未修改")
		}
		mutationResult, mutationErr := options.ControlPlaneMutation.Restore(ctx, *plan.Mutation)
		result.Mutation = &mutationResult
		if mutationErr != nil || !mutationResult.Verified || mutationResult.NeedsReview {
			message := "control-plane adapter 恢复未通过验证"
			if mutationErr != nil {
				message = mutationErr.Error()
			}
			return fail("CONTROL_PLANE_RESTORE_FAILED", message, "普通文件已恢复，控制面保持人工复核")
		}
		updated, transitionErr := lifecycle.TransitionRestoreJournal(ctx, journalPath, lifecycle.RestorePhaseControlPlane, "", "", "control-plane adapter 续接恢复并验证")
		if transitionErr != nil {
			return fail("CONTROL_PLANE_JOURNAL_FAILED", transitionErr.Error(), "控制面已恢复但 journal 未能推进")
		}
		result.Phase = updated.Phase
	}
	updated, transitionErr := lifecycle.TransitionRestoreJournal(ctx, journalPath, lifecycle.RestorePhaseCommitted, "", "", "恢复 journal 已续接完成")
	if transitionErr != nil {
		return fail("RESTORE_COMMIT_FAILED", transitionErr.Error(), "恢复结果已落盘，检查 restore journal")
	}
	result.Phase = updated.Phase
	return result, nil
}

func restoreJournalAssetsMatch(entry lifecycle.RestoreJournalEntry, preview archive.RestorePreview) error {
	if len(entry.Assets) != len(preview.Files) {
		return fmt.Errorf("恢复 journal 资产数量与归档不一致")
	}
	byPath := make(map[string]lifecycle.RestoreAsset, len(entry.Assets))
	for _, asset := range entry.Assets {
		byPath[asset.RelativePath] = asset
	}
	for _, file := range preview.Files {
		asset, ok := byPath[file.RelativePath]
		if !ok || asset.TargetPath != file.TargetPath || asset.SizeBytes != file.SizeBytes || asset.SHA256 != file.SHA256 {
			return fmt.Errorf("恢复 journal 资产身份不匹配: %s", file.RelativePath)
		}
	}
	return nil
}

func restoreTargetsAbsent(files []archive.RestoreFile) (bool, error) {
	allAbsent := true
	for _, file := range files {
		_, err := os.Lstat(file.TargetPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		allAbsent = false
	}
	return allAbsent, nil
}

func makeRestoreResult(manifest archive.Manifest, preview archive.RestorePreview) archive.RestoreResult {
	return archive.RestoreResult{ArchiveDirectory: preview.ArchiveDirectory, TargetRoot: preview.TargetRoot, Encrypted: manifest.Encrypted, RestoredFiles: preview.Files, Manifest: manifest, Verified: true, ControlPlaneSnapshot: manifest.ControlPlaneSnapshot != nil}
}

func verifyRestoreArchive(ctx context.Context, path, password string) (archive.Manifest, error) {
	var manifest archive.Manifest
	var err error
	if strings.TrimSpace(password) == "" {
		manifest, err = archive.VerifyLocal(ctx, path)
	} else {
		manifest, err = archive.VerifyLocalEncrypted(ctx, path, password)
	}
	if err != nil {
		return archive.Manifest{}, fmt.Errorf("校验归档失败: %w", err)
	}
	return manifest, nil
}

func standaloneRestorePlan(manifest archive.Manifest, target string, files []archive.RestoreFile) lifecycle.OperationPlan {
	assets := make([]lifecycle.AssetFingerprint, 0, len(files))
	for _, file := range files {
		assets = append(assets, lifecycle.AssetFingerprint{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Kind: domain.AssetKindJSONL})
	}
	return lifecycle.OperationPlan{OperationID: manifest.OperationID, CreatedAt: manifest.CreatedAt, Root: target, ConversationID: manifest.ConversationID, AssetSetHash: manifest.AssetSetHash, Status: lifecycle.PlanConfirmed, Actions: []lifecycle.Action{lifecycle.ActionRestore}, Assets: assets, ReadOnly: false}
}

func manifestMutationMatches(archived *archive.ControlPlaneSnapshotManifest, snapshot *compat.MutationSnapshot) bool {
	if archived == nil || snapshot == nil || archived.AdapterID != snapshot.AdapterID || archived.ConversationID != snapshot.ConversationID || archived.SourceID != snapshot.SourceID || archived.AssetSetHash != snapshot.AssetSetHash || archived.SnapshotHash != snapshot.SnapshotHash || len(archived.Assets) != len(snapshot.Assets) {
		return false
	}
	byPath := make(map[string]compat.MutationAsset, len(snapshot.Assets))
	for _, asset := range snapshot.Assets {
		byPath[asset.RelativePath] = asset
	}
	for _, asset := range archived.Assets {
		other, ok := byPath[asset.RelativePath]
		if !ok || other.Kind != asset.Kind || other.SizeBytes != asset.SizeBytes || other.SHA256 != asset.SHA256 || other.Status != asset.Status {
			return false
		}
	}
	return true
}

func sameRestorePath(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}
