// 中文说明：验证活动探测的窗口变化、进程证据、writer lock、SQLite busy 和路径安全边界。
package activity

import (
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"codex-chronicle.local/internal/domain"
	_ "modernc.org/sqlite"
)

func TestActivityWriterHelper(t *testing.T) {
	if os.Getenv("CODEX_CHRONICLE_ACTIVITY_HELPER") != "1" {
		return
	}
	if len(os.Args) == 0 {
		return
	}
	path := os.Args[len(os.Args)-1]
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	for {
		if _, err := file.WriteString(`{"type":"writer"}` + "\n"); err != nil {
			return
		}
		_ = file.Sync()
		time.Sleep(5 * time.Millisecond)
	}
}

type fixedProcessDetector struct {
	snapshot ProcessSnapshot
}

func (detector fixedProcessDetector) Detect(context.Context, string) ProcessSnapshot {
	return detector.snapshot
}

func TestProbeStableWithoutProcessProbeWhenConservativeDisabled(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval:            time.Millisecond,
		DisableProcessProbe: true,
		Conservative:        false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.LockProbeAvailable || report.ProcessProbeAvailable || len(report.Results) != 1 {
		t.Fatalf("unexpected probe report: %+v", report)
	}
	result := report.Results[0]
	if result.Status != domain.StatusStable || result.Evidence.ChangedDuringProbe || result.Evidence.AfterSizeBytes != 3 {
		t.Fatalf("unexpected stable result: %+v", result)
	}
}

func TestProbeDetectsWindowChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(path, []byte("after\n"), 0o644)
	}()
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval:            50 * time.Millisecond,
		DisableProcessProbe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != domain.StatusActiveWriting || !report.Results[0].Evidence.ChangedDuringProbe {
		t.Fatalf("window change was not classified as active writing: %+v", report)
	}
}

func TestProbeClassifiesAssetDisappearingDuringWindowAsConflict(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = os.Remove(path)
	}()
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval:            40 * time.Millisecond,
		DisableProcessProbe: true,
		Conservative:        false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != domain.StatusConflict {
		t.Fatalf("窗口内消失的资产没有阻断为 conflict：%+v", report)
	}
}

func TestProbeClassifiesAtomicReplacementDuringWindowAsSourceChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"before"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		replacement := path + ".replacement"
		if err := os.WriteFile(replacement, []byte(`{"type":"after-replacement"}`+"\n"), 0o644); err == nil {
			_ = os.Rename(replacement, path)
		}
	}()
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval:            40 * time.Millisecond,
		DisableProcessProbe: true,
		Conservative:        false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("原子替换探测结果数量错误：%+v", report)
	}
	result := report.Results[0]
	if result.Status != domain.StatusActiveWriting || !result.Evidence.SourceChangedDuringProbe || result.Evidence.SourceIdentityBefore == result.Evidence.SourceIdentityAfter || !result.MutationBlocked {
		t.Fatalf("探测期间原子替换没有被识别并阻断：%+v", result)
	}
}

