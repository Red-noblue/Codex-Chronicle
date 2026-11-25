// 中文说明：验证阶段 0 daemon 的健康检查响应，不读取或修改真实 Codex 数据。
package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/api"
)

func TestListenOccupiedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().String()
	if actual, err := listen(address, false); err == nil || actual != nil || !strings.Contains(err.Error(), "显式指定的端口不会自动切换") {
		t.Fatalf("显式端口冲突应明确报错：%v", err)
	}
	actual, err := listen(address, true)
	if err != nil {
		t.Fatal(err)
	}
	defer actual.Close()
	if actual.Addr().String() == address || actual.Addr().(*net.TCPAddr).IP.String() != "127.0.0.1" {
		t.Fatalf("自动模式没有避让到本机空闲端口：%s", actual.Addr())
	}
	// 自动绑定的端口必须始终被当前 listener 持有，不能先探测后释放。
	if other, err := net.Listen("tcp", actual.Addr().String()); err == nil {
		other.Close()
		t.Fatal("空闲端口未被持续持有")
	}
}

func TestHealthHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()

	api.NewServer("").Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("unexpected content type: %q", got)
	}
	if body := recorder.Body.String(); body != "{\"status\":\"ok\",\"version\":\"0.0.0-dev\"}\n" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestLoopbackAddressGuard(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8787", "[::1]:8787", "localhost:8787"} {
		if !isLoopbackListenAddress(address) {
			t.Fatalf("expected loopback address: %s", address)
		}
	}
	for _, address := range []string{":8787", "0.0.0.0:8787", "[::]:8787", "192.0.2.1:8787"} {
		if isLoopbackListenAddress(address) {
			t.Fatalf("expected non-loopback address: %s", address)
		}
	}
}

func TestDefaultArchiveRootUsesDocuments(t *testing.T) {
	got := defaultArchiveRoot(filepath.Join("home", "test-user"))
	want := filepath.Join("home", "test-user", "Documents", "Codex Chronicle", "archives")
	if got != want {
		t.Fatalf("default archive root = %q, want %q", got, want)
	}
}

func TestBearerTokenMiddlewareFailsClosed(t *testing.T) {
	const token = "01234567890123456789012345678901"
	next := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	handler := requireBearerToken(next, token)
	for _, authorization := range []string{"", "Bearer wrong", "Basic " + token} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		request.Header.Set("Authorization", authorization)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized || !strings.HasPrefix(recorder.Header().Get("WWW-Authenticate"), "Bearer ") {
			t.Fatalf("invalid authorization was accepted: %q status=%d", authorization, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("valid bearer token was rejected: status=%d", recorder.Code)
	}
}

func TestAuthStatePathBoundary(t *testing.T) {
	root := t.TempDir()
	if !pathIsWithin(root, root) || !pathIsWithin(root, filepath.Join(root, "nested", "auth.json")) {
		t.Fatal("expected auth state path inside root to be detected")
	}
	if pathIsWithin(root, filepath.Join(filepath.Dir(root), "outside-auth.json")) {
		t.Fatal("outside auth state path was incorrectly classified as inside root")
	}
	for _, path := range []string{"/Users/test/.codex/auth.json", `C:\Users\test\.codex\auth.json`} {
		if !authStatePathContainsCodex(path) {
			t.Fatalf("expected .codex component to be rejected: %s", path)
		}
	}
	if authStatePathContainsCodex(filepath.Join(root, "codex-chronicle-auth.json")) {
		t.Fatal("ordinary auth state filename was incorrectly rejected")
	}
}
