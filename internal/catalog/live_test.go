// 中文说明：验证 Codex 运行期间 JSONL 前缀读取的追加、半截记录、稳定和截断判定。
package catalog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanLiveJSONLMarksPendingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"complete"}`+"\n"+`{"type":"pending"`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ScanLiveJSONL(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State.Consistency != LivePendingLine || snapshot.State.Complete || !snapshot.State.Active || !snapshot.State.PendingLine {
		t.Fatalf("pending JSONL was not classified correctly: %+v", snapshot.State)
	}
	if snapshot.Summary.PendingLines != 1 || snapshot.State.CapturedBytes != int64(len(`{"type":"complete"}`)+1+len(`{"type":"pending"`)) {
		t.Fatalf("unexpected pending snapshot: %+v summary=%+v", snapshot.State, snapshot.Summary)
	}
}

func TestScanLiveJSONLMarksStableCompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := []byte(`{"type":"complete"}` + "\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ScanLiveJSONL(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State.Consistency != LiveStable || !snapshot.State.Complete || snapshot.State.Active || snapshot.State.CapturedBytes != int64(len(content)) {
		t.Fatalf("stable JSONL was not classified correctly: %+v", snapshot.State)
	}
}

func TestScanLiveJSONLCanReadCapturedPrefixWhileSourceAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := strings.Repeat(`{"type":"complete"}`+"\n", 20000)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// 这个测试验证读取结果的边界；追加发生得足够早时必须返回 active_append，
	// 若机器很快完成读取，也至少要证明捕获结果没有越过源文件初始大小。
	initialSize := int64(len(content))
	go func() {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, _ = file.WriteString(`{"type":"later"}` + "\n")
			_ = file.Close()
		}
	}()
	snapshot, err := ScanLiveJSONL(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State.CapturedBytes > initialSize {
		t.Fatalf("live scanner read beyond captured initial prefix: %+v", snapshot.State)
	}
	if snapshot.State.Consistency != LiveStable && snapshot.State.Consistency != LiveAppending {
		t.Fatalf("unexpected append classification: %+v", snapshot.State)
	}
}

func TestCaptureLiveFileDoesNotModifySource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asset.bin")
	content := []byte("source bytes")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := CaptureLiveFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Consistency != LiveStable || state.SHA256 == "" || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("capture changed or misclassified source: state=%+v before=%+v after=%+v", state, before, after)
	}
}

func TestClassifyLiveStateRejectsTruncationAndRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"original"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := regularFileState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := regularFileState(path)
	if err != nil {
		t.Fatal(err)
	}
	truncated := classifyLiveState(before, after, after.size, false, "digest")
	if truncated.Consistency != LiveTruncated || truncated.Complete || !truncated.Active {
		t.Fatalf("truncation was not rejected: %+v", truncated)
	}

	if err := os.WriteFile(path, []byte(`{"type":"aaaaaaaa"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteBefore, err := regularFileState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"bbbbbbbb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteAfter, err := regularFileState(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := classifyLiveState(rewriteBefore, rewriteAfter, rewriteBefore.size, false, "digest")
	if rewritten.Consistency != LiveRewriteConflict || rewritten.Complete || !rewritten.Active {
		t.Fatalf("rewrite was not rejected: %+v", rewritten)
	}
}
