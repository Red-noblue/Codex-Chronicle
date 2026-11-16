// 中文说明：验证活动副本对 Unicode/长路径、源哈希和取消清理的边界行为。
package copy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
)

func TestLiveCopyPreservesUnicodeAndLongRelativePath(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "副本", "live-copy")
	relative := filepath.Join("会话 空间", strings.Repeat("层级-", 12)+"终点", strings.Repeat("嵌套-", 16)+"记录.jsonl")
	path := filepath.Join(source, relative)
	content := []byte(`{"type":"unicode-path"}` + "\n")
	writeCopyTestFile(t, path, string(content))
	beforeHash := sha256.Sum256(content)
	options := &activity.Options{Interval: time.Millisecond, ProcessDetector: liveFixedProcessDetector{files: map[string][]domain.ProcessHandle{}}}
	plan, err := PrepareLive(context.Background(), LiveSnapshotRequest{SourceRoot: source, DestinationRoot: destination, ActivityOptions: options})
	if err != nil {
		t.Fatalf("Unicode/长路径活动计划失败：%v", err)
	}
	result, err := ExecuteLive(context.Background(), plan, ExecuteOptions{})
	if err != nil {
		t.Fatalf("Unicode/长路径活动副本失败：%v", err)
	}
	if !result.Published || !result.Complete || result.QueuedAssets != 0 {
		t.Fatalf("Unicode/长路径副本状态错误：%+v", result)
	}
	got, err := os.ReadFile(filepath.Join(destination, relative))
	if err != nil {
		t.Fatalf("Unicode/长路径副本文件缺失：%v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("Unicode/长路径副本内容改变：%q", got)
	}
	if hex.EncodeToString(beforeHash[:]) != sha256FileForTest(t, path) {
		t.Fatal("源文件哈希在活动副本后改变")
	}
}

func TestExecuteLiveCancellationCleansStagingWithoutPublishing(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "copies", "cancelled")
	relative := "rollout.jsonl"
	writeCopyTestFile(t, filepath.Join(source, relative), `{"type":"cancel"}`+"\n")
	snapshot, err := catalog.ScanLiveJSONL(context.Background(), filepath.Join(source, relative))
	if err != nil {
		t.Fatal(err)
	}
	plan := LiveSnapshotPlan{
		Version: 1, Mode: LiveBestEffort, SourceRoot: source, DestinationRoot: destination,
		Assets: []LiveAsset{liveAssetFromState(relative, domain.AssetKindJSONL, snapshot.State)},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExecuteLive(ctx, plan, ExecuteOptions{}); err == nil {
		t.Fatal("已取消的活动副本不应发布")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("取消后的活动副本留下了最终目录：%v", err)
	}
	parent := filepath.Dir(destination)
	if entries, readErr := os.ReadDir(parent); readErr == nil && len(entries) != 0 {
		t.Fatalf("取消后的 staging 父目录留下产物：%v", entries)
	}
}

func sha256FileForTest(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
