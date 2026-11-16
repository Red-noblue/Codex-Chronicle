// 中文说明：验证本地归档/隔离 saga 的阶段记录和人工复核边界。
package operation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/maintenance"
	"codex-chronicle.local/internal/platform"
	_ "modernc.org/sqlite"
)

func sagaDescriptor() compat.Descriptor {
	return compat.Descriptor{ID: "fixture-saga", SupportedVersions: []string{"fixture-v1"}, Capabilities: map[compat.Capability]bool{
		compat.CapabilityRead: true, compat.CapabilityArchiveExport: true, compat.CapabilityDelete: true,
	}}
}

type failingMutation struct {
	inner compat.ControlPlaneMutation
}

type remoteArchiveProbe struct {
	failUpload bool
	uploads    []string
	verifies   []string
}

func (p *remoteArchiveProbe) UploadDirectory(_ context.Context, directory, archiveID string) (archive.GitHubArchiveResult, error) {
	p.uploads = append(p.uploads, directory+"|"+archiveID)
	if p.failUpload {
		return archive.GitHubArchiveResult{}, fmt.Errorf("注入远端上传失败")
	}
	return archive.GitHubArchiveResult{ArchiveID: archiveID, ManifestPath: "archives/" + archiveID + "/remote-manifest.json", Verified: true}, nil
}

func (p *remoteArchiveProbe) VerifyArchive(_ context.Context, archiveID string) (archive.GitHubArchiveResult, error) {
	p.verifies = append(p.verifies, archiveID)
	return archive.GitHubArchiveResult{ArchiveID: archiveID, ManifestPath: "archives/" + archiveID + "/remote-manifest.json", Verified: true}, nil
}

func (f failingMutation) Descriptor() compat.Descriptor { return f.inner.Descriptor() }
func (f failingMutation) Prepare(ctx context.Context, request compat.MutationRequest) (compat.MutationSnapshot, error) {
	return f.inner.Prepare(ctx, request)
}
func (f failingMutation) Apply(ctx context.Context, snapshot compat.MutationSnapshot, action compat.MutationAction) (compat.MutationOutcome, error) {
	outcome, err := f.inner.Apply(ctx, snapshot, action)
	if err != nil {
		return outcome, err
	}
	return outcome, fmt.Errorf("注入 control-plane 提交后失败")
}
func (f failingMutation) Verify(ctx context.Context, snapshot compat.MutationSnapshot, outcome compat.MutationOutcome) error {
	return f.inner.Verify(ctx, snapshot, outcome)
}
func (f failingMutation) Restore(ctx context.Context, snapshot compat.MutationSnapshot) (compat.MutationOutcome, error) {
	return f.inner.Restore(ctx, snapshot)
}

func sagaPlan(t *testing.T, root string, trash bool) lifecycle.OperationPlan {
	t.Helper()
	path := filepath.Join(root, "rollout.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	options := lifecycle.PlanOptions{Archive: true, Trash: trash}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{ID: "saga-conversation", AssetSetHash: "hash", Status: domain.StatusStable}, []domain.FileInventory{{Asset: domain.Asset{RelativePath: "rollout.jsonl", SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}, sagaDescriptor(), options)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestExecuteLocalSagaArchiveOnlyCommitsWithoutSourceDeletion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("saga\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, false)
	registered := make([]maintenance.ArtifactManifest, 0, 2)
	result, err := ExecuteLocalSagaWithOptions(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), nil, ExecutionOptions{RegisterArtifact: func(_ context.Context, manifest maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error) {
		if manifest.ArtifactID == "" {
			manifest.ArtifactID = "cleanup-" + strings.Repeat("b", 32)
		}
		registered = append(registered, manifest)
		return manifest, nil
	}})
	if err != nil || result.Phase != lifecycle.PhaseCommitted || result.Archive == nil {
		t.Fatalf("unexpected archive-only saga: result=%+v err=%v", result, err)
	}
	if len(registered) != 2 || registered[0].TargetPath == registered[1].TargetPath || registered[1].TargetPath != result.Archive.Directory || registered[1].State != "published" || registered[1].Rebuildable {
		t.Fatalf("归档 staging 登记未更新：%+v", registered)
	}
	if _, err := os.Stat(filepath.Join(root, "rollout.jsonl")); err != nil {
		t.Fatalf("archive-only saga changed source: %v", err)
	}
	entry, err := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if err != nil || lifecycle.NeedsRecovery(entry) {
		t.Fatalf("journal not committed: %+v err=%v", entry, err)
	}
}

