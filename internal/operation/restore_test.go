// 中文说明：验证本地归档恢复的预览、独立 journal、控制面绑定和人工复核边界。
package operation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/platform"
)

type restoreFailingMutation struct {
	compat.ControlPlaneMutation
}

func (f restoreFailingMutation) Restore(_ context.Context, snapshot compat.MutationSnapshot) (compat.MutationOutcome, error) {
	return compat.MutationOutcome{Action: compat.MutationRestore, AdapterID: snapshot.AdapterID, SnapshotHash: snapshot.SnapshotHash, NeedsReview: true}, fmt.Errorf("注入 control-plane 恢复失败")
}

func TestPreviewAndExecuteStandaloneArchiveRestore(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("standalone restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, false)
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	created, err := createArchiveForRestoreTest(t, plan, archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	restorePlan, err := PreviewArchiveRestore(context.Background(), created.Directory, target, "", "")
	if err != nil || !restorePlan.Preview.Ready || !restorePlan.ControlPlaneBound {
		t.Fatalf("unexpected standalone restore preview: %+v err=%v", restorePlan, err)
	}
	result, err := ExecuteArchiveRestore(context.Background(), restorePlan, filepath.Join(t.TempDir(), "journal"), RestoreExecutionOptions{})
	if err != nil || result.Phase != lifecycle.RestorePhaseCommitted || result.NeedsReview {
		t.Fatalf("unexpected standalone restore result: %+v err=%v", result, err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "rollout.jsonl")); err != nil || string(data) != "standalone restore\n" {
		t.Fatalf("restored standalone file mismatch: %q err=%v", data, err)
	}
	entry, err := lifecycle.LoadRestoreJournal(context.Background(), result.RestoreJournalPath)
	if err != nil || entry.Phase != lifecycle.RestorePhaseCommitted || entry.Mutation != nil {
		t.Fatalf("unexpected restore journal: %+v err=%v", entry, err)
	}
}

func TestArchiveRestoreBindsDeletionJournalAndControlPlane(t *testing.T) {
	root, databasePath := createSagaSQLiteFixture(t)
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("restore control plane\n"), 0o600); err != nil {
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
	deletion, err := ExecuteLocalSagaWithOptions(context.Background(), plan, archives, filepath.Join(t.TempDir(), "delete-journal"), trash, ExecutionOptions{ControlPlaneMutation: adapter})
	if err != nil || deletion.Phase != lifecycle.PhaseCommitted || deletion.Archive == nil {
		t.Fatalf("unexpected source deletion saga: %+v err=%v", deletion, err)
	}
	restorePlan, err := PreviewArchiveRestore(context.Background(), deletion.Archive.Directory, root, deletion.JournalPath, "")
	if err != nil || !restorePlan.Preview.Ready || !restorePlan.ControlPlaneBound || restorePlan.Mutation == nil {
		t.Fatalf("unexpected bound restore preview: %+v err=%v", restorePlan, err)
	}
	restoreJournalRoot := filepath.Join(t.TempDir(), "restore-journal")
	failed, err := ExecuteArchiveRestore(context.Background(), restorePlan, restoreJournalRoot, RestoreExecutionOptions{ControlPlaneMutation: restoreFailingMutation{ControlPlaneMutation: adapter}})
	if err == nil || !failed.NeedsReview || failed.Phase != lifecycle.RestorePhaseNeedsReview {
		t.Fatalf("control-plane failure did not enter review: %+v err=%v", failed, err)
	}
	result, err := RecoverArchiveRestore(context.Background(), failed.RestoreJournalPath, RestoreExecutionOptions{ControlPlaneMutation: adapter})
	if err != nil || result.Phase != lifecycle.RestorePhaseCommitted || result.Mutation == nil || !result.Mutation.Verified {
		t.Fatalf("unexpected resumed bound restore result: %+v err=%v", result, err)
	}
	if data, err := os.ReadFile(rolloutPath); err != nil || string(data) != "restore control plane\n" {
		t.Fatalf("restored rollout mismatch: %q err=%v", data, err)
	}
	if data, err := os.ReadFile(indexPath); err != nil || string(data) != "{\"id\":\"thread-a\",\"title\":\"目标\"}\n{\"id\":\"thread-child\",\"title\":\"子线程\"}\n" {
		t.Fatalf("restored session index mismatch: %q err=%v", data, err)
	}
	assertSagaSQLiteRows(t, databasePath, map[string]bool{
		"threads:thread-a": true, "thread_items:item-a": true, "thread_spawn_edges:edge-a": true, "threads:thread-child": true,
	})
	restoreEntry, err := lifecycle.LoadRestoreJournal(context.Background(), result.RestoreJournalPath)
	if err != nil || restoreEntry.Phase != lifecycle.RestorePhaseCommitted || restoreEntry.Mutation == nil {
		t.Fatalf("unexpected committed restore journal: %+v err=%v", restoreEntry, err)
	}
}

func TestRecoverArchiveRestoreResumesPreparedJournal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("resume prepared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, false)
	created, err := createArchiveForRestoreTest(t, plan, filepath.Join(t.TempDir(), "archives"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	restorePlan, err := PreviewArchiveRestore(context.Background(), created.Directory, root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	assets := make([]lifecycle.RestoreAsset, 0, len(restorePlan.Preview.Files))
	for _, file := range restorePlan.Preview.Files {
		assets = append(assets, lifecycle.RestoreAsset{RelativePath: file.RelativePath, TargetPath: file.TargetPath, SizeBytes: file.SizeBytes, SHA256: file.SHA256})
	}
	entry := lifecycle.RestoreJournalEntry{RestoreOperationID: restorePlan.RestoreOperationID, ArchivePath: restorePlan.ArchivePath, TargetRoot: restorePlan.TargetRoot, Plan: restorePlan.SourcePlan, Assets: assets}
	_, journalPath, err := lifecycle.CreateRestoreJournal(context.Background(), filepath.Join(t.TempDir(), "restore-journal"), entry)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RecoverArchiveRestore(context.Background(), journalPath, RestoreExecutionOptions{})
	if err != nil || result.Phase != lifecycle.RestorePhaseCommitted || result.NeedsReview {
		t.Fatalf("prepared restore journal did not resume: %+v err=%v", result, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "resume prepared\n" {
		t.Fatalf("resumed file mismatch: %q err=%v", data, err)
	}
}

func TestRecoverArchiveRestoreRejectsPartialTarget(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "one.jsonl")
	second := filepath.Join(nested, "two.jsonl")
	if err := os.WriteFile(first, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "one.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: 4, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "nested/two.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: 4, Status: domain.StatusStable}},
	}
	for index := range files {
		info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(files[index].RelativePath)))
		if statErr != nil {
			t.Fatal(statErr)
		}
		files[index].Asset.ModTime = info.ModTime().UTC()
	}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{ID: "partial-restore", Status: domain.StatusStable}, files, sagaDescriptor(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	created, err := createArchiveForRestoreTest(t, plan, filepath.Join(t.TempDir(), "archives"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "one.jsonl"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restorePlan, err := PreviewArchiveRestore(context.Background(), created.Directory, root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	assets := make([]lifecycle.RestoreAsset, 0, len(restorePlan.Preview.Files))
	for _, file := range restorePlan.Preview.Files {
		assets = append(assets, lifecycle.RestoreAsset{RelativePath: file.RelativePath, TargetPath: file.TargetPath, SizeBytes: file.SizeBytes, SHA256: file.SHA256})
	}
	entry := lifecycle.RestoreJournalEntry{RestoreOperationID: restorePlan.RestoreOperationID, ArchivePath: restorePlan.ArchivePath, TargetRoot: restorePlan.TargetRoot, Plan: restorePlan.SourcePlan, Assets: assets}
	_, journalPath, err := lifecycle.CreateRestoreJournal(context.Background(), filepath.Join(t.TempDir(), "restore-journal"), entry)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RecoverArchiveRestore(context.Background(), journalPath, RestoreExecutionOptions{})
	if err == nil || !result.NeedsReview || result.Phase != lifecycle.RestorePhaseNeedsReview {
		t.Fatalf("partial target was not held for review: %+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "nested", "two.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("partial recovery unexpectedly published missing file: %v", err)
	}
}

func createArchiveForRestoreTest(t *testing.T, plan lifecycle.OperationPlan, destination string) (archive.Result, error) {
	t.Helper()
	return archive.CreateLocal(context.Background(), plan, destination)
}
