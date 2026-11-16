// 中文说明：验证真实结构稳定副本的 HTTP 只读边界，以及 fixture-only 归档、回收站删除和本地归档恢复。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/copy"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/operation"
)

const apiRealCopyThreadID = "00000000-0000-4000-8000-000000000001"

func TestRealCopyReadOnlyInventoryAndAssetGraphThroughHTTPAPI(t *testing.T) {
	root := createAPIRealCopyRoot(t)
	before := snapshotRealCopyTree(t, root)
	handler := NewServer(root).Handler()

	capabilities := apiJSON(t, handler, http.MethodGet, "/api/v1/capabilities", "")
	if capabilities.Code != http.StatusOK {
		t.Fatalf("真实副本只读能力读取失败: status=%d body=%s", capabilities.Code, capabilities.Body.String())
	}
	var capabilityResponse struct {
		ReadOnly     bool `json:"read_only"`
		WriteEnabled bool `json:"write_enabled"`
		Adapter      struct {
			ID           string          `json:"id"`
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"adapter"`
	}
	if err := json.Unmarshal(capabilities.Body.Bytes(), &capabilityResponse); err != nil {
		t.Fatal(err)
	}
	if !capabilityResponse.ReadOnly || capabilityResponse.WriteEnabled || capabilityResponse.Adapter.Capabilities["delete"] || capabilityResponse.Adapter.Capabilities["archive_export"] {
		t.Fatalf("真实副本默认能力越过只读边界: %+v body=%s", capabilityResponse, capabilities.Body.String())
	}

	inventory := apiJSON(t, handler, http.MethodGet, "/api/v1/inventory?offset=0&limit=20", "")
	if inventory.Code != http.StatusOK || !strings.Contains(inventory.Body.String(), `"read_only":true`) {
		t.Fatalf("真实副本 inventory 读取失败: status=%d body=%s", inventory.Code, inventory.Body.String())
	}
	conversations := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations?offset=0&limit=20", "")
	if conversations.Code != http.StatusOK {
		t.Fatalf("真实副本会话列表读取失败: status=%d body=%s", conversations.Code, conversations.Body.String())
	}
	var conversationResponse struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(conversations.Body.Bytes(), &conversationResponse); err != nil {
		t.Fatal(err)
	}
	if len(conversationResponse.Items) == 0 {
		t.Fatalf("真实副本没有逻辑会话: %s", conversations.Body.String())
	}
	conversationID := conversationResponse.Items[0].ID

	detail := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations/"+conversationID+"?graph=full&files=summary", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"asset_graph"`) {
		t.Fatalf("真实副本详情或资产图读取失败: status=%d body=%s", detail.Code, detail.Body.String())
	}
	graph := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations/"+conversationID+"/asset-graph?node_offset=0&edge_offset=0&limit=200", "")
	if graph.Code != http.StatusOK || !strings.Contains(graph.Body.String(), `"nodes"`) {
		t.Fatalf("真实副本资产图分页读取失败: status=%d body=%s", graph.Code, graph.Body.String())
	}

	readiness := apiJSON(t, handler, http.MethodGet, "/api/v1/readiness", "")
	if readiness.Code != http.StatusOK || !strings.Contains(readiness.Body.String(), `"real_codex_write_ready":false`) || !strings.Contains(readiness.Body.String(), `"id":"real-codex-mutation"`) {
		t.Fatalf("真实副本 readiness 没有保持真实写入阻断: status=%d body=%s", readiness.Code, readiness.Body.String())
	}
	preview := apiJSON(t, handler, http.MethodPost, "/api/v1/operations/preview", `{"conversation_id":"`+conversationID+`","archive":true,"archive_local":true,"trash":true}`)
	if preview.Code != http.StatusConflict || !strings.Contains(preview.Body.String(), `"blocked":true`) || !strings.Contains(preview.Body.String(), `"read_only":true`) {
		t.Fatalf("真实副本危险操作没有 fail-closed: status=%d body=%s", preview.Code, preview.Body.String())
	}

	if after := snapshotRealCopyTree(t, root); len(after) != len(before) {
		t.Fatalf("只读 API 改变了真实副本文件数量: before=%d after=%d", len(before), len(after))
	} else {
		for path, digest := range before {
			if after[path] != digest {
				t.Fatalf("只读 API 改变了真实副本文件: %s", path)
			}
		}
	}
}

func TestRealCopyLifecycleThroughHTTPAPI(t *testing.T) {
	root := createAPIRealCopyRoot(t)
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	journalRoot := filepath.Join(t.TempDir(), "journals")
	trashRoot := filepath.Join(t.TempDir(), "trash")
	mutation, err := compat.NewRealShapeCopyMutation(filepath.Join(journalRoot, "mutation-snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerWithOptions(root, nil, &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.RealShapeCopyDescriptor(),
		ControlPlaneMutation: mutation, ArchiveRoot: archiveRoot, JournalRoot: journalRoot, TrashRoot: trashRoot,
	})
	handler := server.Handler()
	conversationID := compat.StableConversationID(apiRealCopyThreadID)

	preview := apiJSON(t, handler, http.MethodPost, "/api/v1/operations/preview", `{"conversation_id":"`+conversationID+`","archive":true,"archive_local":true,"trash":true}`)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"blocked":false`) || !strings.Contains(preview.Body.String(), `"adapter_id":"codex-real-copy-v1"`) {
		t.Fatalf("真实副本 API 预览失败: status=%d body=%s", preview.Code, preview.Body.String())
	}

	execute := executeReviewedAPI(t, handler, "/api/v1/operations/execute", `{"conversation_id":"`+conversationID+`","archive":true,"archive_local":true,"trash":true}`)
	if execute.Code != http.StatusOK {
		t.Fatalf("真实副本 API 执行失败: status=%d body=%s", execute.Code, execute.Body.String())
	}
	var executeResponse struct {
		Result operation.SagaResult `json:"result"`
	}
	if err := json.Unmarshal(execute.Body.Bytes(), &executeResponse); err != nil {
		t.Fatal(err)
	}
	if executeResponse.Result.Phase != lifecycle.PhaseCommitted || executeResponse.Result.Archive == nil || executeResponse.Result.Mutation == nil || !executeResponse.Result.Mutation.Verified {
		t.Fatalf("真实副本 API 执行结果不完整: %+v", executeResponse.Result)
	}
	if _, err := compat.InspectRealShapeCopy(context.Background(), root, apiRealCopyThreadID); err == nil {
		t.Fatal("API 执行后真实副本仍被识别为完整目标")
	}

	archiveDirectory := executeResponse.Result.Archive.Directory
	previewBody := `{"archive_path":"` + archiveDirectory + `","target_root":"` + root + `","source_operation_id":"` + executeResponse.Result.OperationID + `"}`
	restorePreview := apiJSON(t, handler, http.MethodPost, "/api/v1/archives/local/restore/preview", previewBody)
	if restorePreview.Code != http.StatusOK || !strings.Contains(restorePreview.Body.String(), `"blocked":false`) {
		t.Fatalf("真实副本 API 恢复预览失败: status=%d body=%s", restorePreview.Code, restorePreview.Body.String())
	}
	var restorePreviewResponse struct {
		RestoreOperationID string `json:"restore_operation_id"`
	}
	if err := json.Unmarshal(restorePreview.Body.Bytes(), &restorePreviewResponse); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(restorePreviewResponse.RestoreOperationID) == "" {
		t.Fatalf("恢复预览缺少操作 ID: %s", restorePreview.Body.String())
	}

	restoreExecute := apiJSON(t, handler, http.MethodPost, "/api/v1/archives/local/restore/execute", `{"restore_operation_id":"`+restorePreviewResponse.RestoreOperationID+`"}`)
	if restoreExecute.Code != http.StatusOK || !strings.Contains(restoreExecute.Body.String(), `"blocked":false`) {
		t.Fatalf("真实副本 API 恢复执行失败: status=%d body=%s", restoreExecute.Code, restoreExecute.Body.String())
	}
	if _, err := compat.InspectRealShapeCopy(context.Background(), root, apiRealCopyThreadID); err != nil {
		t.Fatalf("API 恢复后真实副本仍不可读: %v", err)
	}
}

func TestRealCopyBatchLifecycleThroughHTTPAPI(t *testing.T) {
	root := createAPIRealCopyRoot(t)
	journalRoot := filepath.Join(t.TempDir(), "journals")
	mutation, err := compat.NewRealShapeCopyMutation(filepath.Join(journalRoot, "mutation-snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerWithOptions(root, nil, &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.RealShapeCopyDescriptor(),
		ControlPlaneMutation: mutation, ArchiveRoot: filepath.Join(t.TempDir(), "archives"),
		JournalRoot: journalRoot, TrashRoot: filepath.Join(t.TempDir(), "trash"),
	})
	handler := server.Handler()
	conversationIDs := []string{compat.StableConversationID(apiRealCopyThreadID), compat.StableConversationID("00000000-0000-4000-8000-000000000002")}
	body := `{"conversation_ids":["` + conversationIDs[0] + `","` + conversationIDs[1] + `"],"archive":true,"archive_local":true,"trash":true}`
	preview := apiJSON(t, handler, http.MethodPost, "/api/v1/operations/batch/preview", body)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"prepared":2`) {
		t.Fatalf("真实副本批量预览失败: status=%d body=%s", preview.Code, preview.Body.String())
	}
	execute := executeReviewedAPI(t, handler, "/api/v1/operations/batch/execute", body)
	if execute.Code != http.StatusOK {
		t.Fatalf("真实副本批量执行失败: status=%d body=%s", execute.Code, execute.Body.String())
	}
	var response struct {
		Blocked bool `json:"blocked"`
		Items   []struct {
			Completed bool   `json:"completed"`
			Error     string `json:"error,omitempty"`
		} `json:"items"`
	}
	if err := json.Unmarshal(execute.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Blocked || len(response.Items) != 2 || !response.Items[0].Completed || !response.Items[1].Completed {
		t.Fatalf("真实副本批量操作没有全部完成: %+v body=%s", response, execute.Body.String())
	}
	for _, threadID := range []string{apiRealCopyThreadID, "00000000-0000-4000-8000-000000000002"} {
		if _, err := compat.InspectRealShapeCopy(context.Background(), root, threadID); err == nil {
			t.Fatalf("批量执行后 thread %s 仍被识别为完整副本", threadID)
		}
	}
}

func createAPIRealCopyRoot(t *testing.T) string {
	t.Helper()
	python := "python3"
	if _, err := exec.LookPath(python); err != nil {
		python = "python"
		if _, err := exec.LookPath(python); err != nil {
			t.Skip("未找到 Python 解释器，跳过真实副本 API 集成回归")
		}
	}
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Clean(filepath.Join(workingDirectory, "..", "..", "docs", "项目验证", "codex_realshape_fixture.py"))
	if output, err := exec.Command(python, script, "--root", source).CombinedOutput(); err != nil {
		t.Fatalf("生成真实副本 API fixture 失败: %v\n%s", err, output)
	}
	destination := filepath.Join(base, "codex-copy")
	plan, err := copy.Prepare(context.Background(), copy.CopyRequest{
		SourceRoot: source, DestinationRoot: destination,
		Marker:                       copy.MarkerSpec{Kind: copy.ManifestKind, AdapterID: compat.RealShapeCopyKind, SchemaSHA256: compat.RealShapeFixtureSchemaSHA256, SQLiteSemanticsSHA256: compat.RealShapeSQLiteSemanticsSHA256},
		AllowUnavailableProcessProbe: true,
	})
	if err != nil {
		t.Fatalf("准备真实副本 API 复制计划失败: %v", err)
	}
	result, err := copy.Execute(context.Background(), plan, copy.ExecuteOptions{})
	if err != nil || !result.Published {
		t.Fatalf("发布真实副本 API fixture 失败: result=%+v err=%v", result, err)
	}
	return destination
}

func apiJSON(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func snapshotRealCopyTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("真实副本包含非普通文件: %s", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		snapshot[relative] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
