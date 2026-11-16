// 中文说明：把本地归档、回收站和永久删除串成可审阅、可补偿的合成生命周期 saga。
// 该入口只接受调用方显式提供的根目录、归档目录和 journal 目录；真实 Codex 适配器仍由能力声明决定。
package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/maintenance"
	"codex-chronicle.local/internal/platform"
)

// SagaResult 汇总一次生命周期操作的归档、回收站和 journal 状态。
type SagaResult struct {
	OperationID   string                       `json:"operation_id"`
	JournalPath   string                       `json:"journal_path"`
	Archive       *archive.Result              `json:"archive,omitempty"`
	RemoteArchive *archive.GitHubArchiveResult `json:"remote_archive,omitempty"`
	TrashReceipts []platform.TrashReceipt      `json:"trash_receipts,omitempty"`
	Mutation      *compat.MutationOutcome      `json:"control_plane,omitempty"`
	Phase         lifecycle.JournalPhase       `json:"phase"`
	NeedsReview   bool                         `json:"needs_review"`
}

// ExecutionPhase 是可注入故障和观测的生命周期边界；回调不应写入源目录。
type ExecutionPhase string

const (
	PhaseJournalCreated        ExecutionPhase = "journal_created"
	PhaseArchiveStarting       ExecutionPhase = "archive_starting"
	PhaseArchiveCreated        ExecutionPhase = "archive_created"
	PhaseArchiveVerified       ExecutionPhase = "archive_verified"
	PhaseMutationStarting      ExecutionPhase = "mutation_starting"
	PhaseMutationApplied       ExecutionPhase = "mutation_applied"
	PhaseSessionIndexRewritten ExecutionPhase = "session_index_rewritten"
	PhaseTrashStarting         ExecutionPhase = "trash_starting"
	PhaseTrashMoved            ExecutionPhase = "trash_moved"
	PhaseCommitStarting        ExecutionPhase = "commit_starting"
	PhaseCommitted             ExecutionPhase = "committed"
)

// ErrExecutionInterrupted 模拟突然终止；调用方返回时保留最近一次已落盘 journal，不把它改写成失败。
var ErrExecutionInterrupted = errors.New("模拟进程突然终止")

// PhaseHook 用于故障注入和 UI/测试观测。返回 ErrExecutionInterrupted 会模拟 SIGKILL 后重启。
type PhaseHook func(context.Context, ExecutionPhase, string) error

// ExecuteLocalSaga 保留无确认码的兼容入口；包含永久删除的计划会被安全拒绝。
func ExecuteLocalSaga(ctx context.Context, plan lifecycle.OperationPlan, archiveDestination, journalDirectory string, trash platform.TrashProvider) (SagaResult, error) {
	return ExecuteLocalSagaWithOptions(ctx, plan, archiveDestination, journalDirectory, trash, ExecutionOptions{})
}

// ExecuteLocalSagaWithConfirmation 在调用方提供的合成/副本根目录上执行生命周期计划。
// 永久删除必须再次提供与计划绑定的随机数字；归档、回收站和删除前后都会重新校验源指纹。
func ExecuteLocalSagaWithConfirmation(ctx context.Context, plan lifecycle.OperationPlan, archiveDestination, journalDirectory string, trash platform.TrashProvider, confirmationCode string) (SagaResult, error) {
	return ExecuteLocalSagaWithOptions(ctx, plan, archiveDestination, journalDirectory, trash, ExecutionOptions{ConfirmationCode: confirmationCode})
}

// ExecutionOptions 保存执行时一次性提供的敏感输入；不会写入 journal 或归档 manifest。
type ExecutionOptions struct {
	ConfirmationCode     string
	ArchivePassword      string
	ControlPlaneMutation compat.ControlPlaneMutation
	RemoteArchive        archive.RemoteArchiveProvider
	RemoteArchiveID      string
	PhaseHook            PhaseHook
	// RegisterArtifact 可选登记本次操作创建的 archive staging；默认 daemon 不提供它。
	RegisterArtifact func(context.Context, maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error)
}

