// 中文说明：把脱敏真实形状 fixture、稳定副本制作、真实副本 adapter 和本地生命周期恢复串成隔离回归。
package operation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/application"
	compatpkg "codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/copy"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/platform"
)

const realCopyIntegrationThreadID = "00000000-0000-4000-8000-000000000001"

func TestRealCopyLifecycleArchiveTrashAndRestore(t *testing.T) {
	root := createRealCopyIntegrationRoot(t)
	ctx := context.Background()

	scanner := application.NewScanner()
	report, err := scanner.Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	conversationID := compatpkg.StableConversationID(realCopyIntegrationThreadID)
	conversation, assets, owners, found := integrationConversationAssets(report, conversationID)
	if !found {
		t.Fatalf("稳定副本中没有找到目标对话: %+v", report.Conversations)
	}
	if conversation.Status != domain.StatusStable || len(assets) == 0 {
		t.Fatalf("稳定副本对话状态或资产异常: status=%s assets=%d diagnostics=%+v", conversation.Status, len(assets), conversation.Diagnostics)
	}

	adapter, err := compatpkg.NewRealShapeCopyMutation(filepath.Join(t.TempDir(), "mutation-snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	journalRoot := filepath.Join(t.TempDir(), "journals")
	trashRoot := filepath.Join(t.TempDir(), "trash")
	trash, err := platform.NewDirectoryTrash(trashRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(ctx, root, conversation, assets, compatpkg.RealShapeCopyDescriptor(), lifecycle.PlanOptions{
		Archive: true, Trash: true, AssetOwners: owners,
	})
	if err != nil {
		t.Fatalf("生成真实副本生命周期计划失败: %v", err)
	}
	result, err := ExecuteLocalSagaWithOptions(ctx, plan, archiveRoot, journalRoot, trash, ExecutionOptions{ControlPlaneMutation: adapter})
	if err != nil {
		t.Fatalf("真实副本生命周期执行失败: %v result=%+v", err, result)
	}
	if result.Phase != lifecycle.PhaseCommitted || result.Archive == nil || result.Mutation == nil || !result.Mutation.Verified || len(result.TrashReceipts) != 1 {
		t.Fatalf("真实副本生命周期结果不完整: %+v", result)
	}
	if _, err := compatpkg.InspectRealShapeCopy(ctx, root, realCopyIntegrationThreadID); err == nil {
		t.Fatal("控制面和 rollout 删除后仍被错误识别为完整真实副本")
	}

	restorePlan, err := PreviewArchiveRestore(ctx, result.Archive.Directory, root, result.JournalPath, "")
	if err != nil {
		t.Fatalf("真实副本归档恢复预览失败: %v", err)
	}
	restored, err := ExecuteArchiveRestore(ctx, restorePlan, filepath.Join(t.TempDir(), "restore-journals"), RestoreExecutionOptions{ControlPlaneMutation: adapter})
	if err != nil {
		t.Fatalf("真实副本归档恢复失败: %v result=%+v", err, restored)
	}
	if restored.Phase != lifecycle.RestorePhaseCommitted || restored.Files == nil || restored.Mutation == nil || !restored.Mutation.Verified {
		t.Fatalf("真实副本恢复结果不完整: %+v", restored)
	}
	if _, err := compatpkg.InspectRealShapeCopy(ctx, root, realCopyIntegrationThreadID); err != nil {
		t.Fatalf("恢复后的真实副本仍不可读: %v", err)
	}
}

func createRealCopyIntegrationRoot(t *testing.T) string {
	t.Helper()
	python := "python3"
	if _, err := exec.LookPath(python); err != nil {
		python = "python"
		if _, err := exec.LookPath(python); err != nil {
			t.Skip("未找到 Python 解释器，跳过真实副本生命周期集成回归")
		}
	}
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Clean(filepath.Join(workingDirectory, "..", "..", "docs", "项目验证", "codex_realshape_fixture.py"))
	if configured := strings.TrimSpace(os.Getenv("CODEX_CHRONICLE_REALSHAPE_FIXTURE")); configured != "" {
		script = configured
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("未找到脱敏真实形状 fixture 生成器: %v", err)
	}
	command := exec.Command(python, script, "--root", source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("生成真实副本集成 fixture 失败: %v\n%s", err, output)
	}
	destination := filepath.Join(base, "codex-copy")
	marker := copy.MarkerSpec{
		Kind: copy.ManifestKind, AdapterID: compatpkg.RealShapeCopyKind,
		SchemaSHA256: compatpkg.RealShapeFixtureSchemaSHA256, SQLiteSemanticsSHA256: compatpkg.RealShapeSQLiteSemanticsSHA256,
	}
	plan, err := copy.Prepare(context.Background(), copy.CopyRequest{
		SourceRoot: source, DestinationRoot: destination, Marker: marker, AllowUnavailableProcessProbe: true,
	})
	if err != nil {
		t.Fatalf("准备真实副本复制计划失败: %v", err)
	}
	if result, err := copy.Execute(context.Background(), plan, copy.ExecuteOptions{}); err != nil || !result.Published {
		t.Fatalf("发布真实副本失败: result=%+v err=%v", result, err)
	}
	if report, err := copy.VerifyDestination(context.Background(), destination); err != nil || !report.Verified {
		t.Fatalf("验证真实副本失败: report=%+v err=%v", report, err)
	}
	return destination
}

func integrationConversationAssets(report domain.InventoryReport, conversationID string) (domain.ConversationSummary, []domain.FileInventory, map[string]int, bool) {
	var conversation domain.ConversationSummary
	for _, candidate := range report.Conversations {
		if candidate.ID == conversationID {
			conversation = candidate
			break
		}
	}
	if conversation.ID == "" {
		return domain.ConversationSummary{}, nil, nil, false
	}
	paths := make(map[string]struct{})
	for _, relative := range append(append([]string{}, conversation.RolloutPaths...), conversation.RelatedAssetPaths...) {
		paths[relative] = struct{}{}
	}
	assets := make([]domain.FileInventory, 0, len(paths))
	for _, file := range report.Files {
		if _, ok := paths[file.RelativePath]; ok {
			assets = append(assets, file)
		}
	}
	owners := make(map[string]int, len(paths))
	for relative := range paths {
		owners[relative] = 1
	}
	return conversation, assets, owners, true
}
