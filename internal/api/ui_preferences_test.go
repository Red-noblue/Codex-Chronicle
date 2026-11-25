// 中文说明：验证界面设置跨服务实例恢复、部分更新不丢设置，并拒绝越界路径、凭证和非法输入。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIPreferencesSurviveServerReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui-preferences.json")
	server := NewServer("")
	defer server.Close()
	if err := server.SetUIPreferencesPath(path); err != nil {
		t.Fatal(err)
	}
	for _, patch := range []string{
		`{"codex-chronicle.theme.v1":"dark","codex-chronicle.saved-filters.v1":"{\"workspace\":[]}"}`,
		`{"codex-chronicle.density.v1":"reading"}`,
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "/api/v1/ui-preferences", strings.NewReader(patch)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("保存失败：%d %s", recorder.Code, recorder.Body.String())
		}
	}
	next := NewServer("")
	defer next.Close()
	if err := next.SetUIPreferencesPath(path); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	next.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/ui-preferences", nil))
	var body struct {
		Available   bool              `json:"available"`
		Preferences map[string]string `json:"preferences"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || !body.Available || body.Preferences["codex-chronicle.theme.v1"] != "dark" || body.Preferences["codex-chronicle.density.v1"] != "reading" || body.Preferences["codex-chronicle.saved-filters.v1"] != `{"workspace":[]}` {
		t.Fatalf("更换实例后设置丢失：%s", recorder.Body.String())
	}
}

func TestUIPreferencesRejectInvalidChanges(t *testing.T) {
	server := NewServer("")
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ui-preferences.json")
	if err := server.SetUIPreferencesPath(path); err != nil {
		t.Fatal(err)
	}
	for _, patch := range []string{
		`{"token":"secret"}`, `{"codex-chronicle.theme.v1":"invalid"}`,
		`{"codex-chronicle.density.v1":"tiny"}`, `{"codex-chronicle.saved-filters.v1":"[]"}`,
		`{"codex-chronicle.saved-filters.v1":"null"}`, `null`, `{} {}`,
		`{"codex-chronicle.saved-filters.v1":"` + strings.Repeat("x", uiPreferencesLimit) + `"}`,
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "/api/v1/ui-preferences", strings.NewReader(patch)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("非法设置被接受：%d %s", recorder.Code, recorder.Body.String())
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("非法请求创建了文件：%v", err)
	}
	if err := server.SetUIPreferencesPath(filepath.Join(t.TempDir(), ".codex", "ui-preferences.json")); err == nil {
		t.Fatal("允许在 .codex 内保存设置")
	}
}
