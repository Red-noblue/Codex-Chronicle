// 中文说明：验证生命周期计划只生成 dry-run、拒绝不安全状态，并能检测执行前指纹变化。
package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

func writableDescriptor() compat.Descriptor {
	return compat.Descriptor{
		ID: "fixture-writable", SupportedVersions: []string{"fixture-v1"},
		Capabilities: map[compat.Capability]bool{
			compat.CapabilityRead: true, compat.CapabilityArchiveExport: true,
			compat.CapabilityDelete: true, compat.CapabilityVerify: true,
		},
	}
}

func stableFixture(t *testing.T) (string, domain.ConversationSummary, []domain.FileInventory) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"fixture-thread\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	conversation := domain.ConversationSummary{
		ID: "conversation-fixture", AssetSetHash: "asset-hash", Status: domain.StatusStable,
		RolloutPaths: []string{"rollout.jsonl"},
	}
	files := []domain.FileInventory{{
		Asset:             domain.Asset{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable},
		ChangedDuringScan: false,
	}}
	return root, conversation, files
}

func TestBuildPlanProducesReviewableManifestWithoutSourceWrite(t *testing.T) {
	root, conversation, files := stableFixture(t)
	before, err := os.ReadFile(filepath.Join(root, "rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{Archive: true, Trash: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != PlanPrepared || !plan.ReadOnly || len(plan.Assets) != 1 || len(plan.Actions) != 2 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	manifest, err := MarshalManifest(plan)
	if err != nil || len(manifest) == 0 {
		t.Fatalf("manifest failed: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("plan changed source content")
	}
}

func TestFileAssetsAlwaysKeepsTypedControlPlaneAssetsOut(t *testing.T) {
	plan := OperationPlan{Assets: []AssetFingerprint{
		{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL},
		{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite},
		{RelativePath: "session_index.jsonl", Kind: domain.AssetKindSessionIndex},
		{RelativePath: "legacy-control.bin", Kind: domain.AssetKindUnknown, ControlPlaneOnly: true},
	}}
	assets := plan.FileAssets()
	if len(assets) != 1 || assets[0].RelativePath != "rollout.jsonl" {
		t.Fatalf("typed control-plane assets escaped file operation set: %+v", assets)
	}
}

func TestBuildPlanRejectsUnknownStateAndReadonlyAdapter(t *testing.T) {
	root, conversation, files := stableFixture(t)
	conversation.Status = domain.StatusUnknown
	if _, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{Trash: true}); err == nil {
		t.Fatal("expected unknown conversation rejection")
	}
	conversation.Status = domain.StatusStable
	plan, err := BuildPlan(context.Background(), root, conversation, files, compat.GenericJSONLDescriptor(), PlanOptions{Trash: true})
	if err == nil {
		t.Fatal("expected readonly adapter rejection")
	}
	if len(plan.Assets) != 1 || plan.Status != PlanInvalid {
		t.Fatalf("readonly preview lost asset evidence: plan=%+v err=%v", plan, err)
	}
}

func TestRevalidatePlanDetectsChangedAsset(t *testing.T) {
	root, conversation, files := stableFixture(t)
	plan, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RevalidatePlan(context.Background(), plan); err == nil {
		t.Fatal("expected changed fingerprint rejection")
	}
}

func TestBuildPlanRequiresNumericConfirmationForPermanentDelete(t *testing.T) {
	root, conversation, files := stableFixture(t)
	for _, code := range []string{"", "abc", "123"} {
		if _, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{PermanentDelete: true, ConfirmationCode: code}); err == nil {
			t.Fatalf("expected confirmation rejection for %q", code)
		}
	}
	plan, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{PermanentDelete: true, ConfirmationCode: "4821"})
	if err != nil || !plan.RequiresConfirm || len(plan.Actions) != 1 {
		t.Fatalf("unexpected permanent delete plan: %+v err=%v", plan, err)
	}
	if plan.ConfirmationDigest == "" || !VerifyConfirmation(plan, "4821") || VerifyConfirmation(plan, "4822") {
		t.Fatalf("confirmation digest did not bind the plan to the code: %+v", plan)
	}
	if plan.CreatedAt.After(time.Now().UTC()) {
		t.Fatal("plan creation time is in the future")
	}
}

func TestBuildPlanRejectsTrashAndPermanentDeleteTogether(t *testing.T) {
	root, conversation, files := stableFixture(t)
	if _, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{
		Trash: true, PermanentDelete: true, ConfirmationCode: "4821",
	}); err == nil {
		t.Fatal("expected mutually exclusive destructive actions to be rejected")
	}
}

func TestBuildPlanRejectsSharedAssetsByDefault(t *testing.T) {
	root, conversation, files := stableFixture(t)
	plan, err := BuildPlan(context.Background(), root, conversation, files, writableDescriptor(), PlanOptions{
		Trash: true, AssetOwners: map[string]int{"rollout.jsonl": 2},
	})
	if err == nil || plan.Status != PlanInvalid || len(plan.Assets) != 0 {
		t.Fatalf("expected shared asset rejection before fingerprinting: plan=%+v err=%v", plan, err)
	}
}

