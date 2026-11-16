// 中文说明：验证稳定副本制作的只读边界、活动门禁、原子 staging 和完整哈希校验。
package copy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/maintenance"
)

func TestPrepareAndExecutePublishesVerifiedCopy(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "copies", "codex-copy")
	if err := os.MkdirAll(filepath.Join(source, "sessions", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "sessions", "nested", "rollout.jsonl"), `{"type":"session_meta"}`+"\n")
	writeCopyTestFile(t, filepath.Join(source, "state.sqlite"), "sqlite bytes")
	if err := os.Chtimes(filepath.Join(source, "sessions"), time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: destination,
		ActivityOptions: &activity.Options{
			Interval:            time.Millisecond,
			DisableProcessProbe: false,
			ProcessDetector:     stableProcessDetector{},
		},
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if plan.FileCount != 2 || plan.TotalBytes == 0 || plan.SourceManifestSHA256 == "" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("Prepare must not create destination: err=%v", err)
	}

	registered := make([]maintenance.ArtifactManifest, 0, 2)
	result, err := Execute(context.Background(), plan, ExecuteOptions{RegisterArtifact: func(_ context.Context, manifest maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error) {
		if manifest.ArtifactID == "" {
			manifest.ArtifactID = "cleanup-" + strings.Repeat("a", 32)
		}
		registered = append(registered, manifest)
		return manifest, nil
	}})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Published || result.ManifestPath == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(registered) != 2 || registered[0].TargetPath == registered[1].TargetPath || registered[1].TargetPath != destination || registered[1].State != "published" || registered[1].Rebuildable {
		t.Fatalf("staging 登记未在发布后更新：%+v", registered)
	}
	report, err := VerifyDestination(context.Background(), destination)
	if err != nil {
		t.Fatalf("VerifyDestination failed: %v", err)
	}
	if !report.Verified || report.FileCount != plan.FileCount || report.SourceManifestSHA256 != plan.SourceManifestSHA256 {
		t.Fatalf("unexpected destination report: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(destination, ManifestName)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "sessions", "nested", "rollout.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, ".codex-real-copy-only.json")); !os.IsNotExist(err) {
		t.Fatalf("marker must be inside destination only: %v", err)
	}
}

func TestPrepareAndExecuteHonorsExplicitExclusions(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "copies", "codex-copy")
	if err := os.MkdirAll(filepath.Join(source, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "sessions", "rollout.jsonl"), "conversation\n")
	writeCopyTestFile(t, filepath.Join(source, "auth.json"), "secret")
	writeCopyTestFile(t, filepath.Join(source, "backups", "auth.json"), "secret backup")

	plan, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: destination,
		ExcludePaths:    []string{"auth.json", "backups"},
		ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}},
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if plan.FileCount != 1 || len(plan.ExcludedPaths) != 2 {
		t.Fatalf("explicit exclusions were not recorded: files=%d excluded=%v", plan.FileCount, plan.ExcludedPaths)
	}
	if _, err := Execute(context.Background(), plan, ExecuteOptions{}); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "sessions", "rollout.jsonl")); err != nil {
		t.Fatalf("included asset missing: %v", err)
	}
	for _, relative := range []string{"auth.json", filepath.Join("backups", "auth.json")} {
		if _, err := os.Stat(filepath.Join(destination, relative)); !os.IsNotExist(err) {
			t.Fatalf("excluded asset was copied: %s (%v)", relative, err)
		}
	}
	if _, err := os.Stat(filepath.Join(source, "auth.json")); err != nil {
		t.Fatalf("source credential file changed or disappeared: %v", err)
	}
	if report, err := VerifyDestination(context.Background(), destination); err != nil || !report.Verified {
		t.Fatalf("excluded copy did not verify: report=%+v err=%v", report, err)
	}
}

func TestPrepareRejectsUnsafeOrOverlappingExclusions(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(filepath.Join(source, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "backups", "auth.json"), "secret")
	options := &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}
	for _, paths := range [][]string{{"../auth.json"}, {"backups", "backups/auth.json"}} {
		if _, err := Prepare(context.Background(), CopyRequest{SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"), ExcludePaths: paths, ActivityOptions: options}); err == nil {
			t.Fatalf("expected exclusion rejection for %v", paths)
		}
	}
}

