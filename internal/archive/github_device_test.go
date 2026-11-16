// 中文说明：验证 GitHub OAuth Device Flow 的请求、轮询、代理和凭证存储边界。
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitHubDeviceAuthorizerRequestsDeviceCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/login/device/code" {
			t.Fatalf("unexpected device code request: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("unexpected content type: %q", request.Header.Get("Content-Type"))
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("parse device code form: %v", err)
		}
		if request.Form.Get("client_id") != "client-123" || request.Form.Get("scope") != "repo:status" {
			t.Fatalf("unexpected device code form: %v", request.Form)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"device_code":               "device-123",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          "https://github.com/login/device",
			"verification_uri_complete": "https://github.com/login/device?user_code=ABCD-EFGH",
			"expires_in":                900,
			"interval":                  5,
		})
	}))
	defer server.Close()

	authorizer, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{
		BaseURL: server.URL, ClientID: "client-123", Scope: "repo:status",
	})
	if err != nil {
		t.Fatal(err)
	}
	code, err := authorizer.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code.DeviceCode != "device-123" || code.UserCode != "ABCD-EFGH" || code.IntervalSeconds != 5 || code.ExpiresIn != 900 {
		t.Fatalf("unexpected device code: %+v", code)
	}
}

func TestGitHubDeviceAuthorizerPollsPendingThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/login/oauth/access_token" {
			t.Fatalf("unexpected access token request: %s %s", request.Method, request.URL.Path)
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("parse access token form: %v", err)
		}
		if request.Form.Get("client_id") != "client-123" || request.Form.Get("device_code") != "device-123" || request.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Fatalf("unexpected access token form: %v", request.Form)
		}
		mu.Lock()
		callCount++
		currentCall := callCount
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if currentCall == 1 {
			_ = json.NewEncoder(writer).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{
			"access_token": "ghs-runtime-secret", "token_type": "bearer", "scope": "repo:status",
		})
	}))
	defer server.Close()

	authorizer, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client-123"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := authorizer.PollAccessTokenWithInterval(context.Background(), GitHubDeviceCode{
		DeviceCode: "device-123", ExpiresIn: 10,
	}, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "ghs-runtime-secret" || token.TokenType != "bearer" || token.Scope != "repo:status" {
		t.Fatalf("unexpected access token: %+v", token)
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Fatalf("expected pending then success, calls=%d", callCount)
	}
}

func TestGitHubDeviceAuthorizerSlowDownHonorsContext(t *testing.T) {
	var mu sync.Mutex
	var callCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]string{"error": "slow_down"})
	}))
	defer server.Close()

	authorizer, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client-123"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = authorizer.PollAccessTokenWithInterval(ctx, GitHubDeviceCode{DeviceCode: "device-123", ExpiresIn: 10}, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline after slow_down, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Fatalf("slow_down should wait before retrying, calls=%d", callCount)
	}
}

func TestGitHubDeviceAuthorizerRejectsAccessDeniedAndDoesNotLeakInvalidToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/login/oauth/access_token" {
			writer.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(writer).Encode(map[string]string{
				"error": "access_denied", "error_description": "用户拒绝授权",
			})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	authorizer, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client-123"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorizer.PollAccessTokenWithInterval(context.Background(), GitHubDeviceCode{DeviceCode: "device-123", ExpiresIn: 10}, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "access_denied") || !strings.Contains(err.Error(), "用户拒绝授权") {
		t.Fatalf("unexpected access denied error: %v", err)
	}

	invalidServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(writer).Encode(map[string]string{"access_token": "ghs-should-not-leak"})
	}))
	defer invalidServer.Close()
	invalidAuthorizer, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{BaseURL: invalidServer.URL, ClientID: "client-123"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = invalidAuthorizer.PollAccessTokenWithInterval(context.Background(), GitHubDeviceCode{DeviceCode: "device-123", ExpiresIn: 10}, time.Millisecond)
	if err == nil || strings.Contains(err.Error(), "ghs-should-not-leak") {
		t.Fatalf("invalid response leaked access token or returned no error: %v", err)
	}
}

func TestGitHubDeviceAuthorizerExpiresPendingAuthorization(t *testing.T) {
	var mu sync.Mutex
	var callCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer server.Close()

	authorizer, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client-123"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorizer.PollAccessTokenWithInterval(context.Background(), GitHubDeviceCode{DeviceCode: "device-123", ExpiresIn: 1}, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("expected expired device authorization, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount == 0 {
		t.Fatal("expected at least one pending poll")
	}
}

func TestGitHubDeviceAuthorizerReusesHTTPProxy(t *testing.T) {
	var mu sync.Mutex
	accessCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/login/device/code":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"device_code": "device-123", "user_code": "ABCD-EFGH", "verification_uri": "https://github.com/login/device", "expires_in": 900,
			})
		case "/login/oauth/access_token":
			mu.Lock()
			accessCalls++
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(map[string]string{"access_token": "ghs-runtime-secret"})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	proxy := httptest.NewServer(http.HandlerFunc(httpForwardProxyHandler))
	defer proxy.Close()

	authorizer, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{BaseURL: server.URL, ClientID: "client-123", ProxyURL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	code, err := authorizer.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("device code through proxy failed: %v", err)
	}
	if _, err := authorizer.PollAccessTokenWithInterval(context.Background(), code, time.Millisecond); err != nil {
		t.Fatalf("access token through proxy failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if accessCalls != 1 {
		t.Fatalf("expected one proxied access token request, calls=%d", accessCalls)
	}
}

func TestMemoryGitHubTokenStoreLifecycle(t *testing.T) {
	store := NewMemoryGitHubTokenStore()
	ctx := context.Background()
	if _, err := store.Load(ctx, "account"); !errors.Is(err, ErrGitHubCredentialNotFound) {
		t.Fatalf("expected missing credential, got %v", err)
	}
	if err := store.Save(ctx, "account", "ghs-runtime-secret"); err != nil {
		t.Fatal(err)
	}
	token, err := store.Load(ctx, "account")
	if err != nil || token != "ghs-runtime-secret" {
		t.Fatalf("unexpected stored token: %q %v", token, err)
	}
	if err := store.Delete(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "account"); !errors.Is(err, ErrGitHubCredentialNotFound) {
		t.Fatalf("expected deleted credential, got %v", err)
	}
	if err := store.Save(ctx, "", "token"); err == nil || !strings.Contains(err.Error(), "无效") {
		t.Fatalf("expected invalid save input error, got %v", err)
	}
	if err := store.Save(ctx, "account", ""); err == nil || !strings.Contains(err.Error(), "无效") {
		t.Fatalf("expected empty token error, got %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Load(canceled, "account"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled load, got %v", err)
	}
}

func TestGitHubDeviceAuthorizerRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{}); err == nil {
		t.Fatal("expected missing client ID rejection")
	}
	if _, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{ClientID: "client", BaseURL: "ftp://github.example"}); err == nil {
		t.Fatal("expected unsupported base URL rejection")
	}
	if _, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{ClientID: "client", ProxyURL: "ftp://127.0.0.1:1080"}); err == nil {
		t.Fatal("expected unsupported proxy rejection")
	}
	if _, err := NewGitHubDeviceAuthorizer(GitHubDeviceAuthConfig{ClientID: "client", BaseURL: "https://github.example/path?query=1"}); err == nil {
		t.Fatal("expected query-bearing base URL rejection")
	}
}
