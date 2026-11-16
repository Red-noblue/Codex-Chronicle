// 中文说明：验证 GitHub 设备授权会话服务的异步状态和凭证边界。
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitHubDeviceAuthServiceAuthorizesAndStoresWithoutExposingToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
	defer server.Close()
	store := NewMemoryGitHubTokenStore()
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client"}, store, "personal")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	session, err := service.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != GitHubDeviceStatusPending || session.UserCode != "ABCD-EFGH" || session.Account != "personal" {
		t.Fatalf("unexpected device session: %+v", session)
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "device-secret") || strings.Contains(string(encoded), "ghs-runtime-secret") {
		t.Fatalf("device or access token leaked into session JSON: %s", encoded)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		current, getErr := service.Get(session.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == GitHubDeviceStatusAuthorized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device session did not authorize: %+v", current)
		}
		time.Sleep(20 * time.Millisecond)
	}
	available, err := service.CredentialAvailable(context.Background())
	if err != nil || !available {
		t.Fatalf("stored GitHub credential unavailable: %v", err)
	}
	provider, err := service.NewArchiveProvider(context.Background(), GitHubConfig{BaseURL: server.URL, Owner: "owner", Repository: "repo"})
	if err != nil {
		t.Fatalf("construct provider from stored credential: %v", err)
	}
	if provider.config.Token != "ghs-runtime-secret" {
		t.Fatalf("provider did not receive stored credential")
	}
	if _, err := service.NewArchiveProvider(context.Background(), GitHubConfig{BaseURL: server.URL, Owner: "owner", Repository: "repo", Token: "manual-token"}); err == nil {
		t.Fatal("expected manual token injection rejection")
	}
	if err := service.Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	available, err = service.CredentialAvailable(context.Background())
	if err != nil || available {
		t.Fatalf("credential remained after revoke: available=%v err=%v", available, err)
	}
}

func TestGitHubRevokeCancelsPendingAuthorizationBeforeCredentialCanBeSaved(t *testing.T) {
	requested := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/login/device/code" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"device_code": "device-secret", "user_code": "ABCD-EFGH", "verification_uri": "https://github.com/login/device", "expires_in": 600, "interval": 1})
			return
		}
		close(requested)
		<-release
		_ = json.NewEncoder(writer).Encode(map[string]string{"access_token": "must-not-save"})
	}))
	defer server.Close()
	defer close(release)
	store := NewMemoryGitHubTokenStore()
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client"}, store, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	session, err := service.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("授权请求未开始")
	}
	if err := service.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := service.Get(session.ID)
	if current.Status != GitHubDeviceStatusCanceled {
		t.Fatal("解除绑定必须取消待完成授权")
	}
	if available, err := service.CredentialAvailable(t.Context()); err != nil || available {
		t.Fatalf("不应保存已取消的凭证: %v %v", available, err)
	}
}

func TestGitHubImportedCredentialCancelsPendingDeviceAuthorization(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/login/device/code" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"device_code": "device-secret", "user_code": "ABCD-EFGH", "verification_uri": "https://github.com/login/device", "expires_in": 600, "interval": 1})
			return
		}
		<-request.Context().Done()
	}))
	defer remote.Close()
	store := NewMemoryGitHubTokenStore()
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: remote.URL, ClientID: "client"}, store, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	session, err := service.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SaveCredential(t.Context(), "cli-token"); err != nil {
		t.Fatal(err)
	}
	current, _ := service.Get(session.ID)
	if current.Status != GitHubDeviceStatusCanceled {
		t.Fatal("导入 CLI 登录应取消旧设备授权")
	}
	token, err := store.Load(t.Context(), "test")
	if err != nil || token != "cli-token" {
		t.Fatal("应保存新导入的凭证")
	}
}

func TestGitHubCredentialUploadRechecksPrivacyAndRedactsServerErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/repos/owner/repo" {
			_ = json.NewEncoder(writer).Encode(map[string]bool{"private": false})
			return
		}
		t.Error("仓库变为公开后不能开始上传")
		writer.WriteHeader(500)
	}))
	defer server.Close()
	store := NewMemoryGitHubTokenStore()
	_ = store.Save(t.Context(), "test", "runtime-token")
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{ClientID: "client"}, store, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	provider, err := NewGitHubCredentialArchiveProvider(service, GitHubConfig{BaseURL: server.URL, Owner: "owner", Repository: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(t.Context(), t.TempDir(), "test"); err == nil || !strings.Contains(err.Error(), "私有") {
		t.Fatalf("应阻断公开仓库上传: %v", err)
	}
	raw, err := NewGitHubProvider(GitHubConfig{Owner: "owner", Repository: "repo", Token: "runtime-token"})
	if err != nil {
		t.Fatal(err)
	}
	response := &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("denied runtime-token"))}
	if err := raw.responseError(response, "上传", "path"); strings.Contains(err.Error(), "runtime-token") {
		t.Fatal("远端错误响应泄露凭证")
	}
}

