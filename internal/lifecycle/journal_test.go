// 中文说明：验证操作 journal 的原子阶段、回执、恢复判定和源目录隔离。
package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalPersistsTransitionsAndReceipts(t *testing.T) {
	root, conversation, files := stableFixture(t)
	plan, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{Archive: true, Trash: true})
	if err != nil {
		t.Fatal(err)
	}
	entry, path, err := CreateJournal(context.Background(), filepath.Join(t.TempDir(), "journal"), plan)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Phase != PhasePrepared || !NeedsRecovery(entry) {
		t.Fatalf("unexpected initial journal: %+v", entry)
	}
	if _, err := AppendReceipt(context.Background(), path, ReceiptRecord{RelativePath: "rollout.jsonl", OriginalPath: filepath.Join(root, "rollout.jsonl"), IsolatedPath: "/trash/rollout.jsonl", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	entry, err = TransitionJournal(context.Background(), path, PhaseArchived, "", "", "")
	if err != nil || entry.Phase != PhaseArchived || len(entry.Receipts) != 1 {
		t.Fatalf("unexpected archive transition: %+v err=%v", entry, err)
	}
	entry, err = TransitionJournal(context.Background(), path, PhaseNeedsReview, "ASSET_CHANGED", "源资产变化", "重新扫描并由用户决定")
	if err != nil || entry.Phase != PhaseNeedsReview || entry.ErrorCode != "ASSET_CHANGED" {
		t.Fatalf("unexpected review transition: %+v err=%v", entry, err)
	}
	if _, err := TransitionJournal(context.Background(), path, PhasePrepared, "", "", ""); err == nil {
		t.Fatal("expected invalid backward transition")
	}
	entry, err = TransitionJournal(context.Background(), path, PhaseRolledBack, "", "", "已补偿")
	if err != nil || NeedsRecovery(entry) {
		t.Fatalf("unexpected rolled back state: %+v err=%v", entry, err)
	}
}

func TestCreateJournalRejectsCodexPathAndDuplicate(t *testing.T) {
	root, conversation, files := stableFixture(t)
	plan, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateJournal(context.Background(), filepath.Join(t.TempDir(), ".codex", "journal"), plan); err == nil {
		t.Fatal("expected .codex journal rejection")
	}
	directory := filepath.Join(t.TempDir(), "journal")
	if _, _, err := CreateJournal(context.Background(), directory, plan); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateJournal(context.Background(), directory, plan); err == nil {
		t.Fatal("expected duplicate journal rejection")
	}
}

func TestJournalUpdateDoesNotFollowLegacyTempSymlink(t *testing.T) {
	root, conversation, files := stableFixture(t)
	plan, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "journal")
	_, path, err := CreateJournal(context.Background(), directory, plan)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path+".tmp"); err != nil {
		t.Skipf("当前平台不支持创建符号链接: %v", err)
	}
	if _, err := TransitionJournal(context.Background(), path, PhaseNeedsReview, "TEST_TEMP", "测试", "测试临时文件安全"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "outside" {
		t.Fatalf("journal 更新跟随了旧临时符号链接: %q err=%v", data, err)
	}
}

func TestJournalPersistsInputOutputHashesStagesAndCommitStatus(t *testing.T) {
	root, conversation, files := stableFixture(t)
	plan, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "journal")
	entry, path, err := CreateJournal(context.Background(), directory, plan)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := PlanDigest(plan)
	if err != nil || entry.InputPlanSHA256 != wantDigest || entry.CommitStatus != CommitPending {
		t.Fatalf("journal 输入绑定不完整：entry=%+v digest=%s err=%v", entry, wantDigest, err)
	}
	entry, err = RecordStage(context.Background(), path, StageRecord{Name: "archive", State: "completed", Path: filepath.Join(t.TempDir(), "archive"), InputSHA256: plan.AssetSetHash, OutputSHA256: "output-hash", Note: "测试阶段"})
	if err != nil || len(entry.Stages) != 1 || entry.Stages[0].OutputSHA256 != "output-hash" {
		t.Fatalf("journal 阶段记录不完整：entry=%+v err=%v", entry, err)
	}
	entry, err = TransitionJournal(context.Background(), path, PhaseArchived, "", "", "测试归档阶段")
	if err != nil || entry.Phase != PhaseArchived {
		t.Fatalf("journal 合法阶段转换失败：entry=%+v err=%v", entry, err)
	}
	entry, err = TransitionJournal(context.Background(), path, PhaseCommitted, "", "", "测试提交")
	if err != nil || entry.CommitStatus != CommitCommitted || !entry.UpdatedAt.After(entry.CreatedAt) {
		t.Fatalf("journal 提交状态不完整：entry=%+v err=%v", entry, err)
	}
	loaded, err := LoadJournal(context.Background(), path)
	if err != nil || loaded.InputPlanSHA256 != wantDigest || loaded.CommitStatus != CommitCommitted || len(loaded.Stages) != 1 {
		t.Fatalf("重新读取 journal 丢失证据：entry=%+v err=%v", loaded, err)
	}
}
