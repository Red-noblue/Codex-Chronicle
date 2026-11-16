// 中文说明：按真实 GitHub 语义覆盖 1 MiB 大文件、无分页目录、1000 项目录树和分片中断续传。
package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/lifecycle"
)

func TestGitHubLargeRawAndIdempotentUpload(t *testing.T) {
	server := newGitHubContentsTestServer()
	defer server.Close()
	p, err := NewGitHubProvider(GitHubConfig{BaseURL: server.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token"})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Repeat("大型分片", 200000))
	if err := p.putContent(t.Context(), "large", body, "test"); err != nil {
		t.Fatal(err)
	}
	result, _, err := p.getContent(t.Context(), "large")
	if err != nil || string(result) != string(body) {
		t.Fatal("大于 1 MiB 的 raw 读取失败", err)
	}
	puts := server.putCount()
	if err := p.putContent(t.Context(), "large", body, "test"); err != nil {
		t.Fatal(err)
	}
	if server.putCount() != puts {
		t.Fatal("相同大分片被重复上传")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	server.mu.Lock()
	for i := 0; i < 120; i++ {
		server.files[fmt.Sprintf("folder/file-%03d", i)] = []byte("x")
	}
	server.mu.Unlock()
	entries, err := p.listDirectory(ctx, "folder")
	if err != nil || len(entries) != 120 {
		t.Fatal("Contents 不分页时目录读取不完整", len(entries), err)
	}
}

func TestGitHubTreeFallbackAndTruncation(t *testing.T) {
	truncated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			entries := make([]githubDirectoryEntry, 1000)
			for i := range entries {
				entries[i] = githubDirectoryEntry{Type: "file", Path: fmt.Sprintf("root/f%d", i)}
			}
			json.NewEncoder(w).Encode(entries)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/HEAD") {
			json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]string{{"type": "tree", "path": "root", "sha": "root-tree"}}})
			return
		}
		entries := []map[string]string{}
		for i := 0; i < 1005; i++ {
			entries = append(entries, map[string]string{"type": "blob", "path": fmt.Sprintf("f%d", i), "sha": "blob"})
		}
		json.NewEncoder(w).Encode(map[string]any{"tree": entries, "truncated": truncated})
	}))
	defer server.Close()
	p, _ := NewGitHubProvider(GitHubConfig{BaseURL: server.URL, Owner: "owner", Repository: "chronicle", Token: "runtime-token"})
	entries, err := p.listDirectory(t.Context(), "root")
	if err != nil || len(entries) != 1005 {
		t.Fatal(len(entries), err)
	}
	truncated = true
	if _, err := p.listDirectory(t.Context(), "root"); err == nil {
		t.Fatal("截断目录树不应被当作完整结果")
	}
}

func TestGitHubInterruptedChunksResumeWithoutRepeatingWrites(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(t.Context(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	local, err := CreateLocal(t.Context(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newGitHubContentsTestServer()
	defer server.Close()
	p, _ := NewGitHubProvider(GitHubConfig{BaseURL: server.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", ChunkBytes: 32})
	ctx, cancel := context.WithCancel(t.Context())
	progress := WithTransferProgress(ctx, func(size int64) { cancel() })
	if _, err := p.UploadDirectory(progress, local.Directory, "resume"); err == nil {
		t.Fatal("取消后仍完成上传")
	}
	if server.has("resume/remote-manifest.json") {
		t.Fatal("未完成上传不应发布清单")
	}
	partial := server.putCount()
	if partial == 0 {
		t.Fatal("未产生部分上传")
	}
	result, err := p.UploadDirectory(t.Context(), local.Directory, "resume")
	if err != nil {
		t.Fatal(err)
	}
	if server.putCount() != result.ChunkCount+1 {
		t.Fatalf("续传重复 PUT: partial=%d total=%d chunks=%d", partial, server.putCount(), result.ChunkCount)
	}
}