func TestGitHubDeviceAuthServiceReportsFailureAndMissingSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/login/device/code" {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"device_code": "device-secret", "user_code": "ABCD-EFGH", "verification_uri": "https://github.com/login/device", "expires_in": 10, "interval": 1,
			})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"error": "access_denied", "error_description": "用户拒绝授权"})
	}))
	defer server.Close()
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client"}, nil, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.Get("missing"); !errors.Is(err, ErrGitHubDeviceSessionNotFound) {
		t.Fatalf("expected missing session error, got %v", err)
	}
	session, err := service.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		current, getErr := service.Get(session.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == GitHubDeviceStatusFailed {
			if !strings.Contains(current.Error, "access_denied") || strings.Contains(current.Error, "device-secret") {
				t.Fatalf("unexpected sanitized failure: %+v", current)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("device session did not fail: %+v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGitHubDeviceAuthServiceCloseCancelsPendingSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/login/device/code" {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"device_code": "device-secret", "user_code": "ABCD-EFGH", "verification_uri": "https://github.com/login/device", "expires_in": 100, "interval": 5,
			})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer server.Close()
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client"}, nil, "default")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	service.Close()
	deadline := time.Now().Add(time.Second)
	for {
		current, getErr := service.Get(session.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == GitHubDeviceStatusCanceled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("device session was not canceled: %+v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGitHubCredentialArchiveProviderLoadsCredentialLazily(t *testing.T) {
	store := NewMemoryGitHubTokenStore()
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: "https://github.example", ClientID: "client"}, store, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	provider, err := NewGitHubCredentialArchiveProvider(service, GitHubConfig{BaseURL: "https://api.github.example", Owner: "owner", Repository: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.provider(context.Background()); !errors.Is(err, ErrGitHubCredentialNotFound) {
		t.Fatalf("expected delayed credential lookup, got %v", err)
	}
	if err := store.Save(context.Background(), "default", "ghs-runtime-secret"); err != nil {
		t.Fatal(err)
	}
	concrete, err := provider.provider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if concrete.config.Token != "ghs-runtime-secret" {
		t.Fatalf("lazy provider did not load stored token")
	}
	var _ RemoteArchiveProvider = provider
	var _ RemoteArchiveCatalog = provider
}

func TestGitHubDeviceAuthLatestRestoresNewestAndExpiresPendingSession(t *testing.T) {
	service := NewGitHubCredentialService(nil, "test")
	defer service.Close()
	if _, err := service.Latest(); !errors.Is(err, ErrGitHubDeviceSessionNotFound) {
		t.Fatal("尚未发起授权时不应返回会话")
	}
	now := time.Now().UTC()
	for _, session := range []GitHubDeviceAuthSession{
		{ID: "old", Status: GitHubDeviceStatusAuthorized, CreatedAt: now.Add(-time.Hour)},
		{ID: "new", Status: GitHubDeviceStatusPending, CreatedAt: now, ExpiresAt: now.Add(-time.Second)},
		{ID: "middle", Status: GitHubDeviceStatusFailed, CreatedAt: now.Add(-time.Minute)},
	} {
		service.sessions[session.ID] = &githubDeviceAuthSession{GitHubDeviceAuthSession: session}
	}
	session, err := service.Latest()
	if err != nil || session.ID != "new" || session.Status != GitHubDeviceStatusExpired || session.Error == "" {
		t.Fatalf("刷新应恢复最近授权，并展示其已过期状态：%+v err=%v", session, err)
	}
}

type failingGitHubCredentialStore struct{ *MemoryGitHubTokenStore }

func (failingGitHubCredentialStore) Save(context.Context, string, string) error {
	return errors.New("synthetic-private-store-error")
}

func TestGitHubDeviceAuthSaveFailureRemainsVisibleWithoutExposingCredential(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "device-secret", "user_code": "ABCD-EFGH", "verification_uri": "https://github.com/login/device", "expires_in": 60, "interval": 1})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "synthetic-private-token"})
	}))
	defer remote.Close()
	store := failingGitHubCredentialStore{NewMemoryGitHubTokenStore()}
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: remote.URL, ClientID: "test"}, store, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		session, err := service.Latest()
		if err != nil {
			t.Fatal(err)
		}
		if session.Status != GitHubDeviceStatusPending {
			if session.Status != GitHubDeviceStatusFailed || !strings.Contains(session.Error, "凭证保存失败") || strings.Contains(session.Error, "synthetic-private") {
				t.Fatalf("保存失败必须可见且不能泄露底层错误或凭证：%+v", session)
			}
			available, err := service.CredentialAvailable(t.Context())
			if err != nil || available {
				t.Fatal("凭证保存失败不能报告已连接")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("未能收到凭证保存失败状态")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
