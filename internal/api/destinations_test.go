// 中文说明：只在临时隔离目录测试探针、用户确认、路径变化失效及服务器门禁，不移动任何真实对话。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"codex-chronicle.local/internal/platform"
)

func destinationTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "source")
	trash := filepath.Join(base, "Trash", "files")
	for _, path := range []string{root, trash} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "untouched.jsonl"), []byte("{\"keep\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := platform.NewTrashSettingsStore(filepath.Join(base, "settings", "trash.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(context.Background(), platform.TrashSettings{CustomPath: trash}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(root)
	server.SetTrashSettingsStore(settings)
	server.lifecycle = &LifecycleOptions{Enabled: true, FixtureOnly: true, UseSystemTrash: true, RequireDestinationConfirmation: true, ArchiveRoot: filepath.Join(base, "archives"), JournalRoot: filepath.Join(base, "journals")}
	return server, root
}

func destinationRequest(t *testing.T, s *Server, path string, input any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	writer := httptest.NewRecorder()
	s.Handler().ServeHTTP(writer, request)
	return writer
}

func TestTrashProbeRequiresHumanConfirmationAndDoesNotTouchSource(t *testing.T) {
	server, root := destinationTestServer(t)
	status, err := server.operationDestinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.requireOperationDestinations(context.Background(), operationPreviewRequest{Trash: true}); err == nil {
		t.Fatal("未测试不得执行")
	}
	writer := destinationRequest(t, server, "/api/v1/operations/destinations/confirm", map[string]any{"destination_key": status.Key, "probe_id": "not-a-probe", "found": true})
	if writer.Code != http.StatusConflict {
		t.Fatalf("不应接受伪造探针: %d %s", writer.Code, writer.Body.String())
	}
	writer = destinationRequest(t, server, "/api/v1/platform/trash/probe", map[string]string{"destination_key": status.Key})
	if writer.Code != http.StatusOK {
		t.Fatalf("探针失败: %d %s", writer.Code, writer.Body.String())
	}
	status, err = server.operationDestinations(context.Background())
	if err != nil || status.Probe == nil || status.Probe.State != "moved" || status.TrashVerified {
		t.Fatalf("移动成功还不代表用户目视确认: %+v %v", status, err)
	}
	if err := server.requireOperationDestinations(context.Background(), operationPreviewRequest{Trash: true}); err == nil {
		t.Fatal("不能跳过用户目视确认")
	}
	if data, err := os.ReadFile(filepath.Join(root, "untouched.jsonl")); err != nil || string(data) != "{\"keep\":true}\n" {
		t.Fatal("探针不应修改源数据")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("源目录不应出现探针产物: %v %v", entries, err)
	}
	if err := verifyTrashProbe(status.Probe); err != nil {
		t.Fatal(err)
	}
	writer = destinationRequest(t, server, "/api/v1/operations/destinations/confirm", map[string]any{"destination_key": status.Key, "probe_id": status.Probe.ID, "found": true})
	if writer.Code != http.StatusOK {
		t.Fatalf("确认失败: %d %s", writer.Code, writer.Body.String())
	}
	if err := server.requireOperationDestinations(context.Background(), operationPreviewRequest{Trash: true}); err != nil {
		t.Fatal(err)
	}
	settings, _ := server.loadTrashSettings(context.Background())
	if settings.Verification == nil {
		t.Fatal("应保存用户确认")
	}
	if server.lifecycle.FixtureOnly != true {
		t.Fatal("目的地确认不能改变真实数据授权")
	}
}

func TestTrashProbeRejectsTamperingAndInvalidatesChangedPath(t *testing.T) {
	server, _ := destinationTestServer(t)
	status, _ := server.operationDestinations(context.Background())
	writer := destinationRequest(t, server, "/api/v1/platform/trash/probe", map[string]string{"destination_key": status.Key})
	if writer.Code != http.StatusOK {
		t.Fatal(writer.Body.String())
	}
	status, _ = server.operationDestinations(context.Background())
	if err := os.WriteFile(status.Probe.TrashPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer = destinationRequest(t, server, "/api/v1/operations/destinations/confirm", map[string]any{"destination_key": status.Key, "probe_id": status.Probe.ID, "found": true})
	if writer.Code != http.StatusConflict {
		t.Fatal("损坏探针不能通过确认")
	}
	other := filepath.Join(t.TempDir(), "Trash", "files")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	writer = destinationRequest(t, server, "/api/v1/platform/trash/settings", map[string]string{"custom_path": other})
	if writer.Code != http.StatusOK {
		t.Fatal(writer.Body.String())
	}
	writer = destinationRequest(t, server, "/api/v1/operations/destinations/confirm", map[string]any{"destination_key": status.Key, "probe_id": status.Probe.ID, "found": true})
	if writer.Code != http.StatusConflict {
		t.Fatal("路径变化必须拒绝旧确认")
	}
	updated, _ := server.operationDestinations(context.Background())
	if updated.TrashVerified || updated.Probe != nil {
		t.Fatal("改路径后必须重新测试")
	}
	writer = destinationRequest(t, server, "/api/v1/platform/trash/probe", map[string]string{"destination_key": status.Key})
	if writer.Code != http.StatusConflict {
		t.Fatal("过期目的地不可生成探针")
	}
}

func TestArchiveDestinationConfirmationIsBoundToWorkspaceAndPath(t *testing.T) {
	server, _ := destinationTestServer(t)
	status, _ := server.operationDestinations(context.Background())
	if err := server.requireOperationDestinations(context.Background(), operationPreviewRequest{Archive: true, ArchiveRemote: true}); err != nil {
		t.Fatalf("纯云端归档不应要求本地归档目录确认: %v", err)
	}
	if err := server.requireOperationDestinations(context.Background(), operationPreviewRequest{Archive: true, ArchiveLocal: true}); err == nil {
		t.Fatal("归档前应确认目录")
	}
	writer := destinationRequest(t, server, "/api/v1/operations/destinations/confirm", map[string]string{"destination_key": status.Key, "archive_path": status.ArchivePath})
	if writer.Code != http.StatusOK {
		t.Fatal(writer.Body.String())
	}
	if err := server.requireOperationDestinations(context.Background(), operationPreviewRequest{Archive: true, ArchiveLocal: true}); err != nil {
		t.Fatal(err)
	}
	server.lifecycle.ArchiveRoot = filepath.Join(t.TempDir(), "different-archives")
	if err := server.requireOperationDestinations(context.Background(), operationPreviewRequest{Archive: true, ArchiveLocal: true}); err == nil {
		t.Fatal("归档路径变化必须重新确认")
	}
}

func TestRemoteArchiveDoesNotRequireLocalArchiveRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	options := LifecycleOptions{JournalRoot: filepath.Join(base, "journals")}
	if err := validateLifecycleDestinations(root, options, operationPreviewRequest{Archive: true, ArchiveRemote: true}); err != nil {
		t.Fatalf("纯云端归档不应要求本地 ArchiveRoot: %v", err)
	}
}

func TestTrashConfirmationRejectsReplacedDirectoriesAtTheSamePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 目录身份需要原生文件 ID 验收；本测试覆盖 Unix inode 变化")
	}
	for _, replaceSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "trash", true: "source"}[replaceSource], func(t *testing.T) {
			server, root := destinationTestServer(t)
			status, _ := server.operationDestinations(context.Background())
			writer := destinationRequest(t, server, "/api/v1/platform/trash/probe", map[string]string{"destination_key": status.Key})
			if writer.Code != http.StatusOK {
				t.Fatal(writer.Body.String())
			}
			status, _ = server.operationDestinations(context.Background())
			writer = destinationRequest(t, server, "/api/v1/operations/destinations/confirm", map[string]any{"destination_key": status.Key, "probe_id": status.Probe.ID, "found": true})
			if writer.Code != http.StatusOK {
				t.Fatal(writer.Body.String())
			}
			path := status.TrashPath
			if replaceSource {
				path = root
			}
			if err := os.Rename(path, path+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			current, err := server.operationDestinations(context.Background())
			if err != nil || current.Key == status.Key || current.TrashVerified || current.Probe != nil {
				t.Fatalf("同路径替换目录后必须重新验证: %+v %v", current, err)
			}
			if err := server.requireOperationDestinations(context.Background(), operationPreviewRequest{Trash: true}); err == nil {
				t.Fatal("目录身份变化后不得复用执行许可")
			}
		})
	}
}

