// 中文说明：验证 daemon 静态工作台托管不会覆盖 API，并支持前端路由回退。
package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithWebRootServesAssetsAndPreservesAPI(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>工作台</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("console.log('ok')"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapped, err := WithWebRoot(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/test" {
			t.Fatalf("unexpected API path: %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusNoContent)
	}), root)
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		path string
		code int
		body string
	}{
		{path: "/", code: http.StatusOK, body: "工作台"},
		{path: "/conversations/abc", code: http.StatusOK, body: "工作台"},
		{path: "/sessions/abc", code: http.StatusOK, body: "工作台"},
		{path: "/settings/system", code: http.StatusOK, body: "工作台"},
		{path: "/assets/app.js", code: http.StatusOK, body: "console.log"},
	}
	for _, check := range checks {
		recorder := httptest.NewRecorder()
		wrapped.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, check.path, nil))
		if recorder.Code != check.code || !strings.Contains(recorder.Body.String(), check.body) {
			t.Fatalf("static path %s failed: status=%d body=%s", check.path, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	wrapped.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/test", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("API path was not preserved: status=%d", recorder.Code)
	}
}