// ExecuteLocalSagaWithOptions 执行本地计划，并在需要时使用客户端加密密码。
func ExecuteLocalSagaWithOptions(ctx context.Context, plan lifecycle.OperationPlan, archiveDestination, journalDirectory string, trash platform.TrashProvider, options ExecutionOptions) (SagaResult, error) {
	if plan.Status != lifecycle.PlanPrepared && plan.Status != lifecycle.PlanConfirmed {
		return SagaResult{}, fmt.Errorf("计划状态 %s 不可执行", plan.Status)
	}
	if containsAction(plan, lifecycle.ActionPermanentDelete) && !lifecycle.VerifyConfirmation(plan, options.ConfirmationCode) {
		return SagaResult{}, fmt.Errorf("永久删除确认数字与计划不匹配")
	}
	if err := lifecycle.RevalidatePlan(ctx, plan); err != nil {
		return SagaResult{}, fmt.Errorf("执行前资产复核失败: %w", err)
	}
	entry, journalPath, err := lifecycle.CreateJournal(ctx, journalDirectory, plan)
	if err != nil {
		return SagaResult{}, err
	}
	result := SagaResult{OperationID: plan.OperationID, JournalPath: journalPath, Phase: entry.Phase}
	registeredArtifacts := make(map[string]maintenance.ArtifactManifest)
	fail := func(code, message, note string) (SagaResult, error) {
		updated, transitionErr := lifecycle.TransitionJournal(context.Background(), journalPath, lifecycle.PhaseNeedsReview, code, message, note)
		if transitionErr == nil {
			result.Phase = updated.Phase
			result.NeedsReview = true
		}
		if options.RegisterArtifact != nil {
			for name, registered := range registeredArtifacts {
				if registered.State == "published" {
					continue
				}
				registered.Kind = maintenance.ArtifactFailedArchive
				registered.State = "failed"
				registered.LeaseUntil = time.Time{}
				registered.ExpiresAt = time.Time{}
				registered.Rebuildable = true
				registered.Risk = "high"
				registered.RetentionReason = "归档操作失败；保留失败产物登记供审计，等待 failed retention 到期"
				if _, registerErr := options.RegisterArtifact(context.Background(), registered); registerErr != nil && transitionErr == nil {
					transitionErr = fmt.Errorf("登记失败归档 %s 失败：%w", name, registerErr)
				}
			}
		}
		return result, fmt.Errorf("saga 进入人工复核: %s", message)
	}
	hook := func(phase ExecutionPhase, code, note string) (bool, error) {
		if options.PhaseHook == nil {
			return false, nil
		}
		if err := options.PhaseHook(ctx, phase, journalPath); err != nil {
			if errors.Is(err, ErrExecutionInterrupted) {
				return true, err
			}
			_, failErr := fail(code, err.Error(), note)
			return true, failErr
		}
		return false, nil
	}
	recordStage := func(name, state, path, inputHash, outputHash, note string) error {
		if state == "started" && options.RegisterArtifact != nil && (name == "archive" || name == "encrypted_archive") {
			stagePath := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
			registered, err := options.RegisterArtifact(context.Background(), maintenance.ArtifactManifest{
				Kind: maintenance.ArtifactStaging, TargetPath: stagePath, CreatedAt: time.Now().UTC(), Owner: "lifecycle/" + name,
				OperationID: entry.OperationID, LeaseUntil: time.Now().UTC().Add(24 * time.Hour), Source: "本地归档 staging",
				Rebuildable: true, Risk: "medium", RetentionReason: "等待归档操作完成或异常恢复后再判断", JournalPath: journalPath, WorkspaceRoot: plan.Root,
			})
			if err != nil {
				return fmt.Errorf("登记归档 staging 失败：%w", err)
			}
			registeredArtifacts[name] = registered
		}
		if state == "verified" && options.RegisterArtifact != nil {
			if registered, ok := registeredArtifacts[name]; ok {
				registered.TargetPath = path
				registered.State = "published"
				registered.Rebuildable = false
				registered.LeaseUntil = time.Time{}
				registered.RetentionReason = "归档已验证并发布；归档生命周期由用户单独管理"
				if _, err := options.RegisterArtifact(context.Background(), registered); err != nil {
					return fmt.Errorf("更新归档生成物登记失败：%w", err)
				}
				registeredArtifacts[name] = registered
			}
		}
		_, err := lifecycle.RecordStage(context.Background(), journalPath, lifecycle.StageRecord{Name: name, State: state, Path: path, InputSHA256: inputHash, OutputSHA256: outputHash, CompletedAt: time.Now().UTC(), Note: note})
		return err
	}
	if stopped, hookErr := hook(PhaseJournalCreated, "JOURNAL_HOOK_FAILED", "journal 已创建但尚未执行外部副作用"); stopped {
		return result, hookErr
	}
	if err := recordStage("journal", "completed", journalPath, entry.InputPlanSHA256, "", "prepared journal 已原子落盘"); err != nil {
		return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "journal 阶段记录失败，外部副作用尚未开始")
	}
	if requiresControlPlaneMutation(plan) && options.ControlPlaneMutation == nil {
		return fail("CONTROL_PLANE_ADAPTER_MISSING", "计划包含控制面资产，但未提供兼容 mutation adapter", "源文件、SQLite 行和 session index 均保持不变；先绑定已验证的 adapter 后重新生成计划")
	}
	var mutationSnapshot *compat.MutationSnapshot
	if requiresControlPlaneMutation(plan) {
		snapshot, prepareErr := options.ControlPlaneMutation.Prepare(ctx, mutationRequestFromPlan(plan))
		if prepareErr != nil {
			return fail("CONTROL_PLANE_PREPARE_FAILED", prepareErr.Error(), "控制面快照未完成，源资产保持当前状态")
		}
		if _, attachErr := lifecycle.AttachMutationSnapshot(ctx, journalPath, snapshot); attachErr != nil {
			return fail("CONTROL_PLANE_JOURNAL_BIND_FAILED", attachErr.Error(), "控制面快照已生成但未能绑定 journal")
		}
		mutationSnapshot = &snapshot
	}

	for _, action := range plan.Actions {
		switch action {
		case lifecycle.ActionArchive:
			if stopped, hookErr := hook(PhaseArchiveStarting, "ARCHIVE_HOOK_FAILED", "归档尚未开始，源资产保持不变"); stopped {
				return result, hookErr
			}
			if err := recordStage("archive", "started", filepath.Join(archiveDestination, plan.OperationID), plan.AssetSetHash, "", "归档 staging 即将创建"); err != nil {
				return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "归档尚未开始")
			}
			if archiveDestination == "" {
				return fail("ARCHIVE_DESTINATION_MISSING", "未提供归档目录", "补充外部归档目录后重新执行")
			}
			var archiveResult archive.Result
			var archiveErr error
			if mutationSnapshot != nil {
				archiveResult, archiveErr = archive.CreateLocalWithMutationSnapshot(ctx, plan, archiveDestination, *mutationSnapshot)
			} else {
				archiveResult, archiveErr = archive.CreateLocal(ctx, plan, archiveDestination)
			}
			if archiveErr != nil {
				return fail("ARCHIVE_FAILED", archiveErr.Error(), "源资产应保持不变，检查归档临时目录和磁盘空间")
			}
			result.Archive = &archiveResult
			if stopped, hookErr := hook(PhaseArchiveCreated, "ARCHIVE_HOOK_FAILED", "本地归档可能已生成，保留 journal 等待复核"); stopped {
				return result, hookErr
			}
			if attachErr := attachLocalArchive(ctx, journalPath, archiveResult.Directory); attachErr != nil {
				return fail("JOURNAL_ARCHIVE_RECEIPT_FAILED", attachErr.Error(), "本地归档已生成但未能写入 journal")
			}
			if err := recordStage("archive", "verified", archiveResult.Directory, plan.AssetSetHash, resultArchiveHash(archiveResult), "归档 manifest 和 payload 已通过 provider 校验"); err != nil {
				return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "归档已生成但阶段记录失败")
			}
			if stopped, hookErr := hook(PhaseArchiveVerified, "ARCHIVE_HOOK_FAILED", "归档已校验，源资产尚未移动"); stopped {
				return result, hookErr
			}
			if options.RemoteArchive != nil {
				remoteResult, remoteErr := publishRemoteArchive(ctx, options.RemoteArchive, archiveResult.Directory, plan, options.RemoteArchiveID)
				if remoteErr != nil {
					return fail("REMOTE_ARCHIVE_FAILED", remoteErr.Error(), "本地归档保留；检查远端网络、权限和未发布分片")
				}
				result.RemoteArchive = &remoteResult
				if attachErr := attachRemoteArchive(ctx, journalPath, remoteResult); attachErr != nil {
					return fail("JOURNAL_REMOTE_ARCHIVE_RECEIPT_FAILED", attachErr.Error(), "远端归档已校验但未能写入 journal")
				}
			}
			updated, transitionErr := lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseArchived, "", "", "归档 manifest 和 payload 已校验")
			if transitionErr != nil {
				return fail("JOURNAL_ARCHIVE_TRANSITION_FAILED", transitionErr.Error(), "归档已存在，需人工校验 journal")
			}
			result.Phase = updated.Phase
		case lifecycle.ActionEncryptedArchive:
			if stopped, hookErr := hook(PhaseArchiveStarting, "ARCHIVE_HOOK_FAILED", "加密归档尚未开始，源资产保持不变"); stopped {
				return result, hookErr
			}
			if err := recordStage("encrypted_archive", "started", filepath.Join(archiveDestination, plan.OperationID), plan.AssetSetHash, "", "加密归档 staging 即将创建"); err != nil {
				return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "加密归档尚未开始")
			}
			if archiveDestination == "" {
				return fail("ARCHIVE_DESTINATION_MISSING", "未提供归档目录", "补充外部归档目录后重新执行")
			}
			if options.ArchivePassword == "" {
				return fail("ARCHIVE_PASSWORD_MISSING", "加密归档未提供密码，拒绝以明文冒充加密归档", "补充密码后重新执行")
			}
			var archiveResult archive.Result
			var archiveErr error
			if mutationSnapshot != nil {
				archiveResult, archiveErr = archive.CreateLocalEncryptedWithMutationSnapshot(ctx, plan, archiveDestination, options.ArchivePassword, *mutationSnapshot)
			} else {
				archiveResult, archiveErr = archive.CreateLocalEncrypted(ctx, plan, archiveDestination, options.ArchivePassword)
			}
			if archiveErr != nil {
				return fail("ENCRYPTED_ARCHIVE_FAILED", archiveErr.Error(), "源资产应保持不变，检查密码和磁盘空间")
			}
			result.Archive = &archiveResult
			if stopped, hookErr := hook(PhaseArchiveCreated, "ARCHIVE_HOOK_FAILED", "加密归档可能已生成，保留 journal 等待复核"); stopped {
				return result, hookErr
			}
			if attachErr := attachLocalArchive(ctx, journalPath, archiveResult.Directory); attachErr != nil {
				return fail("JOURNAL_ARCHIVE_RECEIPT_FAILED", attachErr.Error(), "本地加密归档已生成但未能写入 journal")
			}
			if err := recordStage("encrypted_archive", "verified", archiveResult.Directory, plan.AssetSetHash, resultArchiveHash(archiveResult), "加密归档 manifest、分块认证和 payload 已通过校验"); err != nil {
				return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "加密归档已生成但阶段记录失败")
			}
			if stopped, hookErr := hook(PhaseArchiveVerified, "ARCHIVE_HOOK_FAILED", "加密归档已校验，源资产尚未移动"); stopped {
				return result, hookErr
			}
			if options.RemoteArchive != nil {
				remoteResult, remoteErr := publishRemoteArchive(ctx, options.RemoteArchive, archiveResult.Directory, plan, options.RemoteArchiveID)
				if remoteErr != nil {
					return fail("REMOTE_ARCHIVE_FAILED", remoteErr.Error(), "本地加密归档保留；检查远端网络、权限和未发布分片")
				}
				result.RemoteArchive = &remoteResult
				if attachErr := attachRemoteArchive(ctx, journalPath, remoteResult); attachErr != nil {
					return fail("JOURNAL_REMOTE_ARCHIVE_RECEIPT_FAILED", attachErr.Error(), "远端加密归档已校验但未能写入 journal")
				}
			}
			updated, transitionErr := lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseArchived, "", "", "加密归档 manifest、分块认证和 payload 已校验")
			if transitionErr != nil {
				return fail("JOURNAL_ARCHIVE_TRANSITION_FAILED", transitionErr.Error(), "加密归档已存在，需人工校验 journal")
			}
			result.Phase = updated.Phase
		case lifecycle.ActionTrash:
			if stopped, hookErr := hook(PhaseTrashStarting, "TRASH_HOOK_FAILED", "回收站移动尚未开始"); stopped {
				return result, hookErr
			}
			if err := recordStage("trash", "started", "", plan.AssetSetHash, "", "回收站移动即将开始"); err != nil {
				return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "回收站移动尚未开始")
			}
			if trash == nil {
				return fail("TRASH_PROVIDER_MISSING", "未提供回收站 provider", "归档结果保留，源资产未移动")
			}
			receipts, trashErr := platform.ExecuteTrashWithJournal(ctx, plan, trash, func(receipt platform.TrashReceipt) error {
				record := lifecycle.ReceiptRecord{
					RelativePath: receipt.RelativePath,
					OriginalPath: plan.Root + string(os.PathSeparator) + receipt.RelativePath,
					IsolatedPath: receipt.TrashPath,
					Provider:     "trash_provider",
				}
				for _, asset := range plan.FileAssets() {
					if filepath.ToSlash(filepath.Clean(filepath.FromSlash(asset.RelativePath))) == filepath.ToSlash(filepath.Clean(filepath.FromSlash(receipt.RelativePath))) {
						record.SizeBytes = asset.SizeBytes
						record.Identity = asset.Identity
						record.SHA256 = asset.SHA256
						break
					}
				}
				_, appendErr := lifecycle.AppendReceipt(ctx, journalPath, record)
				return appendErr
			})
			if trashErr != nil {
				return fail("TRASH_FAILED", trashErr.Error(), "已尝试恢复先前移动的资产")
			}
			result.TrashReceipts = receipts
			if stopped, hookErr := hook(PhaseTrashMoved, "TRASH_HOOK_FAILED", "回收站回执已产生，控制面可能尚未更新"); stopped {
				return result, hookErr
			}
			if err := recordStage("trash", "completed", "", plan.AssetSetHash, trashReceiptHash(receipts), "回收站 receipt 已写入 journal"); err != nil {
				return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "回收站回执已产生但阶段记录失败")
			}
			updated, transitionErr := lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseIsolated, "", "", "资产已移入隔离回收站，等待控制面关系复核")
			if transitionErr != nil {
				return fail("JOURNAL_ISOLATION_TRANSITION_FAILED", transitionErr.Error(), "检查 journal 和隔离目录")
			}
			result.Phase = updated.Phase
			if mutationSnapshot == nil {
				updated, transitionErr = lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseNeedsReview, "CONTROL_PLANE_NOT_UPDATED", "资产已归档并隔离，但尚未修改 Codex SQLite 和索引", "保持回收站回执，待兼容 adapter 完成完整关系删除后再决定提交")
				if transitionErr != nil {
					return fail("JOURNAL_REVIEW_TRANSITION_FAILED", transitionErr.Error(), "检查 journal 和隔离目录")
				}
				result.Phase = updated.Phase
				result.NeedsReview = true
				return result, fmt.Errorf("本地 saga 已隔离但需要人工复核")
			}
			if stopped, hookErr := hook(PhaseMutationStarting, "MUTATION_HOOK_FAILED", "控制面 mutation 尚未开始"); stopped {
				return result, hookErr
			}
			mutationResult, mutationErr := options.ControlPlaneMutation.Apply(ctx, *mutationSnapshot, compat.MutationDelete)
			result.Mutation = &mutationResult
			if mutationErr != nil {
				return fail("CONTROL_PLANE_DELETE_FAILED", mutationErr.Error(), "保留回收站回执和控制面快照，使用恢复流程回滚")
			}
			if stopped, hookErr := hook(PhaseMutationApplied, "MUTATION_HOOK_FAILED", "控制面 mutation 可能已提交，必须按 journal 复核"); stopped {
				return result, hookErr
			}
			if stopped, hookErr := hook(PhaseSessionIndexRewritten, "MUTATION_HOOK_FAILED", "session index 可能已重写，必须按快照复核"); stopped {
				return result, hookErr
			}
			if err := recordStage("mutation", "completed", "", mutationSnapshot.SnapshotHash, mutationResult.SnapshotHash, "控制面行和 session index 已验证"); err != nil {
				return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "控制面可能已更新但阶段记录失败")
			}
			updated, transitionErr = lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseControlDB, "", "", "fixture control-plane 行已事务删除并校验")
			if transitionErr != nil {
				return fail("JOURNAL_CONTROL_DB_TRANSITION_FAILED", transitionErr.Error(), "控制面已更新，检查 journal 和回收站")
			}
			result.Phase = updated.Phase
		case lifecycle.ActionPermanentDelete:
			if stopped, hookErr := hook(PhaseMutationStarting, "MUTATION_HOOK_FAILED", "永久删除尚未开始"); stopped {
				return result, hookErr
			}
			if err := recordStage("permanent_delete", "started", "", plan.AssetSetHash, "", "永久删除前复核完成"); err != nil {
				return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "永久删除尚未开始")
			}
			if err := lifecycle.RevalidatePlan(ctx, plan); err != nil {
				return fail("ASSET_CHANGED_BEFORE_DELETE", err.Error(), "源资产发生变化，归档结果保留且未继续删除")
			}
			for _, asset := range plan.FileAssets() {
				path, resolveErr := application.ResolveReadOnlyPath(plan.Root, asset.RelativePath)
				if resolveErr != nil {
					return fail("DELETE_PATH_INVALID", resolveErr.Error(), "源资产未继续删除")
				}
				if removeErr := os.Remove(path); removeErr != nil {
					return fail("PERMANENT_DELETE_FAILED", removeErr.Error(), "永久删除不可自动恢复，需根据已删除清单人工复核")
				}
			}
			updated, transitionErr := lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseSourceRemoved, "", "", "计划内源文件已按确认码永久删除")
			if transitionErr != nil {
				return fail("JOURNAL_DELETE_TRANSITION_FAILED", transitionErr.Error(), "源文件已删除，检查 journal 和归档 manifest")
			}
			result.Phase = updated.Phase
			if stopped, hookErr := hook(PhaseMutationApplied, "MUTATION_HOOK_FAILED", "源文件已删除，控制面可能尚未更新"); stopped {
				return result, hookErr
			}
			if mutationSnapshot != nil {
				mutationResult, mutationErr := options.ControlPlaneMutation.Apply(ctx, *mutationSnapshot, compat.MutationDelete)
				result.Mutation = &mutationResult
				if mutationErr != nil {
					return fail("CONTROL_PLANE_DELETE_FAILED", mutationErr.Error(), "源文件已删除但控制面未完成，必须人工复核")
				}
				if stopped, hookErr := hook(PhaseSessionIndexRewritten, "MUTATION_HOOK_FAILED", "session index 可能已重写，必须按快照复核"); stopped {
					return result, hookErr
				}
				if err := recordStage("permanent_delete_control_plane", "completed", "", mutationSnapshot.SnapshotHash, mutationResult.SnapshotHash, "控制面行和 session index 已验证"); err != nil {
					return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "源文件和控制面可能已更新但阶段记录失败")
				}
				updated, transitionErr = lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseControlDB, "", "", "fixture control-plane 行已事务删除并校验")
				if transitionErr != nil {
					return fail("JOURNAL_CONTROL_DB_TRANSITION_FAILED", transitionErr.Error(), "控制面已更新，检查 journal 和归档")
				}
				result.Phase = updated.Phase
			}
		default:
			return fail("UNSUPPORTED_ACTION", fmt.Sprintf("当前本地 saga 不支持动作 %s", action), "保持源资产不变并等待对应 provider")
		}
	}

	if stopped, hookErr := hook(PhaseCommitStarting, "COMMIT_HOOK_FAILED", "提交标记尚未写入"); stopped {
		return result, hookErr
	}
	if err := recordStage("commit", "started", journalPath, plan.AssetSetHash, "", "准备写入 committed 标记"); err != nil {
		return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "外部副作用可能已完成但提交状态尚未落盘")
	}
	updated, transitionErr := lifecycle.TransitionJournal(ctx, journalPath, lifecycle.PhaseCommitted, "", "", "计划内动作完成，journal 已进入终态")
	if transitionErr != nil {
		return fail("JOURNAL_COMMIT_FAILED", transitionErr.Error(), "归档结果已存在，检查 journal")
	}
	result.Phase = updated.Phase
	if stopped, hookErr := hook(PhaseCommitted, "COMMIT_HOOK_FAILED", "committed 标记已落盘"); stopped {
		return result, hookErr
	}
	if err := recordStage("commit", "completed", journalPath, plan.AssetSetHash, plan.AssetSetHash, "committed 标记已落盘并可恢复核对"); err != nil {
		return fail("JOURNAL_STAGE_RECORD_FAILED", err.Error(), "committed 标记已写入但阶段记录失败")
	}
	return result, nil
}