func TestExecuteLocalSagaPublishesAndVerifiesRemoteArchiveBeforeCommit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("remote archive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, false)
	remote := &remoteArchiveProbe{}
	result, err := ExecuteLocalSagaWithOptions(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), nil, ExecutionOptions{RemoteArchive: remote, RemoteArchiveID: "remote-batch"})
	if err != nil || result.Phase != lifecycle.PhaseCommitted || result.RemoteArchive == nil {
		t.Fatalf("unexpected remote archive saga: result=%+v err=%v", result, err)
	}
	if len(remote.uploads) != 1 || len(remote.verifies) != 1 || remote.verifies[0] != "remote-batch" {
		t.Fatalf("unexpected remote provider calls: uploads=%v verifies=%v", remote.uploads, remote.verifies)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("remote archive-only saga changed source: %v", err)
	}
	entry, err := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if err != nil || entry.ArchivePath == "" || entry.RemoteArchiveID != "remote-batch" || entry.RemoteArchiveManifestPath == "" {
		t.Fatalf("journal did not persist remote archive receipt: %+v err=%v", entry, err)
	}
}

func TestExecuteLocalSagaStopsBeforeSourceMutationWhenRemoteArchiveFails(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("remote failure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, false)
	remote := &remoteArchiveProbe{failUpload: true}
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	result, err := ExecuteLocalSagaWithOptions(context.Background(), plan, archiveRoot, filepath.Join(t.TempDir(), "journal"), nil, ExecutionOptions{RemoteArchive: remote, RemoteArchiveID: "remote-failing"})
	if err == nil || !result.NeedsReview || result.Phase != lifecycle.PhaseNeedsReview {
		t.Fatalf("expected remote failure review: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("remote archive failure changed source: %v", err)
	}
	if result.Archive == nil || result.Archive.Directory == "" {
		t.Fatal("local archive receipt was lost after remote failure")
	}
	if _, err := os.Stat(result.Archive.Directory); err != nil {
		t.Fatalf("local archive should remain for review: %v", err)
	}
	entry, err := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if err != nil || entry.ArchivePath != result.Archive.Directory || entry.RemoteArchiveID != "" {
		t.Fatalf("unexpected failed remote journal: %+v err=%v", entry, err)
	}
}

func TestExecuteLocalSagaIsolationStopsForReview(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("saga\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, true)
	trash, err := platform.NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteLocalSaga(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), trash)
	if err == nil || !result.NeedsReview || result.Phase != lifecycle.PhaseNeedsReview {
		t.Fatalf("expected review stop: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "rollout.jsonl")); !os.IsNotExist(statErr) {
		t.Fatalf("expected fixture asset isolated, stat=%v", statErr)
	}
	entry, loadErr := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if loadErr != nil || entry.Phase != lifecycle.PhaseNeedsReview || len(entry.Receipts) != 1 {
		t.Fatalf("unexpected isolation journal: %+v err=%v", entry, loadErr)
	}
}

func TestExecuteLocalSagaPermanentDeleteRequiresSecondConfirmation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("permanent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{
		ID: "permanent-conversation", Status: domain.StatusStable,
	}, []domain.FileInventory{{Asset: domain.Asset{RelativePath: "rollout.jsonl", SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}, sagaDescriptor(), lifecycle.PlanOptions{
		PermanentDelete: true, ConfirmationCode: "4821",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExecuteLocalSaga(context.Background(), plan, "", filepath.Join(t.TempDir(), "journal"), nil); err == nil {
		t.Fatal("expected missing second confirmation to reject execution")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rejected operation changed source: %v", err)
	}
}

func TestExecuteLocalSagaPermanentDeleteArchivesBeforeRemoval(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("permanent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{
		ID: "permanent-conversation", Status: domain.StatusStable,
	}, []domain.FileInventory{{Asset: domain.Asset{RelativePath: "rollout.jsonl", SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}, sagaDescriptor(), lifecycle.PlanOptions{
		Archive: true, PermanentDelete: true, ConfirmationCode: "4821",
	})
	if err != nil {
		t.Fatal(err)
	}
	archives := filepath.Join(t.TempDir(), "archives")
	result, err := ExecuteLocalSagaWithConfirmation(context.Background(), plan, archives, filepath.Join(t.TempDir(), "journal"), nil, "4821")
	if err != nil || result.Phase != lifecycle.PhaseCommitted || result.Archive == nil {
		t.Fatalf("unexpected permanent delete result: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source still exists after confirmed deletion: %v", err)
	}
	if _, err := archive.VerifyLocal(context.Background(), result.Archive.Directory); err != nil {
		t.Fatalf("archive verification failed after source deletion: %v", err)
	}
}

func TestExecuteLocalSagaBlocksControlPlaneDeletionWithoutAdapter(t *testing.T) {
	root, databasePath := createSagaSQLiteFixture(t)
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("control plane guard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rolloutInfo, err := os.Stat(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	indexPath, indexFile := createSagaSessionIndex(t, root)
	databaseInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{
		ID: "conversation-thread-a", SourceID: "thread-a", AssetSetHash: "fixture-assets-v1", Status: domain.StatusStable,
	}, []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: rolloutInfo.Size(), ModTime: rolloutInfo.ModTime().UTC(), Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: databaseInfo.Size(), ModTime: databaseInfo.ModTime().UTC(), Status: domain.StatusStable}},
		indexFile,
	}, sagaDescriptor(), lifecycle.PlanOptions{Archive: true, PermanentDelete: true, ConfirmationCode: "4821"})
	if err != nil {
		t.Fatal(err)
	}
	journalRoot := filepath.Join(t.TempDir(), "journal")
	result, executeErr := ExecuteLocalSagaWithConfirmation(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), journalRoot, nil, "4821")
	if executeErr == nil || !result.NeedsReview || result.Phase != lifecycle.PhaseNeedsReview || !strings.Contains(executeErr.Error(), "mutation adapter") {
		t.Fatalf("expected control-plane adapter gate: result=%+v err=%v", result, executeErr)
	}
	if _, statErr := os.Stat(rolloutPath); statErr != nil {
		t.Fatalf("rollout must remain after adapter gate: %v", statErr)
	}
	if _, statErr := os.Stat(indexPath); statErr != nil {
		t.Fatalf("session index must remain after adapter gate: %v", statErr)
	}
	assertSagaSQLiteRows(t, databasePath, map[string]bool{
		"threads:thread-a":          true,
		"thread_items:item-a":       true,
		"thread_spawn_edges:edge-a": true,
	})
	entry, loadErr := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if loadErr != nil || entry.Phase != lifecycle.PhaseNeedsReview || entry.ErrorCode != "CONTROL_PLANE_ADAPTER_MISSING" {
		t.Fatalf("unexpected adapter gate journal: %+v err=%v", entry, loadErr)
	}
}

func TestExecuteLocalSagaEncryptedArchiveRequiresAndUsesPassword(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("encrypted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{
		ID: "encrypted-conversation", Status: domain.StatusStable,
	}, []domain.FileInventory{{Asset: domain.Asset{RelativePath: "rollout.jsonl", SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}, sagaDescriptor(), lifecycle.PlanOptions{
		Archive: true, Encrypted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(t.TempDir(), "journal")
	if _, err := ExecuteLocalSagaWithOptions(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), journal, nil, ExecutionOptions{}); err == nil {
		t.Fatal("expected missing archive password rejection")
	}
	result, err := ExecuteLocalSagaWithOptions(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), nil, ExecutionOptions{ArchivePassword: "密码"})
	if err != nil || result.Archive == nil || !result.Archive.Encrypted || result.Phase != lifecycle.PhaseCommitted {
		t.Fatalf("unexpected encrypted saga result: result=%+v err=%v", result, err)
	}
}

func TestRecoverLocalSagaRestoresIsolatedAssets(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("recover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, true)
	trash, err := platform.NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteLocalSaga(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), trash)
	if err == nil || !result.NeedsReview {
		t.Fatalf("expected isolated saga to require review: result=%+v err=%v", result, err)
	}
	recovered, err := RecoverLocalSaga(context.Background(), result.JournalPath, trash)
	if err != nil || recovered.Phase != lifecycle.PhaseRolledBack {
		t.Fatalf("unexpected recovery result: result=%+v err=%v", recovered, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "recover\n" {
		t.Fatalf("recovered source mismatch: %q err=%v", data, err)
	}
}

func TestExecuteLocalSagaInterruptedAfterTrashIsRecoverable(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("interrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, true)
	trash, err := platform.NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	archives := filepath.Join(t.TempDir(), "archives")
	journalRoot := filepath.Join(t.TempDir(), "journal")
	result, executeErr := ExecuteLocalSagaWithOptions(context.Background(), plan, archives, journalRoot, trash, ExecutionOptions{
		PhaseHook: func(_ context.Context, phase ExecutionPhase, _ string) error {
			if phase == PhaseTrashMoved {
				return ErrExecutionInterrupted
			}
			return nil
		},
	})
	if !errors.Is(executeErr, ErrExecutionInterrupted) || result.Phase != lifecycle.PhaseArchived {
		t.Fatalf("预期在回收站移动后中断：result=%+v err=%v", result, executeErr)
	}
	entry, err := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if err != nil || entry.Phase != lifecycle.PhaseArchived || len(entry.Receipts) != 1 || entry.CommitStatus != lifecycle.CommitPending {
		t.Fatalf("中断后 journal 证据不完整：entry=%+v err=%v", entry, err)
	}
	recovered, recoverErr := RecoverLocalSaga(context.Background(), result.JournalPath, trash)
	if recoverErr != nil || recovered.Phase != lifecycle.PhaseRolledBack {
		t.Fatalf("中断后恢复失败：result=%+v err=%v", recovered, recoverErr)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "interrupted\n" {
		t.Fatalf("中断恢复覆盖或丢失源文件：%q err=%v", data, err)
	}
}

func TestExecuteLocalSagaInterruptedAfterArchiveIsExternalUnknown(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("archive interrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, false)
	archives := filepath.Join(t.TempDir(), "archives")
	journalRoot := filepath.Join(t.TempDir(), "journal")
	result, executeErr := ExecuteLocalSagaWithOptions(context.Background(), plan, archives, journalRoot, nil, ExecutionOptions{
		PhaseHook: func(_ context.Context, phase ExecutionPhase, _ string) error {
			if phase == PhaseArchiveCreated {
				return ErrExecutionInterrupted
			}
			return nil
		},
	})
	if !errors.Is(executeErr, ErrExecutionInterrupted) || result.Phase != lifecycle.PhasePrepared {
		t.Fatalf("预期在归档生成后、回执写入前中断：result=%+v err=%v", result, executeErr)
	}
	entry, err := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if err != nil || entry.Phase != lifecycle.PhasePrepared || len(entry.Stages) != 2 || entry.Stages[1].Name != "archive" || entry.Stages[1].State != "started" {
		t.Fatalf("归档中断 journal 证据不完整：entry=%+v err=%v", entry, err)
	}
	candidate := ClassifyJournal(context.Background(), result.JournalPath, entry)
	if candidate.Disposition != RecoveryExternalUnknown || !candidate.ExternalStateUnknown || candidate.CanRetry || candidate.CanRollback {
		t.Fatalf("归档 staging 未被判为外部未知：%+v", candidate)
	}
}

func TestExecuteLocalSagaInterruptedAroundCommitIsRecoverableAndTerminal(t *testing.T) {
	for _, interruptPhase := range []ExecutionPhase{PhaseCommitStarting, PhaseCommitted} {
		t.Run(string(interruptPhase), func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("commit interrupted\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			plan := sagaPlan(t, root, false)
			result, executeErr := ExecuteLocalSagaWithOptions(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), nil, ExecutionOptions{
				PhaseHook: func(_ context.Context, phase ExecutionPhase, _ string) error {
					if phase == interruptPhase {
						return ErrExecutionInterrupted
					}
					return nil
				},
			})
			if !errors.Is(executeErr, ErrExecutionInterrupted) {
				t.Fatalf("未收到模拟突然终止：result=%+v err=%v", result, executeErr)
			}
			entry, err := lifecycle.LoadJournal(context.Background(), result.JournalPath)
			if err != nil {
				t.Fatal(err)
			}
			candidate := ClassifyJournal(context.Background(), result.JournalPath, entry)
			if interruptPhase == PhaseCommitStarting {
				if entry.Phase != lifecycle.PhaseArchived || candidate.Disposition != RecoveryRollbackable || !candidate.CanRollback {
					t.Fatalf("提交前中断未保留可回滚证据：entry=%+v candidate=%+v", entry, candidate)
				}
				if _, err := RecoverLocalSaga(context.Background(), result.JournalPath, nil); err != nil {
					t.Fatalf("提交前中断恢复失败：%v", err)
				}
			} else {
				if entry.Phase != lifecycle.PhaseCommitted || candidate.Disposition != RecoveryCommitted || candidate.CanRetry || candidate.CanRollback {
					t.Fatalf("提交后中断不应自动回滚：entry=%+v candidate=%+v", entry, candidate)
				}
				if recovered, err := RecoverLocalSaga(context.Background(), result.JournalPath, nil); err != nil || recovered.Phase != lifecycle.PhaseCommitted {
					t.Fatalf("已提交 journal 被重复恢复：result=%+v err=%v", recovered, err)
				}
			}
		})
	}
}

func TestRecoverLocalSagaIsIdempotentAfterReceiptWasAlreadyRestored(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := []byte("receipt restored before journal\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, true)
	trash, err := platform.NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	result, executeErr := ExecuteLocalSagaWithOptions(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), trash, ExecutionOptions{
		PhaseHook: func(_ context.Context, phase ExecutionPhase, _ string) error {
			if phase == PhaseTrashMoved {
				return ErrExecutionInterrupted
			}
			return nil
		},
	})
	if !errors.Is(executeErr, ErrExecutionInterrupted) || len(result.TrashReceipts) != 1 {
		t.Fatalf("未得到回收站中断结果：result=%+v err=%v", result, executeErr)
	}
	if err := trash.Restore(context.Background(), result.TrashReceipts[0], root); err != nil {
		t.Fatalf("模拟 journal 写入前的回收站恢复失败：%v", err)
	}
	recovered, err := RecoverLocalSaga(context.Background(), result.JournalPath, trash)
	if err != nil || recovered.Phase != lifecycle.PhaseRolledBack {
		t.Fatalf("已恢复回收站回执未能幂等收尾：result=%+v err=%v", recovered, err)
	}
	if restored, err := os.ReadFile(path); err != nil || string(restored) != string(content) {
		t.Fatalf("幂等恢复改变了源文件：%q err=%v", restored, err)
	}
	if repeated, err := RecoverLocalSaga(context.Background(), result.JournalPath, trash); err != nil || repeated.Phase != lifecycle.PhaseRolledBack {
		t.Fatalf("终态 journal 重复恢复不幂等：result=%+v err=%v", repeated, err)
	}
}

func TestRecoverLocalSagaRejectsReplacementWithIdenticalBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := []byte("replacement with same bytes\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, true)
	trash, err := platform.NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	result, executeErr := ExecuteLocalSagaWithOptions(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), trash, ExecutionOptions{
		PhaseHook: func(_ context.Context, phase ExecutionPhase, _ string) error {
			if phase == PhaseTrashMoved {
				return ErrExecutionInterrupted
			}
			return nil
		},
	})
	if !errors.Is(executeErr, ErrExecutionInterrupted) || len(result.TrashReceipts) != 1 {
		t.Fatalf("未得到回收站中断结果：result=%+v err=%v", result, executeErr)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	recovered, recoverErr := RecoverLocalSaga(context.Background(), result.JournalPath, trash)
	if recoverErr == nil || !recovered.NeedsReview || recovered.Phase != lifecycle.PhaseArchived {
		t.Fatalf("相同字节的新文件未被阻断：result=%+v err=%v", recovered, recoverErr)
	}
	if current, err := os.ReadFile(path); err != nil || string(current) != string(content) {
		t.Fatalf("冲突恢复覆盖了新文件：%q err=%v", current, err)
	}
}

func TestExecuteLocalSagaPermanentDeleteWithControlPlaneUsesControlDBPhase(t *testing.T) {
	root, databasePath := createSagaSQLiteFixture(t)
	_, indexFile := createSagaSessionIndex(t, root)
	databaseInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{
		ID: "permanent-control-plane", SourceID: "thread-a", AssetSetHash: "fixture-assets-v1", Status: domain.StatusStable,
	}, []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: databaseInfo.Size(), ModTime: databaseInfo.ModTime().UTC(), Status: domain.StatusStable}},
		indexFile,
	}, sagaDescriptor(), lifecycle.PlanOptions{PermanentDelete: true, ConfirmationCode: "4821"})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := compat.NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteLocalSagaWithConfirmation(context.Background(), plan, "", filepath.Join(t.TempDir(), "journal"), nil, "4821")
	if err == nil || result.Phase != lifecycle.PhaseNeedsReview {
		t.Fatalf("缺少 control-plane adapter 时不应提交永久删除：result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "session_index.jsonl")); statErr != nil {
		t.Fatalf("失败的永久删除计划改变了 session index：%v", statErr)
	}

	// Rebuild a fresh plan and execute with the adapter to cover source_removed → control_db_updated.
	root, databasePath = createSagaSQLiteFixture(t)
	_, indexFile = createSagaSessionIndex(t, root)
	databaseInfo, err = os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{
		ID: "permanent-control-plane", SourceID: "thread-a", AssetSetHash: "fixture-assets-v1", Status: domain.StatusStable,
	}, []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: databaseInfo.Size(), ModTime: databaseInfo.ModTime().UTC(), Status: domain.StatusStable}},
		indexFile,
	}, sagaDescriptor(), lifecycle.PlanOptions{PermanentDelete: true, ConfirmationCode: "4821"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = ExecuteLocalSagaWithOptions(context.Background(), plan, "", filepath.Join(t.TempDir(), "journal"), nil, ExecutionOptions{ConfirmationCode: "4821", ControlPlaneMutation: adapter})
	if err != nil || result.Phase != lifecycle.PhaseCommitted || result.Mutation == nil || !result.Mutation.Verified {
		t.Fatalf("绑定 control-plane 的永久删除未提交：result=%+v err=%v", result, err)
	}
}

func TestExecuteLocalSagaDeletesControlPlaneRowsAfterIsolatingFileAssets(t *testing.T) {
	root, databasePath := createSagaSQLiteFixture(t)
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("saga with control plane\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rolloutInfo, err := os.Stat(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	indexPath, indexFile := createSagaSessionIndex(t, root)
	databaseInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{
		ID: "conversation-thread-a", SourceID: "thread-a", AssetSetHash: "fixture-assets-v1", Status: domain.StatusStable,
	}, []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: rolloutInfo.Size(), ModTime: rolloutInfo.ModTime().UTC(), Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: databaseInfo.Size(), ModTime: databaseInfo.ModTime().UTC(), Status: domain.StatusStable}},
		indexFile,
	}, sagaDescriptor(), lifecycle.PlanOptions{Archive: true, Trash: true})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := compat.NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	trash, err := platform.NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	archives := filepath.Join(t.TempDir(), "archives")
	result, err := ExecuteLocalSagaWithOptions(context.Background(), plan, archives, filepath.Join(t.TempDir(), "journal"), trash, ExecutionOptions{ControlPlaneMutation: adapter})
	if err != nil || result.Phase != lifecycle.PhaseCommitted || result.Archive == nil || result.Mutation == nil || !result.Mutation.Verified || result.Mutation.ChangedRows != 3 || result.Mutation.ChangedFiles != 1 {
		t.Fatalf("unexpected control-plane saga: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(rolloutPath); !os.IsNotExist(err) {
		t.Fatalf("rollout file was not isolated: %v", err)
	}
	if data, err := os.ReadFile(indexPath); err != nil || string(data) != "{\"id\":\"thread-child\",\"title\":\"子线程\"}\n" {
		t.Fatalf("unexpected deleted session index: %q err=%v", data, err)
	}
	manifest, err := archive.VerifyLocal(context.Background(), result.Archive.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 1 || manifest.Assets[0].RelativePath != "rollout.jsonl" || len(manifest.ControlPlaneAssets) != 2 || !containsManifestAsset(manifest.ControlPlaneAssets, "state.sqlite") || !containsManifestAsset(manifest.ControlPlaneAssets, "session_index.jsonl") || manifest.ControlPlaneSnapshot == nil {
		t.Fatalf("unexpected archive manifest assets: %+v", manifest)
	}
	assertSagaSQLiteRows(t, databasePath, map[string]bool{
		"threads:thread-a":          false,
		"thread_items:item-a":       false,
		"thread_spawn_edges:edge-a": false,
		"threads:thread-child":      true,
	})
	entry, err := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mutation == nil || entry.Phase != lifecycle.PhaseCommitted || len(entry.Receipts) != 1 {
		t.Fatalf("unexpected committed journal: %+v", entry)
	}
}

func TestRecoverLocalSagaRestoresControlPlaneAfterMutationFailure(t *testing.T) {
	root, databasePath := createSagaSQLiteFixture(t)
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("recover control plane\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rolloutInfo, err := os.Stat(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	indexPath, indexFile := createSagaSessionIndex(t, root)
	databaseInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{
		ID: "conversation-thread-a", SourceID: "thread-a", AssetSetHash: "fixture-assets-v1", Status: domain.StatusStable,
	}, []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: rolloutInfo.Size(), ModTime: rolloutInfo.ModTime().UTC(), Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: databaseInfo.Size(), ModTime: databaseInfo.ModTime().UTC(), Status: domain.StatusStable}},
		indexFile,
	}, sagaDescriptor(), lifecycle.PlanOptions{Archive: true, Trash: true})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := compat.NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	trash, err := platform.NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteLocalSagaWithOptions(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), trash, ExecutionOptions{ControlPlaneMutation: failingMutation{inner: adapter}})
	if err == nil || !result.NeedsReview || result.Phase != lifecycle.PhaseNeedsReview {
		t.Fatalf("expected control-plane failure review: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(rolloutPath); !os.IsNotExist(err) {
		t.Fatalf("expected rollout file to remain isolated: %v", err)
	}
	if data, err := os.ReadFile(indexPath); err != nil || string(data) != "{\"id\":\"thread-child\",\"title\":\"子线程\"}\n" {
		t.Fatalf("unexpected isolated session index: %q err=%v", data, err)
	}
	assertSagaSQLiteRows(t, databasePath, map[string]bool{
		"threads:thread-a":          false,
		"thread_items:item-a":       false,
		"thread_spawn_edges:edge-a": false,
		"threads:thread-child":      true,
	})
	entry, err := lifecycle.LoadJournal(context.Background(), result.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mutation == nil || entry.Phase != lifecycle.PhaseNeedsReview || len(entry.Receipts) != 1 {
		t.Fatalf("unexpected failed saga journal: %+v", entry)
	}
	if err := os.Remove(entry.Mutation.SnapshotRef); err != nil {
		t.Fatalf("删除测试用外部控制面快照失败: %v", err)
	}
	recovered, err := RecoverLocalSagaWithOptions(context.Background(), result.JournalPath, trash, adapter)
	if err != nil || recovered.Phase != lifecycle.PhaseRolledBack || recovered.Mutation == nil || !recovered.Mutation.Verified {
		t.Fatalf("unexpected control-plane recovery: result=%+v err=%v", recovered, err)
	}
	if data, err := os.ReadFile(rolloutPath); err != nil || string(data) != "recover control plane\n" {
		t.Fatalf("recovered rollout mismatch: %q err=%v", data, err)
	}
	if data, err := os.ReadFile(indexPath); err != nil || string(data) != "{\"id\":\"thread-a\",\"title\":\"目标\"}\n{\"id\":\"thread-child\",\"title\":\"子线程\"}\n" {
		t.Fatalf("recovered session index mismatch: %q err=%v", data, err)
	}
	assertSagaSQLiteRows(t, databasePath, map[string]bool{
		"threads:thread-a":          true,
		"thread_items:item-a":       true,
		"thread_spawn_edges:edge-a": true,
		"threads:thread-child":      true,
	})
}

func createSagaSQLiteFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`
		CREATE TABLE threads(id TEXT PRIMARY KEY, title TEXT, rollout_path TEXT);
		CREATE TABLE thread_items(id TEXT PRIMARY KEY, thread_id TEXT, body TEXT);
		CREATE TABLE thread_spawn_edges(id TEXT PRIMARY KEY, parent_thread_id TEXT, child_thread_id TEXT);
		INSERT INTO threads VALUES('thread-a','原始标题','rollout.jsonl');
		INSERT INTO threads VALUES('thread-child','子线程','child.jsonl');
		INSERT INTO thread_items VALUES('item-a','thread-a','正文');
		INSERT INTO thread_items VALUES('item-child','thread-child','子正文');
		INSERT INTO thread_spawn_edges VALUES('edge-a','thread-a','thread-child');
	`)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return root, databasePath
}

