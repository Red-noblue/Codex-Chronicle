// 中文说明：验证离线更新清单、版本切换、健康失败回滚和中断恢复边界。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/maintenance"
)

func TestOfflineUpdateActivateRollbackAndRestart(t *testing.T) {
	ctx := context.Background()
	bundleV1 := makeBundle(t, "1.0.0", "版本一")
	bundleV2 := makeBundle(t, "2.0.0", "版本二")
	install, err := NewManager(filepath.Join(t.TempDir(), "install"))
	if err != nil {
		t.Fatal(err)
	}
	health := func(_ context.Context, root string) error {
		data, err := os.ReadFile(filepath.Join(root, "bin", "codex-chronicle"))
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return errors.New("可执行文件为空")
		}
		return nil
	}

	plan1, err := install.Prepare(ctx, bundleV1, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan1.FileCount != 1 || plan1.TotalBytes != int64(len("版本一")) {
		t.Fatalf("unexpected v1 plan: %+v", plan1)
	}
	result1, err := install.ActivateByID(ctx, plan1.ID, health)
	if err != nil || !result1.Verified || result1.ActiveVersion != "1.0.0" {
		t.Fatalf("v1 activation failed: %+v err=%v", result1, err)
	}

	plan2, err := install.Prepare(ctx, bundleV2, false)
	if err != nil {
		t.Fatal(err)
	}
	result2, err := install.ActivateByID(ctx, plan2.ID, health)
	if err != nil || result2.ActiveVersion != "2.0.0" || result2.PreviousVersion != "1.0.0" {
		t.Fatalf("v2 activation failed: %+v err=%v", result2, err)
	}
	status, err := install.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State.ActiveVersion != "2.0.0" || status.State.PreviousVersion != "1.0.0" || len(status.Versions) != 2 || status.RecoveryRequired {
		t.Fatalf("unexpected active status: %+v", status)
	}

	restarted, err := NewManager(install.Root())
	if err != nil {
		t.Fatal(err)
	}
	rolledBack, err := restarted.Rollback(ctx, health)
	if err != nil || rolledBack.ActiveVersion != "1.0.0" || rolledBack.PreviousVersion != "2.0.0" {
		t.Fatalf("rollback failed: %+v err=%v", rolledBack, err)
	}
	status, err = restarted.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State.ActiveVersion != "1.0.0" || status.State.LastStatus != "rolled_back" {
		t.Fatalf("unexpected rollback status: %+v", status)
	}
}

