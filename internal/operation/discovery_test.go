// 中文说明：验证重启后生命周期 journal 的恢复分类不会把未知外部状态误判为可自动执行。
package operation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

func TestClassifyJournalDistinguishesRecoveryStates(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("recovery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 这里直接复用 saga 测试构造器，避免把正文或真实 .codex 引入恢复分类测试。
	plan := simpleRecoveryPlan(t, root)
	directory := filepath.Join(t.TempDir(), "journals")
	_, journalPath, err := lifecycle.CreateJournal(context.Background(), directory, plan)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := lifecycle.LoadJournal(context.Background(), journalPath)
	if err != nil {
		t.Fatal(err)
	}
	classified := ClassifyJournal(context.Background(), journalPath, entry)
	if classified.Disposition != RecoveryNotStarted || !classified.CanRetry || classified.CanRollback {
		t.Fatalf("prepared journal 分类错误：%+v", classified)
	}
	entry, err = lifecycle.TransitionJournal(context.Background(), journalPath, lifecycle.PhaseSourceRemoved, "", "", "测试")
	if err != nil {
		t.Fatal(err)
	}
	classified = ClassifyJournal(context.Background(), journalPath, entry)
	if classified.Disposition != RecoveryNeedsReview || classified.CanRetry || classified.CanRollback {
		t.Fatalf("source_removed 分类错误：%+v", classified)
	}
	entry, err = lifecycle.TransitionJournal(context.Background(), journalPath, lifecycle.PhaseCommitted, "", "", "测试")
	if err != nil {
		t.Fatal(err)
	}
	classified = ClassifyJournal(context.Background(), journalPath, entry)
	if classified.Disposition != RecoveryCommitted || classified.CanRetry || classified.CanRollback {
		t.Fatalf("committed 分类错误：%+v", classified)
	}
}

func TestClassifyJournalBlocksArchiveStagingAndControlPlaneUnknownState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("staging\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := simpleRecoveryPlan(t, root)
	journalRoot := filepath.Join(t.TempDir(), "journals")
	_, journalPath, err := lifecycle.CreateJournal(context.Background(), journalRoot, plan)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "archives", plan.OperationID)
	if _, err := lifecycle.RecordStage(context.Background(), journalPath, lifecycle.StageRecord{Name: "archive", State: "started", Path: archivePath}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archivePath, 0o700); err != nil {
		t.Fatal(err)
	}
	entry, err := lifecycle.LoadJournal(context.Background(), journalPath)
	if err != nil {
		t.Fatal(err)
	}
	classified := ClassifyJournal(context.Background(), journalPath, entry)
	if classified.Disposition != RecoveryExternalUnknown || !classified.ExternalStateUnknown || classified.CanRetry || classified.CanRollback {
		t.Fatalf("归档 staging 残留未被阻断：%+v", classified)
	}

	// 没有回收站 receipt 时，控制面阶段不能被猜测为可回滚。
	if err := os.RemoveAll(archivePath); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.TransitionJournal(context.Background(), journalPath, lifecycle.PhaseIsolated, "", "", "测试控制面阶段"); err != nil {
		t.Fatal(err)
	}
	entry, err = lifecycle.TransitionJournal(context.Background(), journalPath, lifecycle.PhaseControlDB, "", "", "测试控制面提交前中断")
	if err != nil {
		t.Fatal(err)
	}
	classified = ClassifyJournal(context.Background(), journalPath, entry)
	if classified.Disposition != RecoveryExternalUnknown || !classified.ExternalStateUnknown || classified.CanRetry || classified.CanRollback {
		t.Fatalf("无回执控制面阶段未被判为外部未知：%+v", classified)
	}
}

func simpleRecoveryPlan(t *testing.T, root string) lifecycle.OperationPlan {
	t.Helper()
	path := filepath.Join(root, "rollout.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle.OperationPlan{OperationID: "operation-recovery-discovery", CreatedAt: info.ModTime().UTC(), Root: root, ConversationID: "recovery-discovery", AssetSetHash: "asset-hash", AdapterID: "fixture", Status: lifecycle.PlanPrepared, Assets: []lifecycle.AssetFingerprint{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), SHA256: "placeholder"}}, Actions: []lifecycle.Action{lifecycle.ActionArchive}, ReadOnly: true}
}
