// 中文说明：验证诊断下载沿用认证边界，状态损坏时仍可导出，且浏览器响应不可缓存。
package api

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/auth"
	updatepkg "codex-chronicle.local/internal/update"
)

func TestUpdateDiagnosticsDownloadSurvivesBrokenStateAndRequiresAuth(t *testing.T) {
	manager, err := updatepkg.NewManager(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.Root(), updatepkg.StateName), []byte("invalid state"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(t.TempDir())
	s.SetVersion("1.0.0")
	s.SetUpdateManager(manager)
	call := func(method string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest(method, "/api/v1/update/diagnostics", nil))
		return response
	}
	response := call(http.MethodGet)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/zip" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("诊断下载响应错误：%d %v", response.Code, response.Header())
	}
	if !strings.HasPrefix(response.Header().Get("Content-Disposition"), `attachment; filename="codex-chronicle-diagnostics-`) || response.Header().Get("X-Chronicle-Diagnostic-ID") == "" {
		t.Fatal("诊断下载缺少安全文件名或编号")
	}
	archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil || len(archive.File) != 2 {
		t.Fatal("诊断结果不是完整 ZIP", err)
	}
	if response := call(http.MethodPost); response.Code != http.StatusMethodNotAllowed {
		t.Fatal("接受未支持的方法")
	}
	security, _, err := auth.NewManager(auth.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetAuthManager(security)
	if response := call(http.MethodGet); response.Code != http.StatusUnauthorized {
		t.Fatalf("诊断下载绕过认证：%d", response.Code)
	}
}

func TestUpdateDiagnosticsWithoutInstallationExplainsOfflineExport(t *testing.T) {
	s := NewServer(t.TempDir())
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/update/diagnostics", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "--export-diagnostics") {
		t.Fatal("未配置安装根时没有提供离线途径", response.Body.String())
	}
}