func TestBuildPlanAllowsOnlyDeclaredSharedAssetSplit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session_index.jsonl")
	if err := os.WriteFile(path, []byte("{\"id\":\"thread-a\"}\n{\"id\":\"thread-b\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	conversation := domain.ConversationSummary{ID: "conversation-a", AssetSetHash: "asset-hash", Status: domain.StatusStable}
	files := []domain.FileInventory{{Asset: domain.Asset{RelativePath: "session_index.jsonl", Kind: domain.AssetKindSessionIndex, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}
	descriptor := writableDescriptor()
	descriptor.SharedAssetKinds = []domain.AssetKind{domain.AssetKindSessionIndex}
	plan, err := BuildPlan(context.Background(), root, conversation, files, descriptor, PlanOptions{Trash: true, AssetOwners: map[string]int{"session_index.jsonl": 2}})
	if err != nil || plan.Status != PlanPrepared || len(plan.SharedAssetPaths) != 1 || plan.SharedAssetPaths[0] != "session_index.jsonl" {
		t.Fatalf("已声明按行拆分的共享 session index 不应被阻断: plan=%+v err=%v", plan, err)
	}
}

func TestRebaseSharedAssetFingerprintsUpdatesOnlyDeclaredSharedAssets(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "session_index.jsonl")
	firstRollout := filepath.Join(root, "first.jsonl")
	secondRollout := filepath.Join(root, "second.jsonl")
	for path, content := range map[string]string{
		indexPath:     "{\"id\":\"thread-a\"}\n{\"id\":\"thread-b\"}\n",
		firstRollout:  "first conversation\n",
		secondRollout: "second conversation\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	descriptor := writableDescriptor()
	descriptor.SharedAssetKinds = []domain.AssetKind{domain.AssetKindSessionIndex}
	build := func(id, rollout string) OperationPlan {
		info, err := os.Stat(filepath.Join(root, rollout))
		if err != nil {
			t.Fatal(err)
		}
		indexInfo, err := os.Stat(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		conversation := domain.ConversationSummary{ID: id, AssetSetHash: "asset-hash", Status: domain.StatusStable}
		files := []domain.FileInventory{
			{Asset: domain.Asset{RelativePath: "session_index.jsonl", Kind: domain.AssetKindSessionIndex, SizeBytes: indexInfo.Size(), ModTime: indexInfo.ModTime().UTC(), Status: domain.StatusStable}},
			{Asset: domain.Asset{RelativePath: rollout, Kind: domain.AssetKindJSONL, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}},
		}
		plan, err := BuildPlan(context.Background(), root, conversation, files, descriptor, PlanOptions{Trash: true, AssetOwners: map[string]int{"session_index.jsonl": 2}})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	completed := build("conversation-a", "first.jsonl")
	pending := build("conversation-b", "second.jsonl")
	if err := os.WriteFile(indexPath, []byte("{\"id\":\"thread-b\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RebaseSharedAssetFingerprints(context.Background(), descriptor, completed, []*OperationPlan{&pending}); err != nil {
		t.Fatalf("共享资产重绑定失败: %v", err)
	}
	if err := RevalidatePlan(context.Background(), pending); err != nil {
		t.Fatalf("只变化已授权共享资产时后续计划应仍有效: %v", err)
	}
	if err := os.WriteFile(secondRollout, []byte("changed pending conversation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RevalidatePlan(context.Background(), pending); err == nil {
		t.Fatal("共享资产重绑定不得放宽非共享对话资产的指纹检查")
	}
}

func TestRebaseSharedAssetFingerprintsRequiresBothPlansToDeclarePath(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "session_index.jsonl")
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(indexPath, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolloutPath, []byte("conversation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	indexInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	rolloutInfo, err := os.Stat(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := writableDescriptor()
	descriptor.SharedAssetKinds = []domain.AssetKind{domain.AssetKindSessionIndex}
	files := []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "session_index.jsonl", Kind: domain.AssetKindSessionIndex, SizeBytes: indexInfo.Size(), ModTime: indexInfo.ModTime().UTC(), Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: rolloutInfo.Size(), ModTime: rolloutInfo.ModTime().UTC(), Status: domain.StatusStable}},
	}
	build := func(id string) OperationPlan {
		plan, err := BuildPlan(context.Background(), root, domain.ConversationSummary{ID: id, Status: domain.StatusStable}, files, descriptor, PlanOptions{Trash: true, AssetOwners: map[string]int{"session_index.jsonl": 2}})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	completed, pending := build("conversation-a"), build("conversation-b")
	pending.SharedAssetPaths = nil
	if err := os.WriteFile(indexPath, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RebaseSharedAssetFingerprints(context.Background(), descriptor, completed, []*OperationPlan{&pending}); err != nil {
		t.Fatalf("未共同声明的路径不应触发指纹更新错误: %v", err)
	}
	if err := RevalidatePlan(context.Background(), pending); err == nil {
		t.Fatal("未共同声明的共享路径不得被重绑定")
	}
}
