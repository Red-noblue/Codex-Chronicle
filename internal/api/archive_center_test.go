// 中文说明：验证归档中心删除本地副本时直接删除完整归档文件夹，并及时从活动列表移除。
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
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/archive"
)

func TestArchiveCenterDeleteLocalRemovesWholeArchiveAndHidesLastCopy(t *testing.T) {
	root := t.TempDir()
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	task, archivePath := archiveCenterLocalTask(t, root, archiveRoot)
	server := NewServerWithOptions(root, nil, &LifecycleOptions{ArchiveRoot: archiveRoot})
	defer server.Close()
	state := &cloudBackupState{root: filepath.Join(t.TempDir(), "cloud-backups"), tasks: map[string]cloudTask{task.ID: task}}
	if err := os.MkdirAll(state.root, 0o700); err != nil {
		t.Fatal(err)
	}
	server.cloud = state

	previewResponse := archiveCenterRequest(t, server.Handler(), http.MethodPost, "/api/v1/archive-center/"+task.ID+"/preview", map[string]any{"action": "delete_local", "source": "local"}, http.StatusOK)
	var preview archiveManagementReview
	if err := json.Unmarshal(previewResponse, &preview); err != nil || preview.ID == "" || preview.Code == "" {
		t.Fatalf("本地删除预览缺少确认信息: preview=%+v err=%v", preview, err)
	}
	archiveCenterRequest(t, server.Handler(), http.MethodPost, "/api/v1/archive-center/"+task.ID+"/execute", map[string]any{
		"preview_id": preview.ID, "confirmation_code": preview.Code, "confirm": true,
	}, http.StatusAccepted)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state.mu.Lock()
		current := state.tasks[task.ID]
		active := state.active
		state.mu.Unlock()
		if active == "" && current.ManagementState == "completed" {
			if current.LocalCopy || current.LocalPath != "" {
				t.Fatalf("本地归档删除状态不正确: %+v", current)
			}
			if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
				t.Fatalf("本地归档文件夹没有直接删除: %v", err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	state.mu.Lock()
	completed := state.tasks[task.ID].ManagementState == "completed" && state.active == ""
	state.mu.Unlock()
	if !completed {
		t.Fatal("本地归档删除任务未完成")
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/archive-center", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), task.ID) {
		t.Fatalf("最后一个副本删除后仍显示在活动归档列表: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLatestRestoredArchiveBaseUsesExactConversationSet(t *testing.T) {
	baseTime := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	restoredAt := baseTime.Add(time.Hour)
	state := &cloudBackupState{tasks: map[string]cloudTask{
		"older": {ID: "older", SourceRoot: "/codex", SourceIDs: []string{"thread-a", "thread-b"},
			CreatedAt: baseTime, RestoreStatus: "restored", RestoreTargetDigest: "version-a", RestoredAt: &restoredAt},
		"subset": {ID: "subset", SourceRoot: "/codex", SourceIDs: []string{"thread-a"},
			CreatedAt: baseTime.Add(2 * time.Hour), RestoreStatus: "restored", RestoreTargetDigest: "wrong-set", RestoredAt: &restoredAt},
	}}
	selected := cloudTask{ID: "newer", SourceRoot: "/codex", SourceIDs: []string{"thread-b", "thread-a"}, CreatedAt: baseTime.Add(3 * time.Hour)}
	base, ok := latestRestoredArchiveBase(state, selected)
	if !ok || base.ID != "older" || base.RestoreTargetDigest != "version-a" {
		t.Fatalf("没有按精确对话集合找到恢复基线: base=%+v ok=%t", base, ok)
	}
}

func archiveCenterLocalTask(t *testing.T, sourceRoot, archiveRoot string) (cloudTask, string) {
	t.Helper()
	const conversationID = "11111111-2222-4333-8444-555555555555"
	const taskID = "backup-0123456789abcdef01234567"
	relative := "sessions/rollout-" + conversationID + ".jsonl"
	body := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"" + conversationID + "\"}}\n")
	exported := filepath.Join(t.TempDir(), "export")
	if err := os.MkdirAll(filepath.Join(exported, "files", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exported, "files", filepath.FromSlash(relative)), body, 0o600); err != nil {
		t.Fatal(err)
	}
	bodyHash := sha256.Sum256(body)
	snapshot := map[string]any{
		"kind": "chronicle-cloud-snapshot-v1", "ids": []string{conversationID}, "requested_ids": []string{conversationID},
		"assets":    []any{map[string]any{"relative": relative, "id": conversationID, "bytes": len(body), "sha256": hex.EncodeToString(bodyHash[:])}},
		"databases": map[string]any{"state_5.sqlite": map[string]any{"schema_sha256": strings.Repeat("a", 64), "rows": []any{map[string]any{"table": "threads", "owner": "id", "columns": []string{"id", "title"}, "values": [][]string{{conversationID, "测试归档"}}}}}},
		"indexes":   map[string]any{},
	}
	snapshotBytes, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exported, "snapshot.json"), snapshotBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(archiveRoot, taskID)
	if err := os.MkdirAll(archiveRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata, err := archive.CreateCloudBundle(context.Background(), exported, archivePath, taskID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(archivePath, "manifest.json"))
	if err != nil || len(manifest) == 0 {
		t.Fatalf("归档 manifest 创建失败: err=%v", err)
	}
	return cloudTask{
		ID: taskID, ArchiveID: taskID, State: "completed", SourceRoot: sourceRoot,
		SourceIDs: []string{conversationID}, PreviewDigest: metadata.SnapshotSHA256, CreatedAt: time.Now().UTC(),
		Metadata: &metadata, LocalCopy: true, LocalPath: archivePath,
	}, archivePath
}

func archiveCenterRequest(t *testing.T, handler http.Handler, method, path string, body any, expectedStatus int) []byte {
	t.Helper()
	var requestBody []byte
	if body != nil {
		var err error
		requestBody, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, strings.NewReader(string(requestBody)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != expectedStatus {
		t.Fatalf("归档中心请求状态异常: %s %s status=%d body=%s", method, path, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}
