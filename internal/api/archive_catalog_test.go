// 中文说明：验证本地归档目录清单只读读取 manifest，并能识别远端恢复 staging 归档。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

func TestLocalArchiveCatalogListsManifestWithoutPayloadVerification(t *testing.T) {
	sourceRoot := t.TempDir()
	payload := []byte("catalog fixture\n")
	payloadPath := filepath.Join(sourceRoot, "rollout.jsonl")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	plan := lifecycle.OperationPlan{
		OperationID: "catalog-operation", Root: sourceRoot, ConversationID: "catalog-conversation", Status: lifecycle.PlanPrepared,
		Assets: []lifecycle.AssetFingerprint{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}},
	}
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	created, err := archive.CreateLocal(context.Background(), plan, archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	stagingArchive := filepath.Join(archiveRoot, ".remote-restore-staging", "restore-test", "remote-copy")
	if err := copyAPIArchiveTree(created.Directory, stagingArchive); err != nil {
		t.Fatal(err)
	}

	server := NewServerWithOptions("", nil, &LifecycleOptions{ArchiveRoot: archiveRoot})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/archives/local", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("本地归档清单请求失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ReadOnly bool                      `json:"read_only"`
		Items    []localArchiveCatalogItem `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ReadOnly || len(response.Items) != 2 {
		t.Fatalf("本地归档清单结果不正确: %+v", response)
	}
	var regular, staging bool
	for _, item := range response.Items {
		if item.OperationID != plan.OperationID || item.DeclaredPayloadBytes != int64(len(payload)) {
			t.Fatalf("manifest 派生字段不正确: %+v", item)
		}
		if item.Staging {
			staging = true
		} else {
			regular = true
		}
	}
	if !regular || !staging {
		t.Fatalf("未同时列出普通和 staging 归档: %+v", response.Items)
	}
}

func TestLocalArchiveCatalogFailsClosedForCodexArchiveRoot(t *testing.T) {
	server := NewServerWithOptions("", nil, &LifecycleOptions{ArchiveRoot: filepath.Join(t.TempDir(), ".codex", "archives")})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/archives/local", nil))
	if recorder.Code != http.StatusOK || len(recorder.Body.Bytes()) == 0 {
		t.Fatalf("不安全 ArchiveRoot 未返回诊断: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Available bool     `json:"available"`
		Errors    []string `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Available || len(response.Errors) == 0 {
		t.Fatalf("不安全 ArchiveRoot 未 fail-closed: %+v", response)
	}
}
