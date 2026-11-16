// 中文说明：验证只读发现接口、首次添加生效、选择持久化及不扩大源数据写权限。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/workspace"
)

func TestWorkspaceDiscoveryAndFirstAddRemainReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")
	root := filepath.Join(home, ".codex")
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config", "workspaces.json")
	server, err := NewServerWithWorkspaceConfig("", nil, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	getDiscovery := func() []struct {
		workspace.CodexHomeCandidate
		WorkspaceID string `json:"workspace_id"`
		Current     bool   `json:"current"`
	} {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/discovery", nil))
		var body struct {
			ReadOnly bool `json:"read_only"`
			Items    []struct {
				workspace.CodexHomeCandidate
				WorkspaceID string `json:"workspace_id"`
				Current     bool   `json:"current"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || recorder.Code != http.StatusOK || !body.ReadOnly {
			t.Fatalf("discovery failed: %s err=%v", recorder.Body.String(), err)
		}
		return body.Items
	}
	items := getDiscovery()
	if len(items) != 1 || items[0].Root != root || items[0].Status != "available" || items[0].WorkspaceID != "" {
		t.Fatalf("unexpected discovery: %+v", items)
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) || server.currentRoot() != "" {
		t.Fatal("discovery wrote a registry or changed source")
	}
	input, _ := json.Marshal(map[string]string{"root": root, "name": "本机 .codex"})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(string(input))))
	if recorder.Code != http.StatusCreated || server.currentRoot() != root {
		t.Fatalf("first add did not activate source: %s", recorder.Body.String())
	}
	items = getDiscovery()
	if len(items) != 1 || items[0].WorkspaceID == "" || !items[0].Current {
		t.Fatalf("discovery not linked to registered source: %+v", items)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"write_enabled":false`) {
		t.Fatalf("adding real-like root enabled execution: %s", recorder.Body.String())
	}
	restarted, err := NewServerWithWorkspaceConfig("", nil, nil, config)
	if err != nil || restarted.currentRoot() != root {
		t.Fatalf("restart lost source: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "sessions" {
		t.Fatalf("source was changed: %+v err=%v", entries, err)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/discovery", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatal("discovery unexpectedly accepted mutation")
	}
}

func TestWorkspaceRejectsRegistryInsideSource(t *testing.T) {
	base := t.TempDir()
	config := filepath.Join(base, "config", "workspaces.json")
	if _, err := NewServerWithWorkspaceConfig(base, nil, nil, config); err == nil {
		t.Fatal("startup accepted registry inside source")
	}
	server, err := NewServerWithWorkspaceConfig("", nil, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.addWorkspace(t.Context(), workspace.Definition{Root: base}); err == nil {
		t.Fatal("add accepted source containing registry")
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatal("rejected add wrote source")
	}
}
