// 中文说明：验证本地 API 的只读能力声明、固定扫描根和错误边界。
package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/auth"
	"codex-chronicle.local/internal/compat"
	copypkg "codex-chronicle.local/internal/copy"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
	maintenancepkg "codex-chronicle.local/internal/maintenance"
	"codex-chronicle.local/internal/platform"
	resourcepkg "codex-chronicle.local/internal/resource"
	"codex-chronicle.local/internal/store"
	updatepkg "codex-chronicle.local/internal/update"
	_ "modernc.org/sqlite"
)

type apiFixedProcessDetector struct{}

func (apiFixedProcessDetector) Detect(_ context.Context, root string) activity.ProcessSnapshot {
	path := filepath.Join(root, "rollout.jsonl")
	return activity.ProcessSnapshot{
		Available: true,
		Reason:    "API 测试进程探测器",
		Files: map[string][]domain.ProcessHandle{
			path: {{PID: 9001, Command: "codex"}},
		},
	}
}

func apiConversationIDs(t *testing.T, handler http.Handler) []string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("读取测试对话失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(response.Items))
	for _, item := range response.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestCapabilitiesAreReadOnly(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	recorder := httptest.NewRecorder()
	NewServer("").Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
	var response struct {
		ReadOnly                  bool `json:"read_only"`
		RealConversationExecution struct {
			DaemonVersions []string `json:"daemon_versions"`
		} `json:"real_conversation_execution"`
		Adapter struct {
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"adapter"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ReadOnly {
		t.Fatal("capabilities endpoint must be read-only")
	}
	if strings.Join(response.RealConversationExecution.DaemonVersions, ",") != "0.166.0,0.167.0,0.167.2,0.169.0" {
		t.Fatalf("capabilities endpoint must advertise only reviewed daemon versions: %+v", response.RealConversationExecution.DaemonVersions)
	}
	for _, capability := range []string{"repair", "archive_export", "delete", "restore", "rebuild_index"} {
		if response.Adapter.Capabilities[capability] {
			t.Fatalf("capability %q unexpectedly enabled", capability)
		}
	}
}

func TestCapabilitiesExposeFixtureWriteBoundary(t *testing.T) {
	server := NewServerWithOptions(t.TempDir(), nil, &LifecycleOptions{
		Enabled:     true,
		FixtureOnly: true,
		Descriptor:  compat.SyntheticFixtureDescriptor(),
	})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("fixture capabilities request failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ReadOnly     bool `json:"read_only"`
		WriteEnabled bool `json:"write_enabled"`
		FixtureOnly  bool `json:"fixture_only"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ReadOnly || !response.WriteEnabled || !response.FixtureOnly {
		t.Fatalf("fixture 写入边界声明错误: %+v body=%s", response, recorder.Body.String())
	}
}

func TestRemoteAuthBindsProtectsAndRevokesSession(t *testing.T) {
	manager, bootstrap, err := auth.NewManager(auth.Config{StatePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer("")
	server.SetAuthManager(manager)
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("remote request without session was accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	request.RemoteAddr = "127.0.0.1:4000"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("loopback request without session was accepted while remote auth is enabled: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	bindBody, err := json.Marshal(map[string]string{"code": bootstrap.Code, "device": "测试浏览器", "password": "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/bind", strings.NewReader(string(bindBody)))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("remote bind failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("auth response must not be cached: headers=%v", recorder.Header())
	}
	var bindResponse struct {
		Session auth.Session `json:"session"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &bindResponse); err != nil || bindResponse.Session.Token == "" {
		t.Fatalf("binding response did not contain session token: err=%v body=%s", err, recorder.Body.String())
	}
	token := bindResponse.Session.Token

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated remote request failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/sessions", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), token) {
		t.Fatalf("session listing exposed token or failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session was accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRemoteAuthResetBindingIsLoopbackOnly(t *testing.T) {
	manager, bootstrap, err := auth.NewManager(auth.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer("")
	server.SetAuthManager(manager)
	handler := server.Handler()
	bindBody := strings.NewReader(`{"code":"` + bootstrap.Code + `","device":"本机","password":"correct horse battery staple"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/bind", bindBody)
	request.RemoteAddr = "127.0.0.1:54321"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("local bind failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var bindResponse struct {
		Session auth.Session `json:"session"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &bindResponse); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/reset-binding", nil)
	request.RemoteAddr = "192.0.2.8:54321"
	request.Header.Set("Authorization", "Bearer "+bindResponse.Session.Token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote reset binding was accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/reset-binding", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("Authorization", "Bearer "+bindResponse.Session.Token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"bound":false`) {
		t.Fatalf("local reset binding failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCapabilitiesReportReadOnlyRemoteCatalogConfigured(t *testing.T) {
	handler := NewServerWithOptions("", nil, &LifecycleOptions{RemoteArchiveCatalog: apiRemoteArchiveCatalog{}}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"remote_archive_configured":true`) {
		t.Fatalf("read-only remote catalog was not reported: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResourcesExposeAndEnforceDerivedBudget(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "archive.db")
	archive, err := store.Open(t.Context(), archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	server := NewServerWithStore(t.TempDir(), archive)
	if err := server.SetResourceBudget(resourcepkg.Budget{
		MemoryLimitBytes: resourcepkg.DefaultMemoryLimitBytes, PersistentLimitBytes: 1, TemporaryLimitBytes: resourcepkg.DefaultTemporaryLimitBytes,
	}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/resources", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"within_budget":false`) {
		t.Fatalf("resource endpoint did not expose persistent overflow: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/catalog/sync", nil))
	if recorder.Code != http.StatusInsufficientStorage || !strings.Contains(recorder.Body.String(), "资源预算") {
		t.Fatalf("catalog sync was not blocked by persistent budget: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("resource probe should not remove archive.db: %v", err)
	}
}

func TestResourcesIncludePersistedCopyPlans(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".codex")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	planRoot := filepath.Join(base, ".codex-chronicle-copy-plans")
	if err := os.MkdirAll(planRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planRoot, "copy-plan-test.json"), make([]byte, 17), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(root)
	if err := server.SetResourceBudget(resourcepkg.Budget{
		MemoryLimitBytes: resourcepkg.DefaultMemoryLimitBytes, PersistentLimitBytes: 16, TemporaryLimitBytes: resourcepkg.DefaultTemporaryLimitBytes,
	}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/resources", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"within_budget":false`) || !strings.Contains(recorder.Body.String(), ".codex-chronicle-copy-plans") {
		t.Fatalf("副本计划目录未纳入资源预算：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestReadinessReportFailsClosedForRealCodexWrites(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer("").Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/readiness", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"not_ready"`) || !strings.Contains(recorder.Body.String(), `"real_codex_write_ready":false`) || !strings.Contains(recorder.Body.String(), `"id":"real-codex-mutation"`) {
		t.Fatalf("readiness report did not fail closed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestReadinessReportsRemoteAuthWhenConfigured(t *testing.T) {
	manager, bootstrap, err := auth.NewManager(auth.Config{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Bind(auth.BindRequest{Code: bootstrap.Code, Device: "readiness-test", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer("")
	server.SetAuthManager(manager)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/readiness", nil)
	request.Header.Set("Authorization", "Bearer "+session.Token)
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"remote-auth"`) || !strings.Contains(recorder.Body.String(), `"status":"implemented"`) {
		t.Fatalf("configured remote auth was not reported: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestInventoryUsesConfiguredRootOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.jsonl"), []byte("{\"type\":\"session_meta\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := NewServer(root).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/inventory?root=/should/not/be/used", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ReadOnly bool   `json:"read_only"`
		Root     string `json:"root"`
		Files    []struct {
			RelativePath string `json:"relative_path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ReadOnly || response.Root != root || len(response.Files) != 1 || response.Files[0].RelativePath != "one.jsonl" {
		t.Fatalf("unexpected inventory response: %+v", response)
	}
}

func TestInventorySnapshotMetadataAndRefresh(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"api-snapshot\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	readInventory := func(path string) (int, map[string]any) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析 inventory 响应失败: %v body=%s", err, recorder.Body.String())
		}
		return recorder.Code, body
	}
	status, first := readInventory("/api/v1/inventory?offset=0&limit=1")
	if status != http.StatusOK || first["snapshot_status"] != "fresh" || first["stale"] != false || first["scan_task_id"] == "" {
		t.Fatalf("首次 inventory 快照元数据错误: status=%d body=%v", status, first)
	}
	firstTaskID := first["scan_task_id"]
	status, second := readInventory("/api/v1/inventory?offset=0&limit=1")
	if status != http.StatusOK || second["scan_task_id"] != firstTaskID || second["snapshot_status"] != "fresh" {
		t.Fatalf("第二次读取没有复用快照: status=%d first=%v second=%v", status, first, second)
	}
	if err := os.WriteFile(filepath.Join(root, "two.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"api-snapshot-two\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, stale := readInventory("/api/v1/inventory?offset=0&limit=1")
	if status != http.StatusOK || stale["snapshot_status"] != "stale" || stale["stale"] != true {
		t.Fatalf("源目录变化没有暴露 stale: status=%d body=%v", status, stale)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inventory/refresh", nil))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("后台刷新没有返回 202: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var refresh struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &refresh); err != nil || refresh.Task.ID == "" {
		t.Fatalf("刷新响应没有任务 ID: err=%v body=%s", err, recorder.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+url.PathEscape(refresh.Task.ID), nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("读取刷新任务失败: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), `"status":"completed"`) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, refreshed := readInventory("/api/v1/inventory?offset=0&limit=1")
	if status != http.StatusOK || refreshed["snapshot_status"] != "fresh" || refreshed["stale"] != false {
		t.Fatalf("刷新完成后快照仍不 fresh: status=%d body=%v", status, refreshed)
	}
}

func TestStableCopyEndpointsRequirePrepareThenExplicitExecute(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".codex")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(`{"type":"session_meta"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(root)
	handler := server.Handler()
	destination := filepath.Join(base, "copies", "one")

	previewBody, err := json.Marshal(map[string]any{
		"destination_root":                destination,
		"allow_unavailable_process_probe": runtime.GOOS == "windows",
		"chunk_bytes":                     32768,
		"exclude_paths":                   []string{"auth.json"},
		"marker":                          map[string]string{"adapter_id": "observed-test", "schema_sha256": "schema-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/copies", strings.NewReader(string(previewBody)))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("copy prepare endpoint failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var prepareResponse struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &prepareResponse); err != nil || prepareResponse.Task.ID == "" {
		t.Fatalf("copy prepare response missing task: err=%v body=%s", err, recorder.Body.String())
	}
	prepareID := prepareResponse.Task.ID
	prepareTask := waitAPICopyTask(t, handler, prepareID)
	if !strings.Contains(prepareTask, `"status":"completed"`) || !strings.Contains(prepareTask, `"plan"`) || !strings.Contains(prepareTask, `"adapter_id":"observed-test"`) {
		t.Fatalf("copy prepare did not complete: %s", prepareTask)
	}
	// 新 daemon 只读取落盘摘要即可重新发现待确认计划，不需要把完整文件清单载入 API。
	restartedHandler := NewServer(root).Handler()
	restartedRecorder := httptest.NewRecorder()
	restartedHandler.ServeHTTP(restartedRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/copies", nil))
	if restartedRecorder.Code != http.StatusOK || !strings.Contains(restartedRecorder.Body.String(), prepareID) {
		t.Fatalf("重启后副本计划未被列出：status=%d body=%s", restartedRecorder.Code, restartedRecorder.Body.String())
	}

	// 使用重启后的 handler 直接确认落盘计划，验证 daemon 重启不会丢失待确认副本。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/copies/"+url.PathEscape(prepareID)+"/execute", strings.NewReader(`{}`))
	restartedHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("copy execute endpoint failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var executeResponse struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &executeResponse); err != nil || executeResponse.Task.ID == "" {
		t.Fatalf("copy execute response missing task: err=%v body=%s", err, recorder.Body.String())
	}
	executeTask := waitAPICopyTask(t, restartedHandler, executeResponse.Task.ID)
	if !strings.Contains(executeTask, `"status":"completed"`) || !strings.Contains(executeTask, `"published":true`) {
		t.Fatalf("copy execute did not complete: %s", executeTask)
	}
	if _, err := os.Stat(filepath.Join(destination, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("explicitly excluded credential file was copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "codex-real-copy-only.json")); err != nil {
		t.Fatalf("published copy manifest missing: %v", err)
	}
	thirdRestartRecorder := httptest.NewRecorder()
	NewServer(root).Handler().ServeHTTP(thirdRestartRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/copies", nil))
	if thirdRestartRecorder.Code != http.StatusOK || strings.Contains(thirdRestartRecorder.Body.String(), prepareID) {
		t.Fatalf("已发布计划不应在重启后继续列出：status=%d body=%s", thirdRestartRecorder.Code, thirdRestartRecorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/copies/verify", strings.NewReader(`{"root":`+strconv.Quote(destination)+`}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("copy verify endpoint failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestLiveCopyEndpointsPublishPartialSnapshotWhileAssetIsActive(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".codex")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(`{"type":"complete"}`+"\n"+`{"type":"pending"`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	destination := filepath.Join(base, "live-copy")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/live-copies", strings.NewReader(`{"destination_root":`+strconv.Quote(destination)+`}`)))
	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"mode":"live_best_effort"`) {
		t.Fatalf("live prepare endpoint failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var prepare struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &prepare); err != nil || prepare.Task.ID == "" {
		t.Fatalf("live prepare task missing: %v body=%s", err, recorder.Body.String())
	}
	prepareBody := waitAPILiveTask(t, handler, prepare.Task.ID)
	if !strings.Contains(prepareBody, `"status":"completed"`) || !strings.Contains(prepareBody, `"active_count":1`) {
		t.Fatalf("live prepare did not retain active asset evidence: %s", prepareBody)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/live-copies/"+url.PathEscape(prepare.Task.ID)+"/execute", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("live execute endpoint failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var execute struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &execute); err != nil || execute.Task.ID == "" {
		t.Fatalf("live execute task missing: %v body=%s", err, recorder.Body.String())
	}
	executeBody := waitAPILiveTask(t, handler, execute.Task.ID)
	if !strings.Contains(executeBody, `"status":"completed"`) || !strings.Contains(executeBody, `"published":true`) || !strings.Contains(executeBody, `"complete":false`) {
		t.Fatalf("live execute did not publish partial result: %s", executeBody)
	}
	if _, err := os.Stat(filepath.Join(destination, copypkg.LiveManifestName)); err != nil {
		t.Fatalf("live manifest missing: %v", err)
	}
}

func waitAPICopyTask(t *testing.T, handler http.Handler, id string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/copies/"+url.PathEscape(id), nil))
		body := recorder.Body.String()
		if strings.Contains(body, `"status":"completed"`) || strings.Contains(body, `"status":"failed"`) || strings.Contains(body, `"status":"cancelled"`) {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("copy task did not finish: %s", id)
	return ""
}

func waitAPILiveTask(t *testing.T, handler http.Handler, id string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/live-copies/"+url.PathEscape(id), nil))
		body := recorder.Body.String()
		if strings.Contains(body, `"status":"completed"`) || strings.Contains(body, `"status":"failed"`) || strings.Contains(body, `"status":"cancelled"`) {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("live copy task did not finish: %s", id)
	return ""
}

func TestWorkspaceRegistrySwitchesReadAndCatalogContextWithoutDeletingSources(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "codex-a")
	rootB := filepath.Join(base, "codex-b")
	for _, root := range []string{rootA, rootB} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootA, "only-a.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"workspace-a\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "only-b.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"workspace-b\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archiveB := filepath.Join(base, "archive-b.db")
	server, err := NewServerWithWorkspaceConfig(rootA, nil, nil, filepath.Join(base, "config", "workspaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	postWorkspace := func(body string) (int, string) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(body)))
		return recorder.Code, recorder.Body.String()
	}
	status, body := postWorkspace(`{"name":"工作区 B","root":` + strconv.Quote(rootB) + `,"archive_db":` + strconv.Quote(archiveB) + `}`)
	if status != http.StatusCreated {
		t.Fatalf("add workspace failed: status=%d body=%s", status, body)
	}
	var added struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal([]byte(body), &added); err != nil || added.Workspace.ID == "" {
		t.Fatalf("workspace ID missing: err=%v body=%s", err, body)
	}
	workspaceID := added.Workspace.ID

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/workspaces", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"workspace_count":2`) {
		t.Fatalf("unexpected workspace list: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/select", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("workspace select failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inventory", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), strconv.Quote(rootB)) || !strings.Contains(recorder.Body.String(), "only-b.jsonl") || strings.Contains(recorder.Body.String(), "only-a.jsonl") {
		t.Fatalf("inventory did not follow selected workspace: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=workspace-b&scope=technical", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "only-b.jsonl") {
		t.Fatalf("search did not follow selected workspace: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/catalog/sync", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"file_count":1`) {
		t.Fatalf("catalog sync did not use selected archive: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	archive, err := store.Open(t.Context(), archiveB)
	if err != nil {
		t.Fatal(err)
	}
	assetCount, err := archive.AssetCount(t.Context())
	_ = archive.Close()
	if err != nil || assetCount != 1 {
		t.Fatalf("selected archive was not updated: count=%d err=%v", assetCount, err)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/"+workspaceID+"/remove", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("current workspace removal should be blocked: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/select", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("reselect workspace failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// 注册表操作只移除元数据；源根目录和 archive.db 必须保持不变。
	legacyID := ""
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/workspaces", nil))
	var listed struct {
		Workspaces []struct {
			ID string `json:"id"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil || len(listed.Workspaces) != 2 {
		t.Fatalf("workspace list after select invalid: err=%v body=%s", err, recorder.Body.String())
	}
	for _, item := range listed.Workspaces {
		if item.ID != workspaceID {
			legacyID = item.ID
		}
	}
	if legacyID == "" {
		t.Fatal("legacy workspace ID missing")
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+legacyID+"/select", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("select legacy workspace failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/"+workspaceID+"/remove", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("workspace removal failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(rootA); err != nil {
		t.Fatalf("source root A was removed: %v", err)
	}
	if _, err := os.Stat(rootB); err != nil {
		t.Fatalf("source root B was removed: %v", err)
	}
	if _, err := os.Stat(archiveB); err != nil {
		t.Fatalf("archive.db was removed with registry entry: %v", err)
	}
}

func TestInventoryPaginationReturnsTotalsForWindowedWorkspaces(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 2; index++ {
		content := `{"type":"session_meta","payload":{"id":"window-thread-` + string(rune('a'+index)) + `"}}` + "\n"
		if err := os.WriteFile(filepath.Join(root, "rollout-"+string(rune('a'+index))+".jsonl"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recorder := httptest.NewRecorder()
	NewServer(root).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inventory?offset=0&limit=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected paged inventory status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Files             []json.RawMessage `json:"files"`
		Conversations     []json.RawMessage `json:"conversations"`
		FileTotal         int               `json:"file_total"`
		ConversationTotal int               `json:"conversation_total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Files) != 1 || len(response.Conversations) != 1 || response.FileTotal != 2 || response.ConversationTotal != 2 {
		t.Fatalf("paged inventory lost totals: %+v", response)
	}
}

func TestMaintenancePaginationKeepsInventoryResponsesBounded(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 35; index++ {
		path := filepath.Join(root, "tmp", "candidate-"+strconv.Itoa(index)+".tmp")
		if err := os.WriteFile(path, []byte("temporary"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewServer(root).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inventory?offset=0&limit=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected paged inventory status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var inventory struct {
		Maintenance struct {
			CandidateCount  int               `json:"candidate_count"`
			CandidateOffset int               `json:"candidate_offset"`
			CandidateLimit  int               `json:"candidate_limit"`
			HasMore         bool              `json:"has_more"`
			Candidates      []json.RawMessage `json:"candidates"`
		} `json:"maintenance"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Maintenance.CandidateCount != 35 || inventory.Maintenance.CandidateOffset != 0 || inventory.Maintenance.CandidateLimit != 30 || !inventory.Maintenance.HasMore || len(inventory.Maintenance.Candidates) != 30 {
		t.Fatalf("inventory maintenance page was not bounded: %+v", inventory.Maintenance)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/maintenance?offset=30&limit=10", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected maintenance page status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var page struct {
		CandidateCount  int               `json:"candidate_count"`
		CandidateOffset int               `json:"candidate_offset"`
		CandidateLimit  int               `json:"candidate_limit"`
		HasMore         bool              `json:"has_more"`
		Candidates      []json.RawMessage `json:"candidates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.CandidateCount != 35 || page.CandidateOffset != 30 || page.CandidateLimit != 10 || page.HasMore || len(page.Candidates) != 5 {
		t.Fatalf("maintenance endpoint page was not bounded: %+v", page)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/maintenance?q=CANDIDATE-34&category=temporary&eligibility=eligible&sort=path&limit=1&offset=0", nil))
	var filteredPage struct {
		CandidateCount  int                        `json:"candidate_count"`
		FilteredCount   int                        `json:"filtered_count"`
		SourceFileCount int                        `json:"source_file_count"`
		WriteEnabled    bool                       `json:"write_enabled"`
		Candidates      []maintenancepkg.Candidate `json:"candidates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &filteredPage); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || filteredPage.CandidateCount != 35 || filteredPage.FilteredCount != 1 || filteredPage.SourceFileCount != 35 || filteredPage.WriteEnabled || len(filteredPage.Candidates) != 1 || filteredPage.Candidates[0].RelativePath != "tmp/candidate-34.tmp" {
		t.Fatalf("维护检索必须覆盖未载入页，并保留只读权限和完整统计：%s", recorder.Body.String())
	}
	for _, query := range []string{"category=unknown", "eligibility=delete", "sort=delete"} {
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/maintenance?"+query, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("无效筛选不应被忽略：%s", query)
		}
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inventory?q=candidate-0&offset=0&limit=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected filtered inventory status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var filtered struct {
		Maintenance struct {
			CandidateCount int               `json:"candidate_count"`
			Candidates     []json.RawMessage `json:"candidates"`
		} `json:"maintenance"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if filtered.Maintenance.CandidateCount != 35 || len(filtered.Maintenance.Candidates) != 30 {
		t.Fatalf("maintenance candidates were unexpectedly narrowed by file filter: %+v", filtered.Maintenance)
	}
}

func TestInventoryExposesExactAdapterResolution(t *testing.T) {
	root := t.TempDir()
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"adapter-thread\",\"cli_version\":\"fixture-api-v1\"}}\n"
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := compat.NewRegistry(compat.Descriptor{
		ID: "api-fixture-adapter", SupportedVersions: []string{"fixture-api-v1"},
		Capabilities: map[compat.Capability]bool{compat.CapabilityRead: true},
	})
	handler := NewServerWithOptionsAndRegistry(root, nil, nil, registry).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inventory", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"api-fixture-adapter"`) || !strings.Contains(recorder.Body.String(), `"matched":true`) {
		t.Fatalf("exact adapter resolution was not exposed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestConversationDetailExposesCanonicalAssetGraph(t *testing.T) {
	root := t.TempDir()
	content := `{"type":"session_meta","payload":{"id":"detail-thread"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"详情资产图"}]}}
`
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	conversationID := compat.StableConversationID("detail-thread")
	handler := NewServer(root).Handler()
	for _, path := range []string{"/api/v1/conversations?offset=0&limit=1", "/api/v1/inventory?offset=0&limit=1"} {
		listRecorder := httptest.NewRecorder()
		handler.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, path, nil))
		if listRecorder.Code != http.StatusOK {
			t.Fatalf("unexpected conversation list status for %s: %d body=%s", path, listRecorder.Code, listRecorder.Body.String())
		}
		if strings.Contains(listRecorder.Body.String(), `"asset_graph"`) {
			t.Fatalf("list response unexpectedly includes full asset graph for %s", path)
		}
	}
	lightRecorder := httptest.NewRecorder()
	handler.ServeHTTP(lightRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+url.PathEscape(conversationID)+"?graph=none", nil))
	if lightRecorder.Code != http.StatusOK || strings.Contains(lightRecorder.Body.String(), `"asset_graph"`) || strings.Contains(lightRecorder.Body.String(), `"schema_fingerprint"`) || strings.Contains(lightRecorder.Body.String(), `"relationships"`) {
		t.Fatalf("lightweight conversation detail still includes heavy projections: status=%d body_bytes=%d", lightRecorder.Code, lightRecorder.Body.Len())
	}
	graphRecorder := httptest.NewRecorder()
	handler.ServeHTTP(graphRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+url.PathEscape(conversationID)+"/asset-graph?node_offset=0&edge_offset=0&limit=1", nil))
	if graphRecorder.Code != http.StatusOK {
		t.Fatalf("unexpected asset graph page status: %d body=%s", graphRecorder.Code, graphRecorder.Body.String())
	}
	var graphPage struct {
		Graph       domain.ConversationAssetGraph `json:"asset_graph"`
		NodeTotal   int                           `json:"node_total"`
		EdgeTotal   int                           `json:"edge_total"`
		NodeHasMore bool                          `json:"node_has_more"`
		EdgeHasMore bool                          `json:"edge_has_more"`
	}
	if err := json.Unmarshal(graphRecorder.Body.Bytes(), &graphPage); err != nil {
		t.Fatal(err)
	}
	if len(graphPage.Graph.Nodes) != 1 || len(graphPage.Graph.Edges) != 1 || graphPage.NodeTotal != 3 || graphPage.EdgeTotal != 3 || !graphPage.NodeHasMore || !graphPage.EdgeHasMore || graphPage.Graph.Hash == "" {
		t.Fatalf("asset graph paging lost canonical metadata: %+v", graphPage)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+url.PathEscape(conversationID), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected conversation detail status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Conversation struct {
			AssetSetHash string                         `json:"asset_set_hash"`
			AssetGraph   *domain.ConversationAssetGraph `json:"asset_graph"`
		} `json:"conversation"`
		Files []domain.FileInventory `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Conversation.AssetGraph == nil || response.Conversation.AssetGraph.Hash == "" || response.Conversation.AssetSetHash != response.Conversation.AssetGraph.Hash {
		t.Fatalf("conversation detail lost canonical asset graph: %+v", response.Conversation)
	}
	if response.Conversation.AssetGraph.Completeness != "complete" || len(response.Conversation.AssetGraph.Nodes) < 2 || len(response.Conversation.AssetGraph.Edges) == 0 || len(response.Files) != 1 {
		t.Fatalf("unexpected detail asset graph projection: %+v files=%d", response.Conversation.AssetGraph, len(response.Files))
	}
}

func TestConversationListItemsBoundLargePreviewFields(t *testing.T) {
	items := conversationListItems([]domain.ConversationSummary{{
		Title:         strings.Repeat("标题", 300),
		FirstUserText: strings.Repeat("用户消息", 2000),
		Relationships: []domain.SQLiteRelationship{{Table: "threads"}},
		AssetGraph:    &domain.ConversationAssetGraph{Nodes: []domain.AssetGraphNode{{ID: "node"}}},
	}})
	if len(items) != 1 {
		t.Fatalf("unexpected list projection length: %d", len(items))
	}
	if len([]rune(items[0].Title)) != 243 || len([]rune(items[0].FirstUserText)) != 2003 {
		t.Fatalf("large preview fields were not bounded: title=%d text=%d", len([]rune(items[0].Title)), len([]rune(items[0].FirstUserText)))
	}
	if items[0].AssetGraph != nil || items[0].Relationships != nil {
		t.Fatalf("heavy list fields were not removed: %+v", items[0])
	}
}

func TestInventoryRequiresConfiguredRootAndGET(t *testing.T) {
	server := NewServer("").Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/inventory", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected missing-root status: %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/inventory", nil)
	recorder = httptest.NewRecorder()
	NewServer(t.TempDir()).Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected method status: %d", recorder.Code)
	}
}

func TestSourceRangeReadsOnlyConfiguredRelativeFile(t *testing.T) {
	root := t.TempDir()
	content := []byte("前缀\n正文内容")
	if err := os.WriteFile(filepath.Join(root, "one.jsonl"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	server := NewServer(root).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/source-range?path=one.jsonl&start=0&length=7", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || string(recorder.Body.Bytes()) != "前缀\n" {
		t.Fatalf("unexpected range response: status=%d body=%q", recorder.Code, recorder.Body.Bytes())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("unexpected content type: %q", got)
	}
	if recorder.Header().Get("X-Codex-Chronicle-Read-Only") != "true" || recorder.Header().Get("X-Codex-Chronicle-Source-Changed") != "false" {
		t.Fatalf("source range lacks read-only freshness evidence: headers=%v", recorder.Header())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/source-range?path=../secret&start=0&length=1", nil)
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unexpected traversal status: %d", recorder.Code)
	}
}

func TestCatalogSyncRequiresExplicitStoreAndWritesDerivedDatabase(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.jsonl"), []byte("{\"type\":\"session_meta\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withoutStore := NewServer(root).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/catalog/sync", nil)
	recorder := httptest.NewRecorder()
	withoutStore.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected missing-store status: %d", recorder.Code)
	}

	archive, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	server := NewServerWithStore(root, archive).Handler()
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected sync status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assets, err := archive.AssetCount(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if assets != 1 {
		t.Fatalf("unexpected synced asset count: %d", assets)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/catalog/sync", nil)
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected sync method status: %d", recorder.Code)
	}
}

func TestConversationAndSearchEndpointsExposeReadOnlyEvidence(t *testing.T) {
	root := t.TempDir()
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"api-thread\",\"cwd\":\"/work/api\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"需要保留的项目对话\"}]}}\n"
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations?limit=10", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected conversations status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var conversations struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		ReadOnly bool `json:"read_only"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &conversations); err != nil {
		t.Fatal(err)
	}
	if !conversations.ReadOnly || len(conversations.Items) != 1 || conversations.Items[0].ID == "" {
		t.Fatalf("unexpected conversations response: %+v", conversations)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conversations.Items[0].ID, nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "rollout.jsonl") {
		t.Fatalf("unexpected detail response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conversations.Items[0].ID+"/events?limit=10", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "需要保留的项目对话") || !strings.Contains(recorder.Body.String(), `"start_offset"`) {
		t.Fatalf("unexpected event response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=项目对话", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "rollout.jsonl") {
		t.Fatalf("unexpected search response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	activityURL := "/api/v1/activity?conversation_id=" + url.QueryEscape(conversations.Items[0].ID) + "&interval_ms=1&windows=2&process=false&conservative=false"
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, activityURL, nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"read_only":true`) || !strings.Contains(recorder.Body.String(), `"status":"stable"`) || !strings.Contains(recorder.Body.String(), `"window_count":2`) {
		t.Fatalf("unexpected activity response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestConversationSummaryAndBatchSummaryTaskAreReadOnly(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := "{\"type\":\"session_meta\",\"timestamp\":\"2026-10-01T00:00:00Z\",\"payload\":{\"id\":\"summary-thread\",\"cwd\":\"/workspace/summary\",\"model\":\"gpt-test\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"text\":\"请整理这个项目的发布计划\"}]}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"text\":\"我会先检查工程边界\"}]}}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	ids := apiConversationIDs(t, handler)
	if len(ids) != 1 {
		t.Fatalf("unexpected conversation IDs: %v", ids)
	}
	id := url.PathEscape(ids[0])
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+id+"/summary", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "local-deterministic") || !strings.Contains(recorder.Body.String(), "发布计划") {
		t.Fatalf("unexpected single summary response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	body := `{"conversation_ids":["` + ids[0] + `"]}`
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/summaries", strings.NewReader(body)))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("unexpected batch summary start: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var started struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Task.ID == "" {
		t.Fatalf("missing summary task: %s", recorder.Body.String())
	}
	var finished struct {
		Task struct {
			Status  domain.TaskStatus                  `json:"status"`
			Results []domain.ConversationSummaryResult `json:"results"`
		} `json:"task"`
	}
	for attempt := 0; attempt < 30; attempt++ {
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/summaries/"+started.Task.ID, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("summary task lookup failed: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &finished); err != nil {
			t.Fatal(err)
		}
		if finished.Task.Status == domain.TaskCompleted || finished.Task.Status == domain.TaskFailed || finished.Task.Status == domain.TaskCancelled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if finished.Task.Status != domain.TaskCompleted || len(finished.Task.Results) != 1 || finished.Task.Results[0].ConversationID != ids[0] {
		t.Fatalf("unexpected finished summary task: %+v", finished.Task)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("summary operation modified source rollout")
	}
}

func TestConversationProjectRelationIsReadOnly(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	cwd := filepath.Join(projectRoot, "internal")
	for _, directory := range []string{filepath.Join(projectRoot, ".git"), cwd} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeAPIProjectFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAPIProjectFile(filepath.Join(projectRoot, ".git", "HEAD"), "ref: refs/heads/project-api\n")
	writeAPIProjectFile(filepath.Join(projectRoot, ".git", "config"), "[remote \"origin\"]\n\turl = https://example.com/org/codex-chronicle.git\n")
	writeAPIProjectFile(filepath.Join(projectRoot, ".gitignore"), "ignored.txt\n")
	writeAPIProjectFile(filepath.Join(projectRoot, "go.mod"), "module example\n")
	writeAPIProjectFile(filepath.Join(projectRoot, "README.md"), "project\n")
	writeAPIProjectFile(filepath.Join(projectRoot, "ignored.txt"), "ignored\n")
	rollout := filepath.Join(root, "project-thread.jsonl")
	content := "{\"type\":\"session_meta\",\"timestamp\":\"2026-10-01T00:00:00Z\",\"payload\":{\"id\":\"project-api-thread\",\"cwd\":\"" + strings.ReplaceAll(cwd, `\`, `\\`) + "\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"text\":\"项目关系测试\"}]}}\n"
	if err := os.WriteFile(rollout, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeProject, err := os.ReadFile(filepath.Join(projectRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}

	handler := NewServer(root).Handler()
	ids := apiConversationIDs(t, handler)
	if len(ids) != 1 || ids[0] != compat.StableConversationID("project-api-thread") {
		t.Fatalf("unexpected project conversation IDs: %v", ids)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+url.PathEscape(ids[0])+"/project", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected project relation status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ReadOnly bool                         `json:"read_only"`
		Project  domain.ProjectRelationReport `json:"project"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ReadOnly || response.Project.Status != "detected" || response.Project.ProjectRoot != projectRoot || response.Project.GitBranch != "project-api" {
		t.Fatalf("unexpected project relation: %+v body=%s", response, recorder.Body.String())
	}
	if response.Project.GitRemote != "https://example.com/org/codex-chronicle.git" || len(response.Project.KeyFiles) < 3 {
		t.Fatalf("project metadata missing: %+v", response.Project)
	}
	afterProject, err := os.ReadFile(filepath.Join(projectRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeProject) != string(afterProject) {
		t.Fatal("project relation scan modified project files")
	}
}

func TestOfflineUpdateAPIRequiresIndependentInstallRootAndSupportsRollback(t *testing.T) {
	root := t.TempDir()
	installRoot := filepath.Join(t.TempDir(), "install")
	manager, err := updatepkg.NewManager(installRoot)
	if err != nil {
		t.Fatal(err)
	}
	bundleV1 := makeAPIUpdateBundle(t, "1.0.0", "api-v1")
	bundleV2 := makeAPIUpdateBundle(t, "2.0.0", "api-v2")
	server := NewServer(root)
	server.SetUpdateManager(manager)
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/update/status", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"available":true`) {
		t.Fatalf("update status unavailable: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	prepare := func(bundle string) updatepkg.Plan {
		t.Helper()
		body := `{"bundle_root":` + strconv.Quote(bundle) + `}`
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/update/prepare", strings.NewReader(body)))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("update prepare failed: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Plan updatepkg.Plan `json:"plan"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Plan.ID == "" {
			t.Fatalf("update plan missing: err=%v body=%s", err, recorder.Body.String())
		}
		return response.Plan
	}
	activate := func(plan updatepkg.Plan) {
		t.Helper()
		body := `{"plan_id":` + strconv.Quote(plan.ID) + `}`
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/update/activate", strings.NewReader(body)))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"restart_required":true`) {
			t.Fatalf("update activate failed: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	activate(prepare(bundleV1))
	activate(prepare(bundleV2))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/update/rollback", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"active_version":"1.0.0"`) {
		t.Fatalf("update rollback failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/readiness", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"installer-update"`) || !strings.Contains(recorder.Body.String(), `"status":"implemented"`) || !strings.Contains(recorder.Body.String(), "Ed25519 清单签名校验可用但当前未强制") {
		t.Fatalf("readiness did not expose update implementation: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestOnlineUpdateAPIChecksDownloadsAndPreparesPlan(t *testing.T) {
	archiveBytes := makeOnlineAPIArchive(t, "online-api-v1")
	digest := sha256.Sum256(archiveBytes)
	feed := updatepkg.OnlineFeed{
		FormatVersion: updatepkg.OnlineFeedFormatVersion,
		Product:       "codex-chronicle",
		GeneratedAt:   time.Now().UTC(),
		Releases: []updatepkg.OnlineRelease{{
			Version: "3.0.0", BuildID: "api-test", OS: runtime.GOOS, Arch: runtime.GOARCH,
			URL: "PLACEHOLDER", SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(archiveBytes)),
		}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/feed.json":
			feed.Releases[0].URL = "http://" + request.Host + "/artifact"
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(feed)
		case "/artifact":
			_, _ = writer.Write(archiveBytes)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	feed.Releases[0].URL = server.URL + "/artifact"

	manager, err := updatepkg.NewManagerWithOptions(filepath.Join(t.TempDir(), "install"), updatepkg.ManagerOptions{Online: updatepkg.OnlineConfig{FeedURL: server.URL + "/feed.json"}})
	if err != nil {
		t.Fatal(err)
	}
	serverWithUpdate := NewServer(t.TempDir())
	serverWithUpdate.SetUpdateManager(manager)
	handler := serverWithUpdate.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/update/check", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"available":true`) || !strings.Contains(recorder.Body.String(), `"version":"3.0.0"`) {
		t.Fatalf("online update check failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/update/download", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"size_bytes"`) {
		t.Fatalf("online update download failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/update/prepare-online", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"version":"3.0.0"`) {
		t.Fatalf("online update prepare failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHealthPreflightExposesReadOnlyMigrationAndAdapterChecks(t *testing.T) {
	root := t.TempDir()
	archive, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	handler := NewServerWithStore(root, archive).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health/preflight", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected preflight status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Status             string                   `json:"status"`
		ReadOnly           bool                     `json:"read_only"`
		Blocked            bool                     `json:"blocked"`
		Migration          store.MigrationPreflight `json:"migration"`
		MigrationAvailable bool                     `json:"migration_available"`
		Adapter            compat.RegistryHealth    `json:"adapter"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "ready" || !response.ReadOnly || response.Blocked || !response.MigrationAvailable || response.Migration.Status != "ready" || response.Migration.CurrentVersion != store.SupportedSchemaVersion() || !response.Adapter.Valid || len(response.Adapter.Errors) != 0 {
		t.Fatalf("unexpected health preflight: %+v body=%s", response, recorder.Body.String())
	}
}

func makeAPIUpdateBundle(t *testing.T, version, content string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "codex-chronicle"), []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := updatepkg.BuildManifest(context.Background(), root, version, "api-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := updatepkg.WriteManifest(context.Background(), root, manifest); err != nil {
		t.Fatal(err)
	}
	return root
}

func makeOnlineAPIArchive(t *testing.T, content string) []byte {
	t.Helper()
	root := makeAPIUpdateBundle(t, "3.0.0", content)
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		part, err := writer.Create(filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = part.Write(data)
		return err
	})
	if err == nil {
		err = writer.Close()
	} else {
		_ = writer.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestConversationFiltersValidateTimeAndMetadataRanges(t *testing.T) {
	root := t.TempDir()
	content := "{\"type\":\"session_meta\",\"timestamp\":\"2026-10-01T00:00:00Z\",\"payload\":{\"id\":\"filter-thread\",\"model\":\"gpt-test\"}}\n"
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations?model=gpt-test&from=2026-09-01&to=2026-11-01&max_bytes=10000", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"total":1`) {
		t.Fatalf("expected metadata filters to match: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations?from=invalid-date", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid time filter rejection: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestInventoryConversationByteFilterUsesUnfilteredAssetSizes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"bytes-thread\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), []byte("{\"id\":\"bytes-thread\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT); INSERT INTO threads VALUES('bytes-thread','rollout.jsonl');"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	query := "/api/v1/inventory?q=rollout&conversation_min_bytes=1000"
	NewServer(root).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, query, nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"conversation_total":1`) {
		t.Fatalf("conversation byte filter was calculated from filtered files: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAnalyticsAndMaintenanceEndpointsAreReadOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"analytics-thread\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tmp", "stale.tmp"), []byte("temporary"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	for _, path := range []string{"/api/v1/analytics", "/api/v1/maintenance"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "read_only") {
			t.Fatalf("unexpected %s response: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/analytics", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected analytics method status: %d", recorder.Code)
	}
}

func TestMaintenanceFixturePreviewAndExecuteIsolatesSelectedTemporaryFile(t *testing.T) {
	root := t.TempDir()
	tmpPath := filepath.Join(root, "tmp", "stale.tmp")
	if err := os.MkdirAll(filepath.Dir(tmpPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPath, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	journalRoot := filepath.Join(t.TempDir(), "journal")
	trashRoot := filepath.Join(t.TempDir(), "trash")
	options := &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(),
		JournalRoot: journalRoot, TrashRoot: trashRoot,
	}
	handler := NewServerWithOptions(root, nil, options).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/preview", strings.NewReader(`{"paths":["tmp/stale.tmp"]}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) {
		t.Fatalf("unexpected maintenance preview: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		Plan struct {
			OperationID string `json:"operation_id"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil || preview.Plan.OperationID == "" {
		t.Fatalf("preview operation id missing: err=%v body=%s", err, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	body := `{"operation_id":"` + preview.Plan.OperationID + `"}`
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/execute", strings.NewReader(body)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"phase":"committed"`) {
		t.Fatalf("unexpected maintenance execute: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("temporary source was not isolated: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(trashRoot, preview.Plan.OperationID, "payload", "tmp"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("isolated temporary file missing: err=%v entries=%v", err, entries)
	}
}

func TestMaintenancePreviewRemainsReadOnlyForDefaultDaemon(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "stale.tmp")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	NewServer(root).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/preview", strings.NewReader(`{"paths":["tmp/stale.tmp"]}`)))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"read_only":true`) || !(strings.Contains(recorder.Body.String(), "只允许生成计划") || runtime.GOOS == "windows" && strings.Contains(recorder.Body.String(), "文件状态不是 stable")) {
		t.Fatalf("maintenance preview unexpectedly writable: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("read-only preview changed source: %v", err)
	}
}

func TestOperationPreviewFailsClosedForGenericReadonlyAdapter(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"preview-thread\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil))
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Body.String())
	}
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("unexpected conversation list: %+v", list)
	}
	body := strings.NewReader(`{"conversation_id":"` + list.Items[0].ID + `","archive":true,"archive_local":true,"trash":true}`)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/preview", body))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "blocked") || !strings.Contains(recorder.Body.String(), "read_only") {
		t.Fatalf("unexpected preview response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestOperationExecuteIsDisabledByDefault(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir()).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/execute", strings.NewReader(`{"conversation_id":"x"}`)))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"read_only":true`) {
		t.Fatalf("unexpected default execute response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestOperationPreviewUsesExplicitFixtureDescriptor(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"preview-fixture\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := &LifecycleOptions{Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor()}
	handler := NewServerWithOptions(root, nil, options).Handler()
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil))
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("unexpected fixture list: body=%s err=%v", listRecorder.Body.String(), err)
	}
	body := strings.NewReader(`{"conversation_id":"` + list.Items[0].ID + `","archive":true,"archive_local":true}`)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/preview", body))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) || !strings.Contains(recorder.Body.String(), `"adapter_id":"synthetic-fixture-v1"`) || !strings.Contains(recorder.Body.String(), `"asset_graph"`) {
		t.Fatalf("unexpected fixture preview response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBatchOperationPreviewDeduplicatesIDsAndReportsPerItemBlock(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"batch-one\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"batch-two\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := &LifecycleOptions{Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor()}
	handler := NewServerWithOptions(root, nil, options).Handler()
	ids := apiConversationIDs(t, handler)
	if len(ids) != 2 {
		t.Fatalf("unexpected fixture conversation IDs: %v", ids)
	}
	body := `{"conversation_ids":["` + ids[0] + `","` + ids[0] + `","missing-batch"],"archive":true,"archive_local":true}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/batch/preview", strings.NewReader(body)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("unexpected batch preview status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Blocked bool `json:"blocked"`
		Summary struct {
			Requested int `json:"requested_conversations"`
			Count     int `json:"conversations"`
			Prepared  int `json:"prepared"`
			Blocked   int `json:"blocked"`
		} `json:"summary"`
		Items []struct {
			ConversationID string `json:"conversation_id"`
			Blocked        bool   `json:"blocked"`
			Plan           *struct {
				Status string `json:"status"`
			} `json:"plan,omitempty"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Blocked || response.Summary.Requested != 3 || response.Summary.Count != 2 || response.Summary.Prepared != 1 || response.Summary.Blocked != 1 || len(response.Items) != 2 {
		t.Fatalf("batch preview did not deduplicate or summarize correctly: %+v body=%s", response, recorder.Body.String())
	}
	if response.Items[0].ConversationID != ids[0] || response.Items[0].Plan == nil || response.Items[0].Plan.Status != "prepared" || !response.Items[1].Blocked {
		t.Fatalf("batch preview item detail missing: %+v", response.Items)
	}
	if _, err := os.Stat(filepath.Join(root, "one.jsonl")); err != nil {
		t.Fatalf("batch preview changed source: %v", err)
	}
}

func TestBatchOperationExecuteFixtureDeletesEachItemWithIndependentJournals(t *testing.T) {
	root := t.TempDir()
	paths := []string{filepath.Join(root, "one.jsonl"), filepath.Join(root, "two.jsonl")}
	for index, path := range paths {
		id := "batch-execute-" + strconv.Itoa(index+1)
		if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\""+id+"\"}}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	journalRoot := filepath.Join(t.TempDir(), "journal")
	options := &LifecycleOptions{Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(), JournalRoot: journalRoot}
	handler := NewServerWithOptions(root, nil, options).Handler()
	ids := apiConversationIDs(t, handler)
	if len(ids) != 2 {
		t.Fatalf("unexpected fixture conversation IDs: %v", ids)
	}
	body := `{"conversation_ids":["` + ids[0] + `","` + ids[1] + `"],"permanent_delete":true,"confirmation_code":"4821"}`
	recorder := httptest.NewRecorder()
	recorder = executeReviewedAPI(t, handler, "/api/v1/operations/batch/execute", body)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) || !strings.Contains(recorder.Body.String(), `"read_only":false`) {
		t.Fatalf("unexpected batch execute response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("batch execute did not delete %s: %v", path, err)
		}
	}
	entries, err := lifecycleJournalEntries(journalRoot)
	if err != nil || len(entries) != 2 {
		t.Fatalf("batch execute should leave one journal per item: entries=%v err=%v", entries, err)
	}
}

func TestOperationRecoveryListsAndRestoresPendingJournal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := []byte("pending recovery")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	journals := filepath.Join(t.TempDir(), "journal")
	trashRoot := filepath.Join(t.TempDir(), "trash")
	plan := lifecycle.OperationPlan{
		OperationID: "op-recovery-test", Root: root, ConversationID: "recovery-conversation", Status: lifecycle.PlanPrepared,
		Assets: []lifecycle.AssetFingerprint{{RelativePath: "rollout.jsonl", SizeBytes: int64(len(content)), SHA256: strings.Repeat("a", 64)}},
	}
	_, journalPath, err := lifecycle.CreateJournal(context.Background(), journals, plan)
	if err != nil {
		t.Fatal(err)
	}
	trash, err := platform.NewDirectoryTrash(trashRoot)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := trash.Move(context.Background(), plan.OperationID, root, "rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.AppendReceipt(context.Background(), journalPath, lifecycle.ReceiptRecord{RelativePath: receipt.RelativePath, OriginalPath: path, IsolatedPath: receipt.TrashPath, Provider: "directory"}); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.TransitionJournal(context.Background(), journalPath, lifecycle.PhaseNeedsReview, "TEST_PENDING", "等待恢复", "测试 journal"); err != nil {
		t.Fatal(err)
	}

	handler := NewServerWithOptions(root, nil, &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(), JournalRoot: journals, TrashRoot: trashRoot,
	}).Handler()
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/operations/recovery", nil))
	if listRecorder.Code != http.StatusOK || !strings.Contains(listRecorder.Body.String(), `"operation_id":"op-recovery-test"`) || !strings.Contains(listRecorder.Body.String(), `"phase":"needs_review"`) || !strings.Contains(listRecorder.Body.String(), `"disposition":"rollbackable"`) || !strings.Contains(listRecorder.Body.String(), `"can_rollback":true`) {
		t.Fatalf("pending journal was not listed: status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}

	recoverRecorder := httptest.NewRecorder()
	handler.ServeHTTP(recoverRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/recover", strings.NewReader(`{"operation_id":"op-recovery-test"}`)))
	if recoverRecorder.Code != http.StatusOK || !strings.Contains(recoverRecorder.Body.String(), `"phase":"rolled_back"`) {
		t.Fatalf("pending journal was not recovered: status=%d body=%s", recoverRecorder.Code, recoverRecorder.Body.String())
	}
	restored, err := os.ReadFile(path)
	if err != nil || string(restored) != string(content) {
		t.Fatalf("recovery did not restore source: content=%q err=%v", restored, err)
	}
	listRecorder = httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/operations/recovery", nil))
	if listRecorder.Code != http.StatusOK || strings.Contains(listRecorder.Body.String(), `"operation_id":"op-recovery-test"`) {
		t.Fatalf("rolled back journal should leave pending list: status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}

	traversalRecorder := httptest.NewRecorder()
	handler.ServeHTTP(traversalRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/recover", strings.NewReader(`{"operation_id":"../escape"}`)))
	if traversalRecorder.Code != http.StatusBadRequest {
		t.Fatalf("recovery accepted path traversal operation ID: status=%d body=%s", traversalRecorder.Code, traversalRecorder.Body.String())
	}
}

func TestOperationPreviewFailsClosedWhenActivityProbeFindsOpenProcess(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"activity-block\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	activityOptions := &activity.Options{
		Interval:        time.Millisecond,
		Conservative:    false,
		ProcessDetector: apiFixedProcessDetector{},
	}
	options := &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(),
		ActivityOptions: activityOptions,
	}
	handler := NewServerWithOptions(root, nil, options).Handler()
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil))
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("unexpected activity fixture list: body=%s err=%v", listRecorder.Body.String(), err)
	}
	body := strings.NewReader(`{"conversation_id":"` + list.Items[0].ID + `","archive":true,"archive_local":true}`)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/preview", body))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"blocked":true`) || !strings.Contains(recorder.Body.String(), `"active_writing"`) {
		t.Fatalf("activity probe did not block preview: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestOperationExecuteRejectsNonFixtureLifecycleMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"non-fixture\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := &LifecycleOptions{Enabled: true, Descriptor: compat.SyntheticFixtureDescriptor(), JournalRoot: filepath.Join(t.TempDir(), "journal")}
	recorder := httptest.NewRecorder()
	NewServerWithOptions(root, nil, options).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/operations/execute", strings.NewReader(`{"conversation_id":"non-fixture","permanent_delete":true,"confirmation_code":"4821"}`)))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "合成 fixture") {
		t.Fatalf("unexpected non-fixture execute response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("non-fixture rejection changed source: %v", err)
	}
}

func TestOperationExecuteRejectsLifecycleDestinationInsideSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"nested-destination\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(),
		JournalRoot: filepath.Join(root, "journal"),
	}
	recorder := httptest.NewRecorder()
	handler := NewServerWithOptions(root, nil, options).Handler()
	recorder = executeReviewedAPI(t, handler, "/api/v1/operations/execute", `{"conversation_id":"`+apiConversationIDs(t, handler)[0]+`","permanent_delete":true}`)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "journal 目录不能位于源根内") {
		t.Fatalf("unexpected nested destination response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("nested destination rejection changed source: %v", err)
	}
}

func TestOperationExecuteFixturePermanentDeleteRequiresExplicitMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"execute-fixture\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(),
		ArchiveRoot: filepath.Join(t.TempDir(), "archives"), JournalRoot: filepath.Join(t.TempDir(), "journal"),
	}
	handler := NewServerWithOptions(root, nil, options).Handler()
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil))
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("unexpected fixture list: body=%s err=%v", listRecorder.Body.String(), err)
	}
	body := `{"conversation_id":"` + list.Items[0].ID + `","permanent_delete":true,"confirmation_code":"4821"}`
	recorder := httptest.NewRecorder()
	recorder = executeReviewedAPI(t, handler, "/api/v1/operations/execute", body)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"read_only":false`) {
		t.Fatalf("unexpected fixture execute response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fixture source was not deleted after explicit operation: %v", err)
	}
}

func TestLocalArchiveRestoreUsesPreviewAndExecutionJournal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := []byte("api archive restore\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.BuildPlan(t.Context(), root, domain.ConversationSummary{ID: "api-archive-restore", Status: domain.StatusStable}, []domain.FileInventory{{Asset: domain.Asset{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}, compat.SyntheticFixtureDescriptor(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	created, err := archive.CreateLocal(t.Context(), plan, archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	options := &LifecycleOptions{Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(), ArchiveRoot: archiveRoot, JournalRoot: filepath.Join(t.TempDir(), "journal")}
	handler := NewServerWithOptions(root, nil, options).Handler()
	previewRecorder := httptest.NewRecorder()
	previewBody := `{"archive_path":` + strconv.Quote(created.Directory) + `,"target_root":` + strconv.Quote(target) + `}`
	handler.ServeHTTP(previewRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/local/restore/preview", strings.NewReader(previewBody)))
	if previewRecorder.Code != http.StatusOK || !strings.Contains(previewRecorder.Body.String(), `"blocked":false`) {
		t.Fatalf("unexpected local restore preview: status=%d body=%s", previewRecorder.Code, previewRecorder.Body.String())
	}
	var preview struct {
		RestoreOperationID string `json:"restore_operation_id"`
	}
	if err := json.Unmarshal(previewRecorder.Body.Bytes(), &preview); err != nil || preview.RestoreOperationID == "" {
		t.Fatalf("restore preview operation ID missing: err=%v body=%s", err, previewRecorder.Body.String())
	}
	executeRecorder := httptest.NewRecorder()
	executeBody := `{"restore_operation_id":"` + preview.RestoreOperationID + `"}`
	handler.ServeHTTP(executeRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/local/restore/execute", strings.NewReader(executeBody)))
	if executeRecorder.Code != http.StatusOK || !strings.Contains(executeRecorder.Body.String(), `"phase":"committed"`) {
		t.Fatalf("unexpected local restore execution: status=%d body=%s", executeRecorder.Code, executeRecorder.Body.String())
	}
	if restored, err := os.ReadFile(filepath.Join(target, "rollout.jsonl")); err != nil || string(restored) != string(content) {
		t.Fatalf("local API restore content mismatch: %q err=%v", restored, err)
	}
	recoveryRecorder := httptest.NewRecorder()
	handler.ServeHTTP(recoveryRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/archives/local/restore/recovery", nil))
	if recoveryRecorder.Code != http.StatusOK || strings.Contains(recoveryRecorder.Body.String(), preview.RestoreOperationID) {
		t.Fatalf("committed restore journal remained pending: status=%d body=%s", recoveryRecorder.Code, recoveryRecorder.Body.String())
	}
}

func TestOperationExecuteCleansCommittedCatalogDerivatives(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"catalog-delete\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"提交后清理派生数据\"}]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	options := &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(),
		ArchiveRoot: filepath.Join(t.TempDir(), "archives"), JournalRoot: filepath.Join(t.TempDir(), "journal"),
	}
	handler := NewServerWithOptions(root, archive, options).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/catalog/sync", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("catalog sync failed: %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil))
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("unexpected catalog conversation list: status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
	}
	body := `{"conversation_id":"` + list.Items[0].ID + `","permanent_delete":true,"confirmation_code":"4821"}`
	recorder = httptest.NewRecorder()
	recorder = executeReviewedAPI(t, handler, "/api/v1/operations/execute", body)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) {
		t.Fatalf("unexpected catalog cleanup execute response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source was not deleted: %v", err)
	}
	assetCount, err := archive.AssetCount(t.Context())
	if err != nil || assetCount != 0 {
		t.Fatalf("catalog asset remained after committed deletion: count=%d err=%v", assetCount, err)
	}
	hits, err := archive.Search(t.Context(), root, "提交后清理派生数据", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("catalog search projection remained: hits=%+v err=%v", hits, err)
	}
}

func TestOperationExecuteBlocksControlPlaneDeletionWithoutAdapter(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(rolloutPath, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"api-control-plane\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), []byte("{\"id\":\"api-control-plane\",\"title\":\"API 控制面\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`
		CREATE TABLE threads(id TEXT PRIMARY KEY, title TEXT, rollout_path TEXT);
		CREATE TABLE thread_items(id TEXT PRIMARY KEY, thread_id TEXT, body TEXT);
		INSERT INTO threads VALUES('api-control-plane','API 控制面','rollout.jsonl');
		INSERT INTO thread_items VALUES('api-item','api-control-plane','正文');
	`)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	journalRoot := filepath.Join(t.TempDir(), "journal")
	options := &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(), JournalRoot: journalRoot,
	}
	handler := NewServerWithOptions(root, nil, options).Handler()
	ids := apiConversationIDs(t, handler)
	if len(ids) != 1 {
		t.Fatalf("unexpected control-plane conversation list: %v", ids)
	}
	recorder := httptest.NewRecorder()
	body := `{"conversation_id":"` + ids[0] + `","permanent_delete":true,"confirmation_code":"4821"}`
	recorder = executeReviewedAPI(t, handler, "/api/v1/operations/execute", body)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "mutation adapter") {
		t.Fatalf("control-plane adapter gate was not exposed by API: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, path := range []string{rolloutPath, filepath.Join(root, "session_index.jsonl"), databasePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("API adapter gate changed source asset %s: %v", path, err)
		}
	}
	assertAPIControlPlaneRowsRemain(t, databasePath)
	entries, err := lifecycleJournalEntries(journalRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("adapter gate should leave one review journal: entries=%v err=%v", entries, err)
	}
	journal, err := lifecycle.LoadJournal(t.Context(), filepath.Join(journalRoot, entries[0].Name()))
	if err != nil || journal.Phase != lifecycle.PhaseNeedsReview || journal.ErrorCode != "CONTROL_PLANE_ADAPTER_MISSING" {
		t.Fatalf("unexpected API adapter gate journal: %+v err=%v", journal, err)
	}
}

func assertAPIControlPlaneRowsRemain(t *testing.T, databasePath string) {
	t.Helper()
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var threads, items int
	if err := database.QueryRow("SELECT COUNT(*) FROM threads WHERE id = 'api-control-plane'").Scan(&threads); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM thread_items WHERE thread_id = 'api-control-plane'").Scan(&items); err != nil {
		t.Fatal(err)
	}
	if threads != 1 || items != 1 {
		t.Fatalf("control-plane rows changed after adapter gate: threads=%d items=%d", threads, items)
	}
}

func TestTrashDetectionEndpointIsReadOnly(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer("").Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/platform/trash", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "read_only") || !strings.Contains(recorder.Body.String(), "candidate_path") {
		t.Fatalf("unexpected trash detection response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestTrashSettingsEndpointPersistsAndUsesCustomPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 使用原生 Shell 回收站，不支持自定义目录")
	}
	base := t.TempDir()
	root := filepath.Join(base, "source")
	custom := filepath.Join(base, "custom-trash", "Trash", "files")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(custom, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsStore, err := platform.NewTrashSettingsStore(filepath.Join(base, "config", "trash-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(root)
	server.SetTrashSettingsStore(settingsStore)
	handler := server.Handler()

	request := httptest.NewRequest(http.MethodPost, "/api/v1/platform/trash/settings", strings.NewReader(`{"custom_path":`+strconv.Quote(custom)+`}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"configured":true`) || !strings.Contains(response.Body.String(), `"custom_path"`) {
		t.Fatalf("custom trash setting was not saved: status=%d body=%s", response.Code, response.Body.String())
	}
	provider, err := server.systemTrashProvider(context.Background(), root)
	filesystemProvider, ok := provider.(*platform.FilesystemTrash)
	if err != nil || !ok || filesystemProvider.Root != custom {
		t.Fatalf("custom provider does not use saved path: provider=%T value=%+v err=%v", provider, filesystemProvider, err)
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, httptest.NewRequest(http.MethodPost, "/api/v1/platform/trash/settings", strings.NewReader(`{"custom_path":`+strconv.Quote(filepath.Join(root, "Trash", "files"))+`}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("overlapping source path should be rejected: status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	reset := httptest.NewRecorder()
	handler.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/api/v1/platform/trash/settings", strings.NewReader(`{"custom_path":""}`)))
	if reset.Code != http.StatusOK || strings.Contains(reset.Body.String(), `"configured":true`) {
		t.Fatalf("custom trash setting was not cleared: status=%d body=%s", reset.Code, reset.Body.String())
	}
}

func TestGitHubAuthEndpointsKeepTokensOutOfResponses(t *testing.T) {
	oauthServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/login/device/code":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"device_code": "device-secret", "user_code": "ABCD-EFGH", "verification_uri": "https://github.com/login/device", "expires_in": 10, "interval": 1,
			})
		case "/login/oauth/access_token":
			_ = json.NewEncoder(writer).Encode(map[string]string{"access_token": "ghs-runtime-secret", "token_type": "bearer"})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer oauthServer.Close()
	auth, err := archive.NewGitHubDeviceAuthService(archive.GitHubDeviceAuthConfig{BaseURL: oauthServer.URL, ClientID: "client"}, archive.NewMemoryGitHubTokenStore(), "personal")
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	handler := NewServerWithOptions("", nil, &LifecycleOptions{GitHubAuth: auth}).Handler()

	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/v1/github/auth/start", nil))
	if start.Code != http.StatusAccepted || strings.Contains(start.Body.String(), "device-secret") || strings.Contains(start.Body.String(), "ghs-runtime-secret") {
		t.Fatalf("unexpected or secret-bearing start response: status=%d body=%s", start.Code, start.Body.String())
	}
	var started struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil || started.Session.ID == "" {
		t.Fatalf("invalid start response: %s err=%v", start.Body.String(), err)
	}
	restored := githubSettingsRequest(t, handler, http.MethodGet, "/api/v1/github/auth/status", nil, http.StatusOK)
	if !strings.Contains(restored.Body.String(), started.Session.ID) {
		t.Fatal("刷新页面时必须能够找回最近的授权会话")
	}
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/v1/github/auth/status?session_id="+url.QueryEscape(started.Session.ID), nil))
	if status.Code != http.StatusOK || strings.Contains(status.Body.String(), "ghs-runtime-secret") || !strings.Contains(status.Body.String(), "configured") {
		t.Fatalf("unexpected status response: status=%d body=%s", status.Code, status.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(status.Body.String(), `"status":"authorized"`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		status = httptest.NewRecorder()
		handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/v1/github/auth/status?session_id="+url.QueryEscape(started.Session.ID), nil))
	}
	if !strings.Contains(status.Body.String(), `"status":"authorized"`) {
		t.Fatalf("device session did not authorize through API: %s", status.Body.String())
	}
	restored = githubSettingsRequest(t, handler, http.MethodGet, "/api/v1/github/auth/status", nil, http.StatusOK)
	if !strings.Contains(restored.Body.String(), `"status":"authorized"`) || !strings.Contains(restored.Body.String(), `"credential_available":true`) {
		t.Fatal("刷新页面应恢复已完成的授权和凭证状态")
	}
	revoke := httptest.NewRecorder()
	handler.ServeHTTP(revoke, httptest.NewRequest(http.MethodPost, "/api/v1/github/auth/revoke", nil))
	if revoke.Code != http.StatusOK || !strings.Contains(revoke.Body.String(), "credential_revoked") {
		t.Fatalf("unexpected revoke response: status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	restored = githubSettingsRequest(t, handler, http.MethodGet, "/api/v1/github/auth/status", nil, http.StatusOK)
	if strings.Contains(restored.Body.String(), `"status":"authorized"`) || !strings.Contains(restored.Body.String(), `"credential_available":false`) {
		t.Fatal("解除绑定后不得恢复旧的授权成功状态")
	}
}

func TestGitHubAuthEndpointsReportUnconfiguredService(t *testing.T) {
	handler := NewServer("").Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/github/auth/status", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"configured":false`) {
		t.Fatalf("unexpected unconfigured GitHub status: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/auth/start", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected unconfigured GitHub start status: %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/repositories", strings.NewReader(`{"name":"archive"}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected unconfigured repository create status: %d", recorder.Code)
	}
}

type apiRemoteArchiveCatalog struct{}

func (apiRemoteArchiveCatalog) ListArchives(context.Context) ([]archive.GitHubArchiveSummary, error) {
	return []archive.GitHubArchiveSummary{{ArchiveID: "remote-001", ManifestPath: "archives/remote-001/remote-manifest.json", FileCount: 2, ChunkCount: 3}}, nil
}

func (apiRemoteArchiveCatalog) VerifyArchive(context.Context, string) (archive.RemoteArchiveResult, error) {
	return archive.RemoteArchiveResult{ArchiveID: "remote-001", ManifestPath: "archives/remote-001/remote-manifest.json", Verified: true}, nil
}

type apiRemoteArchiveRecovery struct {
	destination string
	deleted     string
}

func (*apiRemoteArchiveRecovery) UploadDirectory(context.Context, string, string) (archive.RemoteArchiveResult, error) {
	return archive.RemoteArchiveResult{}, nil
}

func (r *apiRemoteArchiveRecovery) ListArchives(context.Context) ([]archive.GitHubArchiveSummary, error) {
	return nil, nil
}

func (r *apiRemoteArchiveRecovery) VerifyArchive(context.Context, string) (archive.RemoteArchiveResult, error) {
	return archive.RemoteArchiveResult{ArchiveID: "remote-001", ManifestPath: "archives/remote-001/remote-manifest.json", FileCount: 2, ChunkCount: 3, Verified: true}, nil
}

func (r *apiRemoteArchiveRecovery) DownloadArchive(_ context.Context, archiveID, destination string) (archive.RemoteArchiveResult, error) {
	r.destination = filepath.Join(destination, archiveID)
	return archive.RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: "archives/" + archiveID + "/remote-manifest.json", FileCount: 2, ChunkCount: 3, Verified: true}, nil
}

func (r *apiRemoteArchiveRecovery) DeleteArchive(_ context.Context, archiveID string) error {
	r.deleted = archiveID
	return nil
}

type apiRemoteRestoreProvider struct {
	sourceArchive string
}

func (*apiRemoteRestoreProvider) UploadDirectory(context.Context, string, string) (archive.RemoteArchiveResult, error) {
	return archive.RemoteArchiveResult{}, nil
}

func (*apiRemoteRestoreProvider) ListArchives(context.Context) ([]archive.GitHubArchiveSummary, error) {
	return []archive.GitHubArchiveSummary{{ArchiveID: "remote-restore-001", FileCount: 2, ChunkCount: 2}}, nil
}

func (*apiRemoteRestoreProvider) VerifyArchive(context.Context, string) (archive.RemoteArchiveResult, error) {
	return archive.RemoteArchiveResult{ArchiveID: "remote-restore-001", Verified: true}, nil
}

func (p *apiRemoteRestoreProvider) DownloadArchive(_ context.Context, archiveID, destination string) (archive.RemoteArchiveResult, error) {
	if archiveID != "remote-restore-001" {
		return archive.RemoteArchiveResult{}, fmt.Errorf("未知测试归档: %s", archiveID)
	}
	final := filepath.Join(destination, archiveID)
	if err := copyAPIArchiveTree(p.sourceArchive, final); err != nil {
		return archive.RemoteArchiveResult{}, err
	}
	return archive.RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: "remote-manifest.json", FileCount: 2, ChunkCount: 2, Verified: true}, nil
}

func copyAPIArchiveTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

func TestGitHubArchiveCatalogEndpointsAreReadOnly(t *testing.T) {
	handler := NewServerWithOptions("", nil, &LifecycleOptions{RemoteArchiveCatalog: apiRemoteArchiveCatalog{}}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/github/archives", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "remote-001") || !strings.Contains(recorder.Body.String(), `"read_only":true`) {
		t.Fatalf("unexpected remote archive list: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/github/archives/remote-001/verify", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"verified":true`) {
		t.Fatalf("unexpected remote archive verify: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/v1/github/archives/remote-001", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote archive delete must not be exposed by read-only catalog: %d", recorder.Code)
	}
}

type apiWebDAVArchiveCatalog struct{ apiRemoteArchiveCatalog }

func (apiWebDAVArchiveCatalog) ProviderInfo() archive.RemoteArchiveProviderInfo {
	return archive.RemoteArchiveProviderInfo{
		ID: "webdav", Name: "WebDAV", TargetRequired: false,
		SupportsListing: true, SupportsRecovery: true, SupportsDelete: true,
	}
}

func TestGenericRemoteArchiveEndpointExposesProviderIdentity(t *testing.T) {
	handler := NewServerWithOptions("", nil, &LifecycleOptions{RemoteArchiveCatalog: apiWebDAVArchiveCatalog{}}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"webdav"`) || !strings.Contains(recorder.Body.String(), `"remote_archive_provider_ready":true`) {
		t.Fatalf("capabilities response lost provider identity: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/archives", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"webdav"`) || !strings.Contains(recorder.Body.String(), `"target_required":false`) {
		t.Fatalf("generic remote archive response lost provider identity: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/archives/remote-001/verify", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"verified":true`) {
		t.Fatalf("generic remote archive verify failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGitHubArchiveDownloadRequiresPreviewAndConfiguredRecoveryRoot(t *testing.T) {
	recoveryRoot := t.TempDir()
	recovery := &apiRemoteArchiveRecovery{}
	handler := NewServerWithOptions("", nil, &LifecycleOptions{RemoteArchive: recovery, RemoteRecoveryRoot: recoveryRoot}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/archives/remote-001/download/preview", strings.NewReader(`{"destination":`+strconv.Quote(filepath.Join(recoveryRoot, "downloads"))+`}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"operation_id"`) || !strings.Contains(recorder.Body.String(), "remote-001") {
		t.Fatalf("unexpected download preview: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil || preview.OperationID == "" {
		t.Fatalf("download preview operation ID missing: %s err=%v", recorder.Body.String(), err)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/archives/remote-001/download/execute", strings.NewReader(`{"operation_id":"`+preview.OperationID+`"}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) || recovery.destination == "" {
		t.Fatalf("unexpected download execute: status=%d body=%s destination=%s", recorder.Code, recorder.Body.String(), recovery.destination)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/archives/remote-001/download/preview", strings.NewReader(`{"destination":`+strconv.Quote(filepath.Join(t.TempDir(), "outside"))+`}`)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("outside recovery root was accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRemoteArchiveRestoreReusesLocalRestoreCore(t *testing.T) {
	sourceRoot := t.TempDir()
	payload := []byte("remote restore fixture\n")
	payloadPath := filepath.Join(sourceRoot, "rollout.jsonl")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	plan := lifecycle.OperationPlan{
		OperationID: "remote-source-operation", Root: sourceRoot, ConversationID: "remote-conversation", Status: lifecycle.PlanPrepared,
		Assets: []lifecycle.AssetFingerprint{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}},
	}
	created, err := archive.CreateLocal(context.Background(), plan, filepath.Join(t.TempDir(), "source-archives"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &apiRemoteRestoreProvider{sourceArchive: created.Directory}
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	journalRoot := filepath.Join(t.TempDir(), "journals")
	targetRoot := filepath.Join(t.TempDir(), "restored")
	handler := NewServerWithOptions(sourceRoot, nil, &LifecycleOptions{
		Enabled: true, FixtureOnly: true, ArchiveRoot: archiveRoot, JournalRoot: journalRoot,
		RemoteArchive: provider, RemoteArchiveCatalog: provider,
	}).Handler()

	body := `{"target_root":` + strconv.Quote(targetRoot) + `}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/remote-restore-001/restore/preview", strings.NewReader(body)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) || !strings.Contains(recorder.Body.String(), `"source_read_only":true`) || !strings.Contains(recorder.Body.String(), `"derived_write":true`) || !strings.Contains(recorder.Body.String(), "archive_path") {
		t.Fatalf("远端恢复预览失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		RestoreOperationID string `json:"restore_operation_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil || preview.RestoreOperationID == "" {
		t.Fatalf("远端恢复预览缺少操作 ID: err=%v body=%s", err, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	executeBody := `{"operation_id":"` + preview.RestoreOperationID + `"}`
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/remote-restore-001/restore/execute", strings.NewReader(executeBody)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) {
		t.Fatalf("远端恢复执行失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	restored, err := os.ReadFile(filepath.Join(targetRoot, "rollout.jsonl"))
	if err != nil || string(restored) != string(payload) {
		t.Fatalf("远端归档未通过本地恢复核心发布: %q err=%v", restored, err)
	}

	recorder = httptest.NewRecorder()
	blockedBody := `{"target_root":` + strconv.Quote(sourceRoot) + `}`
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/remote-restore-001/restore/preview", strings.NewReader(blockedBody)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("与当前扫描根重叠的远端恢复目标未被拒绝: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRemoteArchiveRestoreEncryptedFailuresCleanStaging(t *testing.T) {
	sourceRoot := t.TempDir()
	payload := []byte("encrypted remote restore fixture\n")
	payloadPath := filepath.Join(sourceRoot, "rollout.jsonl")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	plan := lifecycle.OperationPlan{
		OperationID: "remote-encrypted-source-operation", Root: sourceRoot, ConversationID: "remote-encrypted-conversation", Status: lifecycle.PlanPrepared,
		Assets: []lifecycle.AssetFingerprint{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}},
	}
	sourceArchiveRoot := t.TempDir()
	created, err := archive.CreateLocalEncrypted(context.Background(), plan, sourceArchiveRoot, "正确密码")
	if err != nil {
		t.Fatal(err)
	}
	provider := &apiRemoteRestoreProvider{sourceArchive: created.Directory}
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	journalRoot := filepath.Join(t.TempDir(), "journals")
	options := &LifecycleOptions{
		Enabled: true, FixtureOnly: true, ArchiveRoot: archiveRoot, JournalRoot: journalRoot,
		RemoteArchive: provider, RemoteArchiveCatalog: provider,
	}
	server := NewServerWithOptions(sourceRoot, nil, options)
	handler := server.Handler()
	targetRoot := filepath.Join(t.TempDir(), "wrong-password-target")
	body := `{"target_root":` + strconv.Quote(targetRoot) + `,"archive_password":"错误密码"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/remote-restore-001/restore/preview", strings.NewReader(body)))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "校验归档失败") {
		t.Fatalf("错误密码未被阻断: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertRemoteRestoreStagingEmpty(t, archiveRoot)

	tamperedPath := filepath.Join(created.Directory, "payload", "rollout.jsonl")
	if err := os.WriteFile(tamperedPath, []byte("tampered remote archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body = `{"target_root":` + strconv.Quote(filepath.Join(t.TempDir(), "tampered-target")) + `,"archive_password":"正确密码"}`
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/remote-restore-001/restore/preview", strings.NewReader(body)))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "校验归档失败") {
		t.Fatalf("篡改归档未被阻断: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertRemoteRestoreStagingEmpty(t, archiveRoot)

	// 使用未篡改副本确认失败清理不会破坏后续正确恢复。
	created, err = archive.CreateLocalEncrypted(context.Background(), plan, t.TempDir(), "正确密码")
	if err != nil {
		t.Fatal(err)
	}
	provider.sourceArchive = created.Directory
	targetRoot = filepath.Join(t.TempDir(), "expired-target")
	body = `{"target_root":` + strconv.Quote(targetRoot) + `,"archive_password":"正确密码"}`
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/remote-restore-001/restore/preview", strings.NewReader(body)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) {
		t.Fatalf("正确密码预览失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		RestoreOperationID string `json:"restore_operation_id"`
		ArchivePath        string `json:"archive_path"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil || preview.RestoreOperationID == "" {
		t.Fatalf("加密远端恢复预览缺少操作 ID: err=%v body=%s", err, recorder.Body.String())
	}
	server.remoteRestoreMu.Lock()
	server.remoteRestores[preview.RestoreOperationID].ExpiresAt = time.Now().Add(-time.Second)
	server.remoteRestoreMu.Unlock()
	targetRoot = filepath.Join(t.TempDir(), "restored")
	body = `{"target_root":` + strconv.Quote(targetRoot) + `,"archive_password":"正确密码"}`
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/remote-restore-001/restore/preview", strings.NewReader(body)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) {
		t.Fatalf("过期计划后的正确密码预览失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var refreshedPreview struct {
		RestoreOperationID string `json:"restore_operation_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &refreshedPreview); err != nil || refreshedPreview.RestoreOperationID == "" {
		t.Fatalf("续建预览缺少操作 ID: err=%v body=%s", err, recorder.Body.String())
	}
	if _, err := os.Stat(preview.ArchivePath); !os.IsNotExist(err) {
		t.Fatalf("过期远端恢复 staging 未清理: path=%s err=%v", preview.ArchivePath, err)
	}
	recorder = httptest.NewRecorder()
	executeBody := `{"operation_id":"` + refreshedPreview.RestoreOperationID + `","archive_password":"正确密码"}`
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/archives/remote-restore-001/restore/execute", strings.NewReader(executeBody)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) {
		t.Fatalf("加密远端恢复执行失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if restored, err := os.ReadFile(filepath.Join(targetRoot, "rollout.jsonl")); err != nil || string(restored) != string(payload) {
		t.Fatalf("加密远端恢复内容不正确: %q err=%v", restored, err)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, ".remote-restore-staging")); err != nil {
		t.Fatalf("成功恢复所需 staging 不应在执行后立即丢失: %v", err)
	}
}

func assertRemoteRestoreStagingEmpty(t *testing.T, archiveRoot string) {
	t.Helper()
	root := filepath.Join(archiveRoot, ".remote-restore-staging")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("读取远端恢复 staging 失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("失败的远端恢复留下 staging: %v", entries)
	}
}

func TestGitHubArchiveDeleteRequiresPreviewAndExactConfirmation(t *testing.T) {
	recovery := &apiRemoteArchiveRecovery{}
	handler := NewServerWithOptions("", nil, &LifecycleOptions{RemoteArchive: recovery}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/archives/remote-001/delete/preview", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"confirmation_required":"remote-001"`) {
		t.Fatalf("unexpected delete preview: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil || preview.OperationID == "" {
		t.Fatalf("delete preview operation ID missing: %s err=%v", recorder.Body.String(), err)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/archives/remote-001/delete/execute", strings.NewReader(`{"operation_id":"`+preview.OperationID+`","confirm_archive_id":"wrong"}`)))
	if recorder.Code != http.StatusConflict || recovery.deleted != "" {
		t.Fatalf("wrong delete confirmation was accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/archives/remote-001/delete/execute", strings.NewReader(`{"operation_id":"`+preview.OperationID+`","confirm_archive_id":"remote-001"}`)))
	if recorder.Code != http.StatusOK || recovery.deleted != "remote-001" || !strings.Contains(recorder.Body.String(), `"deleted":true`) {
		t.Fatalf("expected confirmed delete: status=%d body=%s deleted=%s", recorder.Code, recorder.Body.String(), recovery.deleted)
	}
}

type apiGitHubRepositoryCatalog struct{}

func (apiGitHubRepositoryCatalog) ListRepositories(context.Context) ([]archive.GitHubRepositorySummary, error) {
	return []archive.GitHubRepositorySummary{{Owner: "owner", Name: "chronicle-archive", FullName: "owner/chronicle-archive", Private: true}}, nil
}

func (apiGitHubRepositoryCatalog) CreatePrivateRepository(_ context.Context, name, description string) (archive.GitHubRepositorySummary, error) {
	return archive.GitHubRepositorySummary{Owner: "owner", Name: name, FullName: "owner/" + name, Description: description, Private: true}, nil
}

func TestGitHubRepositoryEndpointsListAndCreatePrivateRepository(t *testing.T) {
	handler := NewServerWithOptions("", nil, &LifecycleOptions{GitHubRepositories: apiGitHubRepositoryCatalog{}}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/github/repositories", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "chronicle-archive") || !strings.Contains(recorder.Body.String(), `"source_read_only":true`) {
		t.Fatalf("unexpected repository list: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/github/repositories", strings.NewReader(`{"name":"new-archive","description":"归档"}`)))
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), "new-archive") {
		t.Fatalf("unexpected repository create: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/v1/github/repositories", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected repository delete status: %d", recorder.Code)
	}
}

type apiGitHubArchiveSelector struct {
	target     archive.GitHubRepositorySummary
	configured bool
}

func (s *apiGitHubArchiveSelector) UploadDirectory(context.Context, string, string) (archive.RemoteArchiveResult, error) {
	return archive.RemoteArchiveResult{ArchiveID: "remote-001", Verified: true}, nil
}

func (s *apiGitHubArchiveSelector) VerifyArchive(context.Context, string) (archive.RemoteArchiveResult, error) {
	return archive.RemoteArchiveResult{ArchiveID: "remote-001", Verified: true}, nil
}

func (s *apiGitHubArchiveSelector) ListArchives(context.Context) ([]archive.GitHubArchiveSummary, error) {
	return nil, nil
}

func (s *apiGitHubArchiveSelector) SelectedArchiveRepository(context.Context) (archive.GitHubRepositorySummary, bool, error) {
	return s.target, s.configured, nil
}

func (s *apiGitHubArchiveSelector) SelectArchiveRepository(_ context.Context, owner, repository string) (archive.GitHubRepositorySummary, error) {
	s.target = archive.GitHubRepositorySummary{Owner: owner, Name: repository, FullName: owner + "/" + repository, Private: true}
	s.configured = true
	return s.target, nil
}

func (s *apiGitHubArchiveSelector) ClearArchiveRepository(context.Context) error {
	s.target = archive.GitHubRepositorySummary{}
	s.configured = false
	return nil
}

func TestGitHubArchiveTargetEndpointSelectsAndClearsLocalTarget(t *testing.T) {
	selector := &apiGitHubArchiveSelector{}
	handler := NewServerWithOptions("", nil, &LifecycleOptions{RemoteArchive: selector, RemoteArchiveCatalog: selector}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/github/archive-target", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"configured":false`) {
		t.Fatalf("unexpected initial archive target: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/github/archive-target", strings.NewReader(`{"owner":"owner","repository":"archive"}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "owner/archive") {
		t.Fatalf("unexpected archive target selection: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/github/archive-target", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"configured":true`) {
		t.Fatalf("selected archive target was not readable: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/v1/github/archive-target", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"cleared":true`) {
		t.Fatalf("unexpected archive target clear: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

type apiPublicGitHubRepositoryCatalog struct{}

func (apiPublicGitHubRepositoryCatalog) ListRepositories(context.Context) ([]archive.GitHubRepositorySummary, error) {
	return []archive.GitHubRepositorySummary{{Owner: "owner", Name: "public-archive", FullName: "owner/public-archive", Private: false}}, nil
}

func (apiPublicGitHubRepositoryCatalog) CreatePrivateRepository(context.Context, string, string) (archive.GitHubRepositorySummary, error) {
	return archive.GitHubRepositorySummary{}, nil
}

func TestGitHubArchiveTargetRejectsPublicRepositoryWhenCatalogAvailable(t *testing.T) {
	selector := &apiGitHubArchiveSelector{}
	handler := NewServerWithOptions("", nil, &LifecycleOptions{RemoteArchive: selector, GitHubRepositories: apiPublicGitHubRepositoryCatalog{}}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/github/archive-target", strings.NewReader(`{"owner":"owner","repository":"public-archive"}`)))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "私有") {
		t.Fatalf("public GitHub repository was accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestScanTaskEndpointAndSSEAreReadOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "task.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"task-api\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/tasks/scan", nil))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("unexpected task creation: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil || created.Task.ID == "" {
		t.Fatalf("unexpected task payload: %s err=%v", recorder.Body.String(), err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+created.Task.ID, nil))
		if recorder.Code != http.StatusOK {
			t.Fatal(recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), `"status":"completed"`) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+created.Task.ID+"/events", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" || !strings.Contains(recorder.Body.String(), "event: snapshot") {
		t.Fatalf("unexpected task SSE: status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestSearchUsesCatalogFTSWhenAvailable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"fts-api\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"归档检索词\"}]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	handler := NewServerWithStore(root, archive).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/catalog/sync", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("catalog sync failed: %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=归档检索词", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "rollout.jsonl") || !strings.Contains(recorder.Body.String(), `"line":2`) {
		t.Fatalf("unexpected catalog search: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
