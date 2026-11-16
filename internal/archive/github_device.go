// 中文说明：实现 GitHub OAuth Device Flow 和可注入凭证存储边界；访问令牌不写入日志、配置或归档。
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrGitHubCredentialNotFound = errors.New("GitHub 凭证不存在")

// GitHubDeviceAuthConfig 是设备授权所需的运行时配置。
// ClientID 是 GitHub OAuth App 的公开标识；访问令牌只在返回值和存储实现内存中流动。
type GitHubDeviceAuthConfig struct {
	BaseURL    string
	ClientID   string
	Scope      string
	ProxyURL   string
	HTTPClient *http.Client
}

// GitHubDeviceAuthorizer 请求设备码并轮询用户授权结果。
type GitHubDeviceAuthorizer struct {
	baseURL  *url.URL
	client   *http.Client
	clientID string
	scope    string
}

// GitHubDeviceCode 是 UI 展示和后续轮询所需的短期信息，不包含访问令牌。
type GitHubDeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	IntervalSeconds         int    `json:"interval"`
}

// GitHubDeviceToken 是设备授权成功后的运行时令牌结果。
type GitHubDeviceToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// GitHubTokenStore 是平台凭证库的最小注入接口。
// macOS Keychain、Windows Credential Manager 和 Linux Secret Service 可分别实现它。
type GitHubTokenStore interface {
	Load(ctx context.Context, account string) (string, error)
	Save(ctx context.Context, account, token string) error
	Delete(ctx context.Context, account string) error
}

// MemoryGitHubTokenStore 仅用于测试和短生命周期运行，不提供持久化保护。
type MemoryGitHubTokenStore struct {
	mu     sync.RWMutex
	tokens map[string]string
}

// NewMemoryGitHubTokenStore 创建进程内凭证存储。
func NewMemoryGitHubTokenStore() *MemoryGitHubTokenStore {
	return &MemoryGitHubTokenStore{tokens: make(map[string]string)}
}

