// 中文说明：验证批量预览可核对原始 ID、阻断时仍报告发现的资产，且保留预览不授予写权限或跨源访问。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/domain"
)

func TestRetainedBatchPreviewDoesNotAuthorizeExecution(t *testing.T) {
	root := t.TempDir()
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"original-thread-id\"}}\n"
	path := filepath.Join(root, "one.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(root)
	handler := server.Handler()
	ids := apiConversationIDs(t, handler)
	if len(ids) != 1 {
		t.Fatalf("期望一个测试会话: %v", ids)
	}
	request := `{"conversation_ids":["` + ids[0] + `"],"trash":true,"archive_password":"do-not-store-secret"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/batch/preview", strings.NewReader(request)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("真实能力未验证应阻断: %d %s", recorder.Code, recorder.Body.String())
	}
	var preview batchOperationResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Summary.Assets != 1 || preview.Summary.Bytes != int64(len(content)) || len(preview.Items[0].DiscoveredAssets) != 1 || preview.Items[0].SourceID != "original-thread-id" {
		t.Fatalf("阻断不应把已发现资产显示为零: %+v", preview)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/operations/batch/preview/latest", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || strings.Contains(recorder.Body.String(), "do-not-store-secret") {
		t.Fatalf("预览保留范围或敏感字段错误: %d %s", recorder.Code, recorder.Body.String())
	}
	var review batchOperationReview
	if err := json.Unmarshal(recorder.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if !review.ReadOnly || review.Approval != "required" || review.Preview.SourceRoot != root || !review.Trash {
		t.Fatalf("预览不能成为写入授权: %+v", review)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/batch/execute", strings.NewReader(request)))
	if recorder.Code != http.StatusForbidden || server.lifecycleWriteEnabled() {
		t.Fatalf("提交预览不能解锁执行: %d", recorder.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != content {
		t.Fatalf("预览改变了源文件: %v", err)
	}
	server.batchReview.Preview.SourceRoot = t.TempDir()
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/operations/batch/preview/latest", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatal("不应返回其他源目录的预览")
	}
	server.batchReview.Preview.SourceRoot = root
	server.batchReview.ExpiresAt = time.Now().Add(-time.Minute)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/operations/batch/preview/latest", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatal("过期预览必须重新提交")
	}
}

func TestSharedControlPlaneBlockDoesNotMeanRolloutWriting(t *testing.T) {
	assets := []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "rollout", Kind: domain.AssetKindJSONL, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, Status: domain.StatusActiveWriting}},
	}
	owners := map[string]int{"rollout": 1, "state.sqlite": 21}
	if !sharedControlPlaneBlocked(assets, owners) {
		t.Fatal("共享控制面占用应与正文活动区分")
	}
	assets[0].Status = domain.StatusActiveWriting
	if sharedControlPlaneBlocked(assets, owners) {
		t.Fatal("正文也在写入时不能声称只有共享控制面占用")
	}
	assets[0].Status = domain.StatusStable
	assets[1].Status = domain.StatusUnsupported
	if sharedControlPlaneBlocked(assets, owners) {
		t.Fatal("共享数据库需要复核不能误报为仍被占用")
	}
}