func createSagaSessionIndex(t *testing.T, root string) (string, domain.FileInventory) {
	t.Helper()
	path := filepath.Join(root, "session_index.jsonl")
	if err := os.WriteFile(path, []byte("{\"id\":\"thread-a\",\"title\":\"目标\"}\n{\"id\":\"thread-child\",\"title\":\"子线程\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, domain.FileInventory{Asset: domain.Asset{RelativePath: "session_index.jsonl", Kind: domain.AssetKindSessionIndex, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}, SessionIndex: &domain.SessionIndexSummary{LineCount: 2, Entries: []domain.SessionIndexEntry{{ID: "thread-a", Line: 1}, {ID: "thread-child", Line: 2}}}}
}

func containsManifestAsset(assets []lifecycle.AssetFingerprint, relative string) bool {
	for _, asset := range assets {
		if asset.RelativePath == relative {
			return true
		}
	}
	return false
}

func assertSagaSQLiteRows(t *testing.T, databasePath string, expected map[string]bool) {
	t.Helper()
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for key, want := range expected {
		parts := strings.SplitN(key, ":", 2)
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM \""+parts[0]+"\" WHERE id=?", parts[1]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if (count > 0) != want {
			t.Fatalf("unexpected %s presence: got=%v want=%v", key, count > 0, want)
		}
	}
}