func resultArchiveHash(result archive.Result) string {
	if result.ManifestPath == "" {
		return ""
	}
	data, err := os.ReadFile(result.ManifestPath)
	if err != nil {
		return ""
	}
	return hashBytes(data)
}

func trashReceiptHash(receipts []platform.TrashReceipt) string {
	if len(receipts) == 0 {
		return ""
	}
	data, err := json.Marshal(receipts)
	if err != nil {
		return ""
	}
	return hashBytes(data)
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func containsAction(plan lifecycle.OperationPlan, wanted lifecycle.Action) bool {
	for _, action := range plan.Actions {
		if action == wanted {
			return true
		}
	}
	return false
}

func attachLocalArchive(ctx context.Context, journalPath, directory string) error {
	_, err := lifecycle.AttachArchiveReceipt(ctx, journalPath, directory, "", "")
	return err
}

func attachRemoteArchive(ctx context.Context, journalPath string, result archive.GitHubArchiveResult) error {
	_, err := lifecycle.AttachArchiveReceipt(ctx, journalPath, "", result.ArchiveID, result.ManifestPath)
	return err
}

func publishRemoteArchive(ctx context.Context, provider archive.RemoteArchiveProvider, directory string, plan lifecycle.OperationPlan, requestedID string) (archive.GitHubArchiveResult, error) {
	archiveID := requestedID
	if archiveID == "" {
		archiveID = plan.OperationID
	}
	uploaded, err := provider.UploadDirectory(ctx, directory, archiveID)
	if err != nil {
		return archive.GitHubArchiveResult{}, fmt.Errorf("远端归档上传失败: %w", err)
	}
	if uploaded.ArchiveID != archiveID || !uploaded.Verified {
		return archive.GitHubArchiveResult{}, fmt.Errorf("远端归档上传结果未通过身份或校验确认")
	}
	verified, err := provider.VerifyArchive(ctx, archiveID)
	if err != nil {
		return archive.GitHubArchiveResult{}, fmt.Errorf("远端归档只读校验失败: %w", err)
	}
	if verified.ArchiveID != archiveID || !verified.Verified {
		return archive.GitHubArchiveResult{}, fmt.Errorf("远端归档校验结果未通过身份或校验确认")
	}
	return verified, nil
}

func requiresControlPlaneMutation(plan lifecycle.OperationPlan) bool {
	if !containsAction(plan, lifecycle.ActionTrash) && !containsAction(plan, lifecycle.ActionPermanentDelete) {
		return false
	}
	for _, asset := range plan.Assets {
		if asset.ControlPlaneOnly || asset.Kind == domain.AssetKindSQLite || asset.Kind == domain.AssetKindSessionIndex {
			return true
		}
	}
	return false
}

func mutationRequestFromPlan(plan lifecycle.OperationPlan) compat.MutationRequest {
	assets := make([]compat.MutationAsset, 0, len(plan.Assets))
	for _, asset := range plan.Assets {
		assets = append(assets, compat.MutationAsset{
			RelativePath: asset.RelativePath, Kind: asset.Kind, SizeBytes: asset.SizeBytes,
			SHA256: asset.SHA256, Status: domain.StatusStable,
		})
	}
	return compat.MutationRequest{Root: plan.Root, ConversationID: plan.ConversationID, SourceID: plan.SourceID, AssetSetHash: plan.AssetSetHash, Assets: assets}
}
