// 中文说明：验证 cleanup registry 的 API 预览、fixture 执行、默认只读和 retention 配置门禁。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/maintenance"
)

func TestCleanupAPIPreviewExecuteAndReportRetained(t *testing.T) {
	root := t.TempDir()
	registryRoot := filepath.Join(t.TempDir(), "cleanup-registry")
	target := filepath.Join(t.TempDir(), "staging-output")
	if err := os.WriteFile(target, []byte("generated"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := maintenance.OpenRegistry(registryRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := registry.RegisterArtifact(context.Background(), maintenance.ArtifactManifest{Kind: maintenance.ArtifactStaging, TargetPath: target, Owner: "api-test", Source: "fixture", Rebuildable: true, CreatedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	options := &LifecycleOptions{Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(), JournalRoot: filepath.Join(t.TempDir(), "journal"), CleanupRoot: registryRoot, CleanupOpenFileProbe: func(context.Context, string) (bool, bool, string) { return false, true, "测试探测成功" }}
	handler := NewServerWithOptions(root, nil, options).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/cleanup", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"candidate_count":1`) {
		t.Fatalf("cleanup GET 未返回候选：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var reportResponse struct {
		Cleanup maintenance.CleanupReport `json:"cleanup"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &reportResponse); err != nil || len(reportResponse.Cleanup.Candidates) != 1 {
		t.Fatalf("cleanup 报告解析失败：err=%v body=%s", err, recorder.Body.String())
	}
	previewBody := `{"artifact_ids":["` + manifest.ArtifactID + `"]}`
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/cleanup/preview", strings.NewReader(previewBody)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"blocked":false`) {
		t.Fatalf("cleanup 预览失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		Plan maintenance.CleanupPlan `json:"plan"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil || preview.Plan.OperationID == "" || preview.Plan.Status != "prepared" {
		t.Fatalf("cleanup 计划解析失败：err=%v body=%s", err, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	executeBody := `{"operation_id":"` + preview.Plan.OperationID + `"}`
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/cleanup/execute", strings.NewReader(executeBody)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"phase":"committed"`) {
		t.Fatalf("cleanup 执行失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("cleanup 执行后目标仍存在：%v", err)
	}
	var execution struct {
		Result maintenance.CleanupResult `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &execution); err != nil || len(execution.Result.Receipts) != 1 {
		t.Fatalf("cleanup 执行回执解析失败：err=%v body=%s", err, recorder.Body.String())
	}
	if _, err := os.Stat(execution.Result.Receipts[0].QuarantinePath); err != nil {
		t.Fatalf("cleanup quarantine 不存在：%v", err)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/cleanup", nil))
	if err := json.Unmarshal(recorder.Body.Bytes(), &reportResponse); err != nil {
		t.Fatal(err)
	}
	if reportResponse.Cleanup.CandidateCount != 0 || len(reportResponse.Cleanup.Quarantined) != 1 {
		t.Fatalf("已隔离文件应保持可见且不可再次选择清理：%s", recorder.Body.String())
	}
	isolated := reportResponse.Cleanup.Quarantined[0]
	if isolated.Eligible || isolated.SizeBytes != 9 || isolated.FileCount != 1 || isolated.LastOperationID != preview.Plan.OperationID {
		t.Fatalf("隔离区应准确报告仍占用的大小和操作归属：%+v", isolated)
	}
}

func TestCleanupAPIDefaultDaemonRemainsReadOnly(t *testing.T) {
	root := t.TempDir()
	registryRoot := filepath.Join(t.TempDir(), "cleanup-registry")
	target := filepath.Join(t.TempDir(), "staging-output")
	if err := os.WriteFile(target, []byte("generated"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := maintenance.OpenRegistry(registryRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := registry.RegisterArtifact(context.Background(), maintenance.ArtifactManifest{Kind: maintenance.ArtifactStaging, TargetPath: target, Owner: "api-test", Source: "fixture", Rebuildable: true, CreatedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// 默认 daemon 没有 LifecycleOptions，因此 cleanup registry 不会开放写入。
	_ = manifest
	handler := NewServer(root).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/cleanup/preview", strings.NewReader(`{"artifact_ids":[]}`)))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "cleanup registry") {
		t.Fatalf("默认 daemon 未保持只读/未配置状态：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("只读 API 意外修改目标：%v", err)
	}
}

func TestCleanupAPIRegisterRequiresFixtureModeAndPersistsManifest(t *testing.T) {
	root := t.TempDir()
	registryRoot := filepath.Join(t.TempDir(), "cleanup-registry")
	target := filepath.Join(t.TempDir(), "generated", "staging")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("generated"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := &LifecycleOptions{Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(), JournalRoot: filepath.Join(t.TempDir(), "journal"), CleanupRoot: registryRoot, CleanupOpenFileProbe: func(context.Context, string) (bool, bool, string) { return false, true, "测试探测成功" }}
	handler := NewServerWithOptions(root, nil, options).Handler()
	body := `{"manifest":{"kind":"staging","target_path":"` + target + `","owner":"api-test","source":"copy staging","rebuildable":true}}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/cleanup/register", strings.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("fixture 登记失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Manifest maintenance.ArtifactManifest `json:"manifest"`
		Path     string                       `json:"manifest_path"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Manifest.ArtifactID == "" || response.Path == "" {
		t.Fatalf("登记响应缺少 manifest：err=%v body=%s", err, recorder.Body.String())
	}
	if _, err := os.Stat(response.Path); err != nil {
		t.Fatalf("登记 manifest 未落盘：%v", err)
	}

	readOnlyHandler := NewServer(root).Handler()
	recorder = httptest.NewRecorder()
	readOnlyHandler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/cleanup/register", strings.NewReader(body)))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "fixture registry") {
		t.Fatalf("默认 daemon 意外允许登记：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCleanupManagementEnablePersistsWithoutGrantingSourceWrite(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "state.sqlite-wal")
	if err := os.WriteFile(sentinel, []byte("unrelated-source-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	registryRoot := filepath.Join(t.TempDir(), "registry")
	options := &LifecycleOptions{CleanupRoot: registryRoot, CleanupOpenFileProbe: func(context.Context, string) (bool, bool, string) { return false, true, "测试进程探测" }}
	server := NewServerWithOptions(root, nil, options)
	defer server.Close()
	handler := server.Handler()
	response := apiJSON(t, handler, http.MethodGet, "/api/v1/cleanup/settings", "")
	var status cleanupManagementStatus
	if json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Enabled || !status.SettingsWritable || server.cleanupArtifactRegistrar() != nil {
		t.Fatalf("默认应展示真实的未启用状态：%s", response.Body.String())
	}
	if _, err := os.Stat(registryRoot); !os.IsNotExist(err) {
		t.Fatalf("读取设置不应创建登记区：%v", err)
	}
	policy := maintenance.DefaultRetentionPolicy()
	policy.StagingTTLSeconds = 2 * 86400
	body, _ := json.Marshal(map[string]any{"policy": policy})
	response = apiJSON(t, handler, http.MethodPut, "/api/v1/cleanup/retention", string(body))
	if response.Code != http.StatusForbidden {
		t.Fatalf("未启用管理不能保存规则：%s", response.Body.String())
	}
	for _, input := range []string{`{}`, `{"enabled":null}`, `{"enabled":true,"root":"/other"}`} {
		response = apiJSON(t, handler, http.MethodPut, "/api/v1/cleanup/settings", input)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("无效启用请求应拒绝：%s", response.Body.String())
		}
	}
	response = apiJSON(t, handler, http.MethodPut, "/api/v1/cleanup/settings", `{"enabled":true}`)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &status) != nil || !status.Enabled || !status.PolicyWriteEnabled || status.AutomaticCleanup {
		t.Fatalf("启用应只开放管理设置和任务登记：%s", response.Body.String())
	}
	response = apiJSON(t, handler, http.MethodPut, "/api/v1/cleanup/retention", string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("真实源只读时也应能保存项目自身的规则：%s", response.Body.String())
	}
	invalidPolicy := policy
	invalidPolicy.StagingTTLSeconds = -1
	invalidBody, _ := json.Marshal(map[string]any{"policy": invalidPolicy})
	response = apiJSON(t, handler, http.MethodPut, "/api/v1/cleanup/retention", string(invalidBody))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("负数保留期应拒绝：%s", response.Body.String())
	}
	registrar := server.cleanupArtifactRegistrar()
	if registrar == nil {
		t.Fatal("启用后新任务应获得登记回调")
	}
	target := filepath.Join(t.TempDir(), "intermediate.bin")
	if err := os.WriteFile(target, []byte("intermediate"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := registrar(context.Background(), maintenance.ArtifactManifest{Kind: maintenance.ArtifactStaging, TargetPath: target, Owner: "test", Source: "test-copy", Rebuildable: true})
	if err != nil || manifest.ExpiresAt.Sub(manifest.CreatedAt) != 48*time.Hour {
		t.Fatalf("新登记必须使用真实保存的保留期：%+v err=%v", manifest, err)
	}
	restarted := NewServerWithOptions(root, nil, options)
	defer restarted.Close()
	response = apiJSON(t, restarted.Handler(), http.MethodGet, "/api/v1/cleanup/settings", "")
	if json.Unmarshal(response.Body.Bytes(), &status) != nil || !status.Enabled {
		t.Fatalf("重启应保留管理开关：%s", response.Body.String())
	}
	for _, endpoint := range []string{"/api/v1/maintenance/execute", "/api/v1/cleanup/execute", "/api/v1/cleanup/recover", "/api/v1/cleanup/register"} {
		response = apiJSON(t, handler, http.MethodPost, endpoint, `{}`)
		if response.Code != http.StatusForbidden {
			t.Fatalf("管理开关不得扩大文件执行权限：%s %d %s", endpoint, response.Code, response.Body.String())
		}
	}
	response = apiJSON(t, handler, http.MethodGet, "/api/v1/cleanup", "")
	var report struct {
		Cleanup maintenance.CleanupReport `json:"cleanup"`
	}
	if json.Unmarshal(response.Body.Bytes(), &report) != nil || len(report.Cleanup.Retained) != 1 || len(report.Cleanup.Unknown) != 0 {
		t.Fatalf("登记项与配置应正确归类：%s", response.Body.String())
	}
	response = apiJSON(t, handler, http.MethodPut, "/api/v1/cleanup/settings", `{"enabled":false}`)
	if response.Code != http.StatusOK || server.cleanupArtifactRegistrar() != nil {
		t.Fatalf("关闭后新任务不再登记：%s", response.Body.String())
	}
	registry, err := maintenance.OpenRegistry(registryRoot)
	if err != nil || registry.Policy.StagingTTLSeconds != policy.StagingTTLSeconds {
		t.Fatalf("关闭不得丢失已保存规则：err=%v", err)
	}
	for path, want := range map[string]string{sentinel: "unrelated-source-data", target: "intermediate"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("管理开关不得移动或删除文件：%s err=%v", path, err)
		}
	}
}

func TestCleanupSettingsRejectRegistryInsideSource(t *testing.T) {
	root := t.TempDir()
	registryRoot := filepath.Join(root, "runtime", "registry")
	server := NewServerWithOptions(root, nil, &LifecycleOptions{CleanupRoot: registryRoot})
	defer server.Close()
	response := apiJSON(t, server.Handler(), http.MethodPut, "/api/v1/cleanup/settings", `{"enabled":true}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "源目录之外") {
		t.Fatalf("设置写入不能进入工作区源目录：%s", response.Body.String())
	}
	if _, err := os.Stat(registryRoot); !os.IsNotExist(err) {
		t.Fatalf("被拒绝的路径不应创建：%v", err)
	}
}