func TestMaintenanceIsolationCannotBypassTrashDestinationConfirmation(t *testing.T) {
	server, root := destinationTestServer(t)
	source := filepath.Join(root, "tmp", "stale.tmp")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("maintenance sample"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer := destinationRequest(t, server, "/api/v1/maintenance/preview", map[string]any{"paths": []string{"tmp/stale.tmp"}})
	if writer.Code != http.StatusOK {
		t.Fatal(writer.Body.String())
	}
	var preview struct {
		Plan struct {
			OperationID string `json:"operation_id"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(writer.Body.Bytes(), &preview); err != nil || preview.Plan.OperationID == "" {
		t.Fatalf("维护预览未生成计划: %s %v", writer.Body.String(), err)
	}
	input := map[string]string{"operation_id": preview.Plan.OperationID}
	writer = destinationRequest(t, server, "/api/v1/maintenance/execute", input)
	if writer.Code != http.StatusConflict {
		t.Fatalf("维护隔离未确认回收站时必须拒绝: %d %s", writer.Code, writer.Body.String())
	}
	if content, err := os.ReadFile(source); err != nil || string(content) != "maintenance sample" {
		t.Fatalf("门禁失败不应改变源文件: %v", err)
	}
	status, _ := server.operationDestinations(context.Background())
	writer = destinationRequest(t, server, "/api/v1/platform/trash/probe", map[string]string{"destination_key": status.Key})
	if writer.Code != http.StatusOK {
		t.Fatal(writer.Body.String())
	}
	status, _ = server.operationDestinations(context.Background())
	writer = destinationRequest(t, server, "/api/v1/operations/destinations/confirm", map[string]any{"destination_key": status.Key, "probe_id": status.Probe.ID, "found": true})
	if writer.Code != http.StatusOK {
		t.Fatal(writer.Body.String())
	}
	writer = destinationRequest(t, server, "/api/v1/maintenance/execute", input)
	if writer.Code != http.StatusOK {
		t.Fatalf("确认后重新执行原维护计划失败: %d %s", writer.Code, writer.Body.String())
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("维护样本未进入隔离区: %v", err)
	}
}