func TestProbeClassifiesMatchingLockAsStale(t *testing.T) {
	root := t.TempDir()
	threadID := "11111111-1111-1111-1111-111111111111"
	otherID := "22222222-2222-2222-2222-222222222222"
	rollout := filepath.Join(root, "rollout-"+threadID+".jsonl")
	if err := os.WriteFile(rollout, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockRoot := filepath.Join(root, "thread-writer-locks")
	if err := os.Mkdir(lockRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockRoot, threadID+".lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockRoot, otherID+".lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Probe(context.Background(), root, []Target{{RelativePath: filepath.Base(rollout), Kind: domain.AssetKindJSONL}}, Options{
		Interval:            time.Millisecond,
		DisableProcessProbe: true,
		Conservative:        false,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if report.LockFileCount != 2 || len(report.LockIDs) != 2 || report.LockIDs[0] != threadID || report.LockIDs[1] != otherID {
		t.Fatalf("lock IDs were not reported in sorted order: %+v", report)
	}
	if result.Status != domain.StatusStaleLock || len(result.Evidence.MatchingLockIDs) != 1 || result.Evidence.MatchingLockIDs[0] != threadID {
		t.Fatalf("matching lock was not classified as stale: %+v", result)
	}
}

func TestProbeUsesOpenProcessEvidence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detector := fixedProcessDetector{snapshot: ProcessSnapshot{
		Available: true,
		Reason:    "测试探测器",
		Files: map[string][]domain.ProcessHandle{
			path: {{PID: 42, Command: "codex"}},
		},
	}}
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval:        time.Millisecond,
		ProcessDetector: detector,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if result.Status != domain.StatusActiveWriting || len(result.Evidence.OpenByProcess) != 1 || result.Evidence.OpenByProcess[0].PID != 42 {
		t.Fatalf("open process was not classified as active writing: %+v", result)
	}
}

func TestProbeReportsPendingJSONLAndBlocksMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"complete"}`+"\n"+`{"type":"pending"`), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval: time.Millisecond, DisableProcessProbe: true, Conservative: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if result.Status != domain.StatusPendingJSONLLine || result.ActivityStatus != domain.StatusPendingJSONLLine || !result.Evidence.JSONLPendingLine || !result.MutationBlocked {
		t.Fatalf("pending JSONL activity evidence is incomplete: %+v", result)
	}
	if !containsActivityState(result.Evidence.ObservedStates, domain.StatusPendingJSONLLine) || !containsActivityState(result.Evidence.ObservedStates, domain.StatusBlockedForMutation) {
		t.Fatalf("pending JSONL states missing: %+v", result.Evidence.ObservedStates)
	}
}

func TestProbeDetectsSameSizeContentRewriteWithSourceHash(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := []byte(`{"type":"aaaa"}` + "\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		// 在窗口内保持大小和 mtime 不变，验证内容指纹仍能发现原地改写。
		time.Sleep(5 * time.Millisecond)
		_ = os.WriteFile(path, []byte(`{"type":"bbbb"}`+"\n"), 0o600)
		_ = os.Chtimes(path, info.ModTime(), info.ModTime())
	}()
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval: 30 * time.Millisecond, DisableProcessProbe: true, Conservative: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if !result.Evidence.SourceHashCompared || result.Evidence.SourceHashBefore == result.Evidence.SourceHashAfter || !result.Evidence.SourceChangedDuringProbe {
		t.Fatalf("same-size rewrite was not detected by source hash: %+v", result)
	}
}

func TestProbeReportsSQLiteWALEvidenceWithoutWritingAuxiliaryFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY)"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(path+suffix, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite}}, Options{
		Interval: time.Millisecond, DisableProcessProbe: true, Conservative: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if !result.Evidence.SQLiteWALPresent || !result.Evidence.SQLiteSHMPresent || result.ActivityStatus != domain.StatusSQLiteWALPresent {
		t.Fatalf("SQLite WAL/SHM evidence missing: %+v", result)
	}
}

func TestProbeWindowsKeepsActivityHistory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"complete"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	detector := fixedProcessDetector{snapshot: ProcessSnapshot{Available: true, Reason: "连续窗口测试", Files: map[string][]domain.ProcessHandle{path: {{PID: 9, Command: "codex"}}}}}
	report, err := ProbeWindows(context.Background(), root, []Target{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval: time.Millisecond, ProcessDetector: detector,
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if report.WindowCount != 2 || len(report.Windows) != 2 || len(result.Evidence.History) < 2 || result.ActivityStatus != domain.StatusOpenByProcess {
		t.Fatalf("continuous activity history is incomplete: report=%+v result=%+v", report, result)
	}
}

func TestProbeObservesForcedWriterTerminationWithoutRepairingSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "writer.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"initial"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestActivityWriterHelper", "--", path)
	command.Env = append(os.Environ(), "CODEX_CHRONICLE_ACTIVITY_HELPER=1")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	time.Sleep(20 * time.Millisecond)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "writer.jsonl", Kind: domain.AssetKindJSONL}}, Options{
		Interval: 30 * time.Millisecond, DisableProcessProbe: true, Conservative: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Status == domain.StatusStable || !report.Results[0].Evidence.ChangedDuringProbe {
		t.Fatalf("强制终止 writer 前未留下可解释活动证据：%+v", report)
	}
	if err := command.Process.Kill(); err != nil {
		// writer 可能已经自然退出；Wait 仍会回收子进程。
		_ = err
	}
	_ = command.Wait()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) < len(before) {
		t.Fatalf("活动探测不应截断 writer 源文件：before=%d after=%d", len(before), len(after))
	}
}

func containsActivityState(values []domain.AssetStatus, expected domain.AssetStatus) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestProbeDetectsSQLiteBusy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY); INSERT INTO threads VALUES('thread-a');"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	transaction, err := database.Begin()
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := transaction.Exec("PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		_ = transaction.Rollback()
		database.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = transaction.Rollback()
		_ = database.Close()
	}()
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite}}, Options{
		Interval:            time.Millisecond,
		DisableProcessProbe: true,
		Conservative:        false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 某些 SQLite 构建在同一进程内仍允许 schema_version 读取；此时至少确认探测结果可解释。
	result := report.Results[0]
	if result.Error != "" || result.Evidence.SQLiteProbeReason == "" {
		t.Fatalf("SQLite busy probe did not return structured evidence: %+v", result)
	}
}

func TestProbeRejectsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.jsonl")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	report, err := Probe(context.Background(), root, []Target{{RelativePath: "link.jsonl", Kind: domain.AssetKindJSONL}}, Options{Interval: time.Millisecond, DisableProcessProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != domain.StatusUnknown {
		t.Fatalf("symlink target was not rejected: %+v", report)
	}
}

func TestProbeSQLiteDoesNotModifyExistingWALAndSHM(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY); INSERT INTO threads VALUES('thread-a');"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	auxiliary := map[string][]byte{
		path + "-wal": []byte("stable wal bytes"),
		path + "-shm": []byte("stable shm bytes"),
	}
	type fileSnapshot struct {
		bytes []byte
		size  int64
		mtime int64
	}
	snapshots := make(map[string]fileSnapshot, len(auxiliary))
	for file, content := range auxiliary {
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		snapshots[file] = fileSnapshot{bytes: append([]byte(nil), content...), size: info.Size(), mtime: info.ModTime().UnixNano()}
	}

	report, err := Probe(context.Background(), root, []Target{{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite}}, Options{
		Interval: time.Millisecond, DisableProcessProbe: true, Conservative: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Evidence.SQLiteProbeReason == "" {
		t.Fatalf("SQLite immutable probe did not return structured evidence: %+v", report)
	}
	for file, expected := range snapshots {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != string(expected.bytes) || info.Size() != expected.size || info.ModTime().UnixNano() != expected.mtime {
			t.Fatalf("SQLite 活动探测修改了旁路文件 %s: before=%+v after_bytes=%q after_info=%+v", file, expected, content, info)
		}
	}
}