func TestPrepareRejectsSourceActivityAndLock(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(filepath.Join(source, "thread-writer-locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "rollout.jsonl")
	writeCopyTestFile(t, path, "payload")
	lockPath := filepath.Join(source, "thread-writer-locks", "writer-11111111-1111-1111-1111-111111111111.lock")
	writeCopyTestFile(t, lockPath, "lock")
	plan, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"),
		ActivityOptions: &activity.Options{
			Interval:            time.Millisecond,
			DisableProcessProbe: false,
			ProcessDetector:     stableProcessDetector{},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "writer lock") || !strings.Contains(err.Error(), "11111111-1111-1111-1111-111111111111") {
		t.Fatalf("expected lock rejection, got %v", err)
	}
	if len(plan.Activity.LockIDs) != 1 || plan.Activity.LockIDs[0] != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("failed prepare should retain lock summary: %+v", plan.Activity)
	}
}

func TestPrepareRejectsActiveProcessInStrictMode(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	path := filepath.Join(source, "rollout.jsonl")
	writeCopyTestFile(t, path, "payload")
	_, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"),
		ActivityOptions: &activity.Options{
			Interval:        time.Millisecond,
			ProcessDetector: activeProcessDetector{path: path},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "活动状态") {
		t.Fatalf("strict mode did not reject an active writer process: %v", err)
	}
}

func TestPrepareRejectsProcessProbeUnavailable(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), "payload")
	_, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"),
		ActivityOptions: &activity.Options{
			Interval:        time.Millisecond,
			ProcessDetector: unavailableProcessDetector{},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "无法确认源目录") {
		t.Fatalf("expected unavailable process rejection, got %v", err)
	}
}

func TestPrepareCanExplicitlyAcceptUnavailableProcessProbe(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), "payload")
	plan, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"), AllowUnavailableProcessProbe: true,
		ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: unavailableProcessDetector{}},
	})
	if err != nil {
		t.Fatalf("explicit unavailable process override failed: %v", err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "无法确认打开文件进程") {
		t.Fatalf("missing explicit risk warning: %+v", plan.Warnings)
	}
}

func TestPrepareRejectsUnsafeRootsAndDestination(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "file"), "payload")
	options := &activity.Options{Interval: time.Millisecond, DisableProcessProbe: false, ProcessDetector: stableProcessDetector{}}
	tests := []struct {
		name string
		dest string
	}{
		{name: "inside source", dest: filepath.Join(source, "copy")},
		{name: "inside codex component", dest: filepath.Join(base, "another", ".codex", "copy")},
		{name: "source itself", dest: source},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Prepare(context.Background(), CopyRequest{SourceRoot: source, DestinationRoot: test.dest, ActivityOptions: options}); err == nil {
				t.Fatal("expected unsafe root rejection")
			}
		})
	}

	existing := filepath.Join(base, "existing")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), CopyRequest{SourceRoot: source, DestinationRoot: existing, ActivityOptions: options}); err == nil {
		t.Fatal("expected existing destination rejection")
	}
}

func TestExecuteCleansStagingAfterFailureAndKeepsExistingDestination(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "copy")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), "payload")
	plan, err := Prepare(context.Background(), CopyRequest{SourceRoot: source, DestinationRoot: destination, ActivityOptions: &activity.Options{Interval: time.Millisecond, DisableProcessProbe: false, ProcessDetector: stableProcessDetector{}}})
	if err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), "changed payload")
	if _, err := Execute(context.Background(), plan, ExecuteOptions{}); err == nil {
		t.Fatal("expected source change rejection")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed Execute must not publish destination: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "codex-chronicle-staging") {
			t.Fatalf("failed Execute left staging directory: %s", entry.Name())
		}
	}

	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), plan, ExecuteOptions{}); err == nil {
		t.Fatal("expected existing destination rejection")
	}
}

func TestPrepareRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := Prepare(context.Background(), CopyRequest{SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"), ActivityOptions: &activity.Options{Interval: time.Millisecond, DisableProcessProbe: false, ProcessDetector: stableProcessDetector{}}})
	if err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestPrepareSkipsKnownRuntimeSymlinks(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "copy")
	if err := os.MkdirAll(filepath.Join(source, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "sessions", "rollout.jsonl"), "payload")
	target := filepath.Join(base, "outside")
	writeCopyTestFile(t, target, "runtime target")
	symlink := filepath.Join(source, "tmp", "runtime-tool")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	plan, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: destination,
		ActivityOptions: &activity.Options{Interval: time.Millisecond, DisableProcessProbe: false, ProcessDetector: stableProcessDetector{}},
	})
	if err != nil {
		t.Fatalf("known runtime symlink should be excluded: %v", err)
	}
	if len(plan.ExcludedPaths) != 1 || plan.ExcludedPaths[0] != "tmp/runtime-tool" {
		t.Fatalf("unexpected excluded paths: %+v", plan.ExcludedPaths)
	}
	if len(plan.Warnings) == 0 || !strings.Contains(plan.Warnings[0], "已排除") {
		t.Fatalf("missing excluded-path warning: %+v", plan.Warnings)
	}
	if _, err := Execute(context.Background(), plan, ExecuteOptions{}); err != nil {
		t.Fatalf("execute with known runtime symlink failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "tmp", "runtime-tool")); !os.IsNotExist(err) {
		t.Fatalf("runtime symlink must not be copied: %v", err)
	}
	verified, err := VerifyDestination(context.Background(), destination)
	if err != nil || !verified.Verified {
		t.Fatalf("excluded-path copy should verify: report=%+v err=%v", verified, err)
	}
}

func TestPrepareRejectsSQLiteAuxiliaryFilesInStrictMode(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "state.sqlite"), "database")
	writeCopyTestFile(t, filepath.Join(source, "state.sqlite-wal"), "wal")
	writeCopyTestFile(t, filepath.Join(source, "state.sqlite-shm"), "shm")
	destination := filepath.Join(base, "copy")
	plan, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: destination,
		ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}},
	})
	if err == nil || !strings.Contains(err.Error(), "严格稳定模式") || len(plan.AuxiliarySQLiteFiles) != 2 {
		t.Fatalf("strict mode should reject WAL/SHM instead of claiming a complete copy: plan=%+v err=%v", plan, err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("strict rejection should not create destination: %v", statErr)
	}
}

func TestPrepareRejectsSQLiteAuxiliaryFilesEvenWhenExcluded(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "state.sqlite"), "database")
	writeCopyTestFile(t, filepath.Join(source, "state.sqlite-wal"), "wal")
	writeCopyTestFile(t, filepath.Join(source, "state.sqlite-shm"), "shm")
	_, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"),
		ExcludePaths:    []string{"state.sqlite-wal", "state.sqlite-shm"},
		ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}},
	})
	if err == nil || !strings.Contains(err.Error(), "严格稳定模式") {
		t.Fatalf("strict mode must reject WAL/SHM even when sidecars are excluded: %v", err)
	}
}

func TestPrepareRejectsOrphanSQLiteAuxiliaryFiles(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "state.sqlite-wal"), "wal")
	_, err := Prepare(context.Background(), CopyRequest{
		SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"),
		ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}},
	})
	if err == nil || !strings.Contains(err.Error(), "缺少对应主库") {
		t.Fatalf("expected orphan WAL/SHM rejection, got %v", err)
	}
}

func TestVerifyDestinationRejectsChangedFile(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "copy")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "rollout.jsonl")
	writeCopyTestFile(t, path, "payload")
	plan, err := Prepare(context.Background(), CopyRequest{SourceRoot: source, DestinationRoot: destination, ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), plan, ExecuteOptions{}); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(destination, "rollout.jsonl"), "tampered")
	if _, err := VerifyDestination(context.Background(), destination); err == nil || (!strings.Contains(err.Error(), "哈希") && !strings.Contains(err.Error(), "元数据")) {
		t.Fatalf("expected tampered destination rejection, got %v", err)
	}
}

type stableProcessDetector struct{}

func (stableProcessDetector) Detect(context.Context, string) activity.ProcessSnapshot {
	return activity.ProcessSnapshot{Available: true, Reason: "test stable", Files: map[string][]domain.ProcessHandle{}}
}

type unavailableProcessDetector struct{}

func (unavailableProcessDetector) Detect(context.Context, string) activity.ProcessSnapshot {
	return activity.ProcessSnapshot{Reason: "test unavailable", Files: map[string][]domain.ProcessHandle{}}
}

type activeProcessDetector struct {
	path string
}

func (detector activeProcessDetector) Detect(context.Context, string) activity.ProcessSnapshot {
	return activity.ProcessSnapshot{Available: true, Reason: "test active process", Files: map[string][]domain.ProcessHandle{
		detector.path: {{PID: 42, Command: "codex"}},
	}}
}

func writeCopyTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