func (s *MemoryGitHubTokenStore) Load(ctx context.Context, account string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == nil {
		return "", ErrGitHubCredentialNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	token, ok := s.tokens[account]
	if !ok {
		return "", ErrGitHubCredentialNotFound
	}
	return token, nil
}

func (s *MemoryGitHubTokenStore) Save(ctx context.Context, account, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || strings.TrimSpace(account) == "" || strings.TrimSpace(token) == "" {
		return fmt.Errorf("GitHub 凭证存储输入无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[account] = token
	return nil
}

func (s *MemoryGitHubTokenStore) Delete(ctx context.Context, account string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || strings.TrimSpace(account) == "" {
		return fmt.Errorf("GitHub 凭证账户无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, account)
	return nil
}

// NewGitHubDeviceAuthorizer 创建设备授权客户端，默认使用 github.com。
func NewGitHubDeviceAuthorizer(config GitHubDeviceAuthConfig) (*GitHubDeviceAuthorizer, error) {
	if strings.TrimSpace(config.ClientID) == "" {
		return nil, fmt.Errorf("GitHub OAuth ClientID 不能为空")
	}
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = "https://github.com"
	}
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("GitHub OAuth BaseURL 无效")
	}
	client, err := newGitHubHTTPClient(config.ProxyURL, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	scope := strings.TrimSpace(config.Scope)
	if scope == "" {
		scope = "repo"
	}
	return &GitHubDeviceAuthorizer{baseURL: parsed, client: client, clientID: config.ClientID, scope: scope}, nil
}

// RequestDeviceCode 请求用户在 GitHub 页面输入的设备码。
func (a *GitHubDeviceAuthorizer) RequestDeviceCode(ctx context.Context) (GitHubDeviceCode, error) {
	if a == nil {
		return GitHubDeviceCode{}, fmt.Errorf("GitHub 设备授权器未初始化")
	}
	form := url.Values{"client_id": {a.clientID}, "scope": {a.scope}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint("/login/device/code"), strings.NewReader(form.Encode()))
	if err != nil {
		return GitHubDeviceCode{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.client.Do(request)
	if err != nil {
		return GitHubDeviceCode{}, fmt.Errorf("请求 GitHub 设备码失败: %w", err)
	}
	defer response.Body.Close()
	var payload githubDeviceCodeResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return GitHubDeviceCode{}, fmt.Errorf("解析 GitHub 设备码响应失败: %w", err)
	}
	if payload.Error != "" {
		return GitHubDeviceCode{}, githubOAuthError(payload.Error, payload.ErrorDescription)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || payload.DeviceCode == "" || payload.UserCode == "" || payload.ExpiresIn <= 0 {
		return GitHubDeviceCode{}, fmt.Errorf("GitHub 设备码响应无效（HTTP %d）", response.StatusCode)
	}
	return GitHubDeviceCode{
		DeviceCode: payload.DeviceCode, UserCode: payload.UserCode,
		VerificationURI: payload.VerificationURI, VerificationURIComplete: payload.VerificationURIComplete,
		ExpiresIn: payload.ExpiresIn, IntervalSeconds: payload.IntervalSeconds,
	}, nil
}

// PollAccessToken 轮询设备授权结果，并遵守 GitHub 返回的间隔和过期时间。
func (a *GitHubDeviceAuthorizer) PollAccessToken(ctx context.Context, code GitHubDeviceCode) (GitHubDeviceToken, error) {
	interval := time.Duration(code.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return a.pollAccessToken(ctx, code, interval)
}

// PollAccessTokenWithInterval 是可控间隔版本，供测试和受控 UI 任务使用。
func (a *GitHubDeviceAuthorizer) PollAccessTokenWithInterval(ctx context.Context, code GitHubDeviceCode, interval time.Duration) (GitHubDeviceToken, error) {
	if interval <= 0 {
		return GitHubDeviceToken{}, fmt.Errorf("设备授权轮询间隔必须为正数")
	}
	return a.pollAccessToken(ctx, code, interval)
}

func (a *GitHubDeviceAuthorizer) pollAccessToken(ctx context.Context, code GitHubDeviceCode, interval time.Duration) (GitHubDeviceToken, error) {
	if a == nil || strings.TrimSpace(code.DeviceCode) == "" || code.ExpiresIn <= 0 {
		return GitHubDeviceToken{}, fmt.Errorf("GitHub 设备授权参数无效")
	}
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return GitHubDeviceToken{}, err
		}
		if time.Now().After(deadline) {
			return GitHubDeviceToken{}, fmt.Errorf("GitHub 设备授权已过期")
		}
		token, oauthErr := a.requestAccessToken(ctx, code.DeviceCode)
		if oauthErr == nil {
			return token, nil
		}
		switch oauthErr.Code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return GitHubDeviceToken{}, oauthErr
		}
		wait := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !wait.Stop() {
				<-wait.C
			}
			return GitHubDeviceToken{}, ctx.Err()
		case <-wait.C:
		}
	}
}

func (a *GitHubDeviceAuthorizer) requestAccessToken(ctx context.Context, deviceCode string) (GitHubDeviceToken, *githubOAuthResponseError) {
	form := url.Values{"client_id": {a.clientID}, "device_code": {deviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint("/login/oauth/access_token"), strings.NewReader(form.Encode()))
	if err != nil {
		return GitHubDeviceToken{}, &githubOAuthResponseError{Code: "request_error", Description: err.Error()}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.client.Do(request)
	if err != nil {
		return GitHubDeviceToken{}, &githubOAuthResponseError{Code: "network_error", Description: err.Error()}
	}
	defer response.Body.Close()
	var payload githubTokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return GitHubDeviceToken{}, &githubOAuthResponseError{Code: "invalid_response", Description: "GitHub 返回内容无法解析"}
	}
	if payload.Error != "" {
		return GitHubDeviceToken{}, &githubOAuthResponseError{Code: payload.Error, Description: payload.ErrorDescription}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || payload.AccessToken == "" {
		return GitHubDeviceToken{}, &githubOAuthResponseError{Code: "invalid_response", Description: fmt.Sprintf("HTTP %d", response.StatusCode)}
	}
	return GitHubDeviceToken{AccessToken: payload.AccessToken, TokenType: payload.TokenType, Scope: payload.Scope}, nil
}

func (a *GitHubDeviceAuthorizer) endpoint(suffix string) string {
	return strings.TrimRight(a.baseURL.String(), "/") + "/" + strings.TrimLeft(suffix, "/")
}

type githubDeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	IntervalSeconds         int    `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

type githubTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type githubOAuthResponseError struct {
	Code        string
	Description string
}

func (e *githubOAuthResponseError) Error() string {
	if e == nil || e.Code == "" {
		return "GitHub OAuth 请求失败"
	}
	if e.Description == "" {
		return "GitHub OAuth 请求失败: " + e.Code
	}
	return "GitHub OAuth 请求失败: " + e.Code + "（" + e.Description + "）"
}

func githubOAuthError(code, description string) error {
	return &githubOAuthResponseError{Code: code, Description: description}
}