func TestOfflineUpdateHealthFailureKeepsOldVersion(t *testing.T) {
	ctx := context.Background()
	install, err := NewManager(filepath.Join(t.TempDir(), "install"))
	if err != nil {
		t.Fatal(err)
	}
	first := makeBundle(t, "1.0.0", "稳定版本")
	plan, err := install.Prepare(ctx, first, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.ActivateByID(ctx, plan.ID, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	failingBundle := makeBundle(t, "2.0.0", "失败版本")
	failingPlan, err := install.Prepare(ctx, failingBundle, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.ActivateByID(ctx, failingPlan.ID, func(context.Context, string) error { return errors.New("健康检查失败") }); err == nil {
		t.Fatal("expected health check failure")
	}
	status, err := install.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State.ActiveVersion != "1.0.0" || status.State.LastStatus != "failed" || status.RecoveryRequired {
		t.Fatalf("health failure changed active state: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(install.Root(), VersionDir, "2.0.0")); !os.IsNotExist(err) {
		t.Fatalf("failed version directory was not cleaned: %v", err)
	}
}

func TestOfflineUpdateRegistersFailedStagingForCleanup(t *testing.T) {
	ctx := context.Background()
	install, err := NewManager(filepath.Join(t.TempDir(), "install"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := maintenance.OpenRegistry(filepath.Join(t.TempDir(), "cleanup-registry"))
	if err != nil {
		t.Fatal(err)
	}
	install.SetArtifactRegistrar(func(ctx context.Context, manifest maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error) {
		stored, _, registerErr := registry.RegisterArtifact(ctx, manifest)
		return stored, registerErr
	})
	bundle := makeBundle(t, "1.0.0", "失败候选")
	plan, err := install.Prepare(ctx, bundle, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.ActivateByID(ctx, plan.ID, func(context.Context, string) error { return errors.New("健康检查失败") }); err == nil {
		t.Fatal("expected health check failure")
	}
	entries, err := os.ReadDir(filepath.Join(registry.Root, "manifests"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed update manifest was not registered: entries=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(registry.Root, "manifests", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var manifest maintenance.ArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != maintenance.ArtifactFailedArchive || manifest.State != "failed" {
		t.Fatalf("unexpected failed staging manifest: %+v", manifest)
	}
	if _, err := os.Stat(manifest.TargetPath); err != nil {
		t.Fatalf("failed staging was removed before cleanup could inspect it: %v", err)
	}
}

func TestOfflineUpdateRejectsBundleTamperingAndDowngrade(t *testing.T) {
	ctx := context.Background()
	install, err := NewManager(filepath.Join(t.TempDir(), "install"))
	if err != nil {
		t.Fatal(err)
	}
	v2 := makeBundle(t, "2.0.0", "二")
	plan, err := install.Prepare(ctx, v2, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.ActivateByID(ctx, plan.ID, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	v1 := makeBundle(t, "1.0.0", "一")
	if _, err := install.Prepare(ctx, v1, false); err == nil || !strings.Contains(err.Error(), "低于当前版本") {
		t.Fatalf("downgrade was not rejected: %v", err)
	}
	v3 := makeBundle(t, "3.0.0", "三")
	tamperedPlan, err := install.Prepare(ctx, v3, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v3, "bin", "codex-chronicle"), []byte("篡改"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := install.ActivateByID(ctx, tamperedPlan.ID, func(context.Context, string) error { return nil }); err == nil || (!strings.Contains(err.Error(), "摘要") && !strings.Contains(err.Error(), "大小")) {
		t.Fatalf("tampered bundle was accepted: %v", err)
	}
	status, err := install.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State.ActiveVersion != "2.0.0" {
		t.Fatalf("tamper changed active version: %+v", status)
	}
}

func TestOfflineUpdateRecoversPublishedJournalWithoutChangingState(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "install")
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	versionRoot := filepath.Join(root, VersionDir, "2.0.0")
	if err := os.MkdirAll(versionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, StateName), []byte(`{"active_version":"1.0.0","updated_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, JournalName), []byte(`{"operation_id":"update-test","plan_id":"plan-test","version":"2.0.0","phase":"published"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.RecoveryRequired {
		t.Fatal("interrupted journal was not surfaced")
	}
	if err := manager.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.RecoveryRequired || status.State.ActiveVersion != "1.0.0" {
		t.Fatalf("recovery changed state unexpectedly: %+v", status)
	}
	if _, err := os.Stat(versionRoot); !os.IsNotExist(err) {
		t.Fatalf("published interrupted version was not removed: %v", err)
	}
}

func TestUpdateRecoversDirectoryPublishAndStateCommitBoundaries(t *testing.T) {
	for _, phase := range []string{"publishing", "published"} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%v", phase, committed), func(t *testing.T) {
				manager, err := NewManager(filepath.Join(t.TempDir(), "updates"))
				if err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(manager.Root(), VersionDir, "2.0.0")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				state := State{ActiveVersion: "1.0.0"}
				if committed {
					state.ActiveVersion, state.PreviousVersion = "2.0.0", "1.0.0"
				}
				if err := manager.writeState(t.Context(), state); err != nil {
					t.Fatal(err)
				}
				entry := journal{OperationID: "update-interrupted-test", PlanID: "plan-interrupted-test", Version: "2.0.0", PreviousVersion: "1.0.0", Phase: phase}
				if err := writeAtomicJSON(t.Context(), filepath.Join(manager.Root(), JournalName), entry); err != nil {
					t.Fatal(err)
				}
				if err := manager.Recover(t.Context()); err != nil {
					t.Fatal(err)
				}
				status, err := manager.Status(t.Context())
				if err != nil || status.RecoveryRequired || status.State.ActiveVersion != state.ActiveVersion {
					t.Fatalf("目录发布/状态提交恢复错误：%+v %v", status, err)
				}
				_, err = os.Stat(root)
				if (err == nil) != committed {
					t.Fatalf("版本目录的保留与提交状态不一致：%v", err)
				}
			})
		}
	}
}

func TestOfflineUpdateRejectsCodexAndSymlinkBundles(t *testing.T) {
	if _, err := NewManager(filepath.Join(t.TempDir(), ".codex", "install")); err == nil {
		t.Fatal("install root inside .codex was accepted")
	}
	bundle := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bundle, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := BuildManifest(context.Background(), bundle, "1.0.0", "test"); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("symlink bundle was accepted: %v", err)
	}
}

func makeBundle(t *testing.T, version, content string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "codex-chronicle"), []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildManifest(context.Background(), root, version, "test-build")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(context.Background(), root, manifest); err != nil {
		t.Fatal(err)
	}
	return root
}
