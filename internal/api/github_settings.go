// 中文说明：让 GitHub 连接设置持久化并即时生效；仅保存公开配置，凭证交给系统凭证库，配置变化使旧远端预览失效。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/platform"
)

// GitHubSettings 不包含 token、client secret 或代理密码。
type GitHubSettings struct {
	ClientID       string `json:"client_id"`
	OAuthBaseURL   string `json:"oauth_base_url"`
	APIBaseURL     string `json:"api_base_url"`
	ProxyURL       string `json:"proxy_url"`
	ArchiveRoot    string `json:"archive_root"`
	Branch         string `json:"branch"`
	ChunkMiB       int64  `json:"chunk_mib"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type githubSettingsState struct {
	settings     GitHubSettings
	path         string
	persisted    bool
	editable     bool
	account      string
	tokens       archive.GitHubTokenStore
	selection    archive.GitHubArchiveSelectionStore
	auth         *archive.GitHubDeviceAuthService
	repositories archive.GitHubRepositoryCatalog
	provider     *archive.GitHubCredentialArchiveProvider
	client       *http.Client
	cli          interface {
		Available() bool
		Token(context.Context) (string, error)
	}
}

// SetGitHubSettings 仅在接收请求前调用；构造与读取配置均不联网、不创建仓库、不访问源数据。
func (s *Server) SetGitHubSettings(path string, defaults GitHubSettings, tokens archive.GitHubTokenStore, account string, selection archive.GitHubArchiveSelectionStore) error {
	if strings.TrimSpace(path) == "" || tokens == nil {
		return fmt.Errorf("GitHub 设置需要独立配置路径与凭证库")
	}
	abs, err := filepath.Abs(path)
	if err != nil || pathHasCodexComponent(abs) || s.currentRoot() != "" && pathsOverlap(s.currentRoot(), abs) {
		return fmt.Errorf("GitHub 设置必须位于源目录与 .codex 之外")
	}
	settings, err := normalizeGitHubSettings(defaults)
	if err != nil {
		return err
	}
	state := &githubSettingsState{settings: settings, path: abs, editable: true, tokens: tokens, account: account, selection: selection, cli: platform.GitHubCLI{}}
	if info, configured := s.remoteArchiveInfo(); configured && info.ID != "github" {
		state.editable = false
		s.githubSettings = state
		return nil
	}
	data, err := readGitHubSettings(abs)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		settings, err = normalizeGitHubSettings(data)
		if err != nil {
			return err
		}
		state.settings, state.persisted = settings, true
		if err := state.connect(settings); err != nil {
			return err
		}
	} else if s.lifecycle != nil && s.lifecycle.GitHubAuth != nil {
		state.auth, state.repositories = s.lifecycle.GitHubAuth, s.lifecycle.GitHubRepositories
		state.provider, _ = s.lifecycle.RemoteArchive.(*archive.GitHubCredentialArchiveProvider)
		state.client, err = archive.NewProxyHTTPClient(settings.ProxyURL, nil)
		if err != nil {
			return err
		}
		state.client.Timeout = time.Duration(settings.TimeoutSeconds) * time.Second
	} else if err := state.connect(settings); err != nil {
		return err
	}
	s.githubSettings = state
	return nil
}

func normalizeGitHubSettings(value GitHubSettings) (GitHubSettings, error) {
	value.ClientID = strings.TrimSpace(value.ClientID)
	if len(value.ClientID) > 200 || strings.ContainsAny(value.ClientID, " \r\n\t") || strings.HasPrefix(value.ClientID, "gh") && (strings.Contains(value.ClientID, "_") || strings.HasPrefix(value.ClientID, "github_pat")) {
		return value, fmt.Errorf("请填写 OAuth App 的公开 Client ID，不要填写访问令牌")
	}
	if value.OAuthBaseURL == "" {
		value.OAuthBaseURL = "https://github.com"
	}
	if value.APIBaseURL == "" {
		value.APIBaseURL = "https://api.github.com"
	}
	for _, item := range []*string{&value.OAuthBaseURL, &value.APIBaseURL} {
		*item = strings.TrimRight(strings.TrimSpace(*item), "/")
		parsed, err := url.Parse(*item)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return value, fmt.Errorf("GitHub 服务地址无效，不允许携带凭证或查询参数")
		}
		ip := net.ParseIP(parsed.Hostname())
		loopback := parsed.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
			return value, fmt.Errorf("GitHub 服务必须使用 HTTPS；仅本机测试允许 HTTP")
		}
	}
	value.ProxyURL = strings.TrimSpace(value.ProxyURL)
	if value.ProxyURL != "" {
		parsed, err := url.Parse(value.ProxyURL)
		if err != nil || parsed.User != nil {
			return value, fmt.Errorf("代理地址无效；连接设置不保存代理用户名或密码")
		}
	}
	if _, err := archive.NewProxyHTTPClient(value.ProxyURL, nil); err != nil {
		return value, fmt.Errorf("代理地址无效，请使用 HTTP、HTTPS 或 SOCKS5 地址")
	}
	value.ArchiveRoot = strings.TrimSpace(value.ArchiveRoot)
	if value.ArchiveRoot == "" {
		value.ArchiveRoot = "codex-chronicle"
	}
	value.Branch = strings.TrimSpace(value.Branch)
	if value.ChunkMiB == 0 {
		value.ChunkMiB = 8
	}
	if value.TimeoutSeconds == 0 {
		value.TimeoutSeconds = 120
	}
	if value.ChunkMiB < 1 || value.ChunkMiB > 50 || value.TimeoutSeconds < 5 || value.TimeoutSeconds > 300 {
		return value, fmt.Errorf("分片大小应为 1–50 MiB，请求超时应为 5–300 秒")
	}
	if _, err := archive.NewGitHubProvider(archive.GitHubConfig{Owner: "validation", Repository: "validation", Token: "validation", RootPath: value.ArchiveRoot, Branch: value.Branch}); err != nil {
		return value, fmt.Errorf("GitHub 归档目录或分支配置无效")
	}
	return value, nil
}

func (state *githubSettingsState) connect(settings GitHubSettings) error {
	client, err := archive.NewProxyHTTPClient(settings.ProxyURL, nil)
	if err != nil {
		return err
	}
	client.Timeout = time.Duration(settings.TimeoutSeconds) * time.Second
	// 不同 OAuth App 或企业端点使用独立的凭证账户，避免更换地址后把旧凭证发送给新服务。
	identity := sha256.Sum256([]byte(settings.ClientID + "\n" + settings.OAuthBaseURL + "\n" + settings.APIBaseURL))
	account := fmt.Sprintf("%s-%x", state.account, identity[:12])
	var auth *archive.GitHubDeviceAuthService
	if settings.ClientID != "" {
		auth, err = archive.NewGitHubDeviceAuthService(archive.GitHubDeviceAuthConfig{
			ClientID: settings.ClientID, BaseURL: settings.OAuthBaseURL, Scope: "repo", HTTPClient: client,
		}, state.tokens, account)
		if err != nil {
			return err
		}
	} else {
		auth = archive.NewGitHubCredentialService(state.tokens, account)
	}
	repositories, err := archive.NewGitHubCredentialRepositoryClient(auth, archive.GitHubRepositoryClientConfig{BaseURL: settings.APIBaseURL, HTTPClient: client})
	if err != nil {
		auth.Close()
		return err
	}
	provider, err := archive.NewGitHubCredentialArchiveProviderWithSelectionStore(auth, archive.GitHubConfig{
		BaseURL: settings.APIBaseURL, RootPath: settings.ArchiveRoot, Branch: settings.Branch, ChunkBytes: settings.ChunkMiB << 20, HTTPClient: client,
	}, state.selection)
	if err != nil {
		auth.Close()
		return err
	}
	state.auth, state.repositories, state.provider, state.client = auth, repositories, provider, client
	return nil
}

func readGitHubSettings(path string) (GitHubSettings, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return GitHubSettings{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return GitHubSettings{}, fmt.Errorf("GitHub 配置必须为不超过 16 KiB 的普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return GitHubSettings{}, err
	}
	defer file.Close()
	var value GitHubSettings
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("GitHub 配置无效或包含不支持的字段")
	}
	return value, nil
}

func saveGitHubSettings(path string, settings GitHubSettings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("GitHub 配置目标必须为普通文件")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".github-settings-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(settings); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (s *Server) githubSettingsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodPut {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET 或 PUT")
		return
	}
	state := s.githubSettings
	if state == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"editable": false, "configured": s.githubAuthService() != nil})
		return
	}
	if request.Method == http.MethodPut {
		if !state.editable {
			writeError(writer, http.StatusConflict, "当前服务已使用其他远端存储，请先调整服务配置")
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
		var input GitHubSettings
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(writer, http.StatusBadRequest, "GitHub 设置无效；不接受 token 或 client secret")
			return
		}
		settings, err := normalizeGitHubSettings(input)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		if !state.persisted || settings != state.settings {
			next := *state
			if err := next.connect(settings); err != nil {
				writeError(writer, http.StatusConflict, "应用 GitHub 配置失败: "+err.Error())
				return
			}
			if err := saveGitHubSettings(state.path, settings); err != nil {
				if next.auth != nil {
					next.auth.Close()
				}
				writeError(writer, http.StatusInternalServerError, "保存 GitHub 配置失败；原配置继续有效")
				return
			}
			if state.auth != nil {
				state.auth.Close()
			}
			next.settings, next.persisted = settings, true
			*state = next
			s.invalidateRemotePreviews()
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"settings": state.settings, "editable": state.editable, "configured": state.auth.DeviceFlowAvailable(),
		"persisted": state.persisted, "source_read_only": true, "restart_required": false,
		"source_archive_supported": s.lifecycleWriteEnabled() && s.lifecycle.ArchiveRoot != "" && !pathHasCodexComponent(s.currentRoot()),
		"source_backup_supported":  s.cloudBackupSupported(),
		"download_available":       s.lifecycle != nil && s.lifecycle.RemoteRecoveryRoot != "",
		"recovery_root":            s.githubRecoveryRoot(),
	})
}

func (s *Server) githubSettingsIdentity() string {
	if s.githubSettings == nil {
		return ""
	}
	data, _ := json.Marshal(s.githubSettings.settings)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *Server) githubRecoveryRoot() string {
	if s.lifecycle == nil {
		return ""
	}
	return s.lifecycle.RemoteRecoveryRoot
}

// remoteConfigurationGuard 保持一次远端操作期间配置不变；变更配置不会等待长任务，也不能复用旧预览。
func (s *Server) remoteConfigurationGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		if !strings.HasPrefix(path, "/api/v1/github/") && !strings.HasPrefix(path, "/api/v1/archives") && !strings.HasPrefix(path, "/api/v1/backups") && !strings.HasPrefix(path, "/api/v1/operations") && path != "/api/v1/capabilities" {
			next.ServeHTTP(writer, request)
			return
		}
		change := path == "/api/v1/github/settings" && request.Method == http.MethodPut || path == "/api/v1/github/archive-target" && request.Method != http.MethodGet || path == "/api/v1/github/auth/revoke" || path == "/api/v1/github/auth/cli"
		if change {
			if !s.remoteConfigMu.TryLock() {
				writeError(writer, http.StatusConflict, "有远端请求或处理任务正在执行，请稍后再修改连接或仓库")
				return
			}
			defer s.remoteConfigMu.Unlock()
		} else {
			s.remoteConfigMu.RLock()
			defer s.remoteConfigMu.RUnlock()
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) invalidateRemotePreviews() {
	s.remoteDownloadMu.Lock()
	clear(s.remoteDownloads)
	s.remoteDownloadMu.Unlock()
	s.remoteDeletesMu.Lock()
	clear(s.remoteDeletes)
	s.remoteDeletesMu.Unlock()
	s.remoteRestoreMu.Lock()
	now := time.Now().UTC()
	for _, plan := range s.remoteRestores {
		if plan != nil && !plan.Executing {
			plan.ExpiresAt = now.Add(-time.Second)
		}
	}
	s.pruneRemoteRestorePlansLocked(now)
	s.remoteRestoreMu.Unlock()
}

func (s *Server) remoteArchiveProvider() archive.RemoteArchiveProvider {
	if s.githubSettings != nil && s.githubSettings.editable {
		if s.githubSettings.provider == nil {
			return nil
		}
		return s.githubSettings.provider
	}
	if s.lifecycle == nil {
		return nil
	}
	return s.lifecycle.RemoteArchive
}

// githubConnectionTestHandler 只发出 GET 请求，不创建仓库、不上传文件、不启动授权。
func (s *Server) githubConnectionTestHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.githubSettings == nil || !s.githubSettings.editable {
		writeError(writer, http.StatusServiceUnavailable, "当前服务未启用 GitHub 连接设置")
		return
	}
	settings := s.githubSettings.settings
	client, err := archive.NewProxyHTTPClient(settings.ProxyURL, nil)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "代理地址无效")
		return
	}
	client.Timeout = 15 * time.Second
	checks := make([]map[string]any, 0, 2)
	for _, target := range []struct{ label, address string }{{"GitHub 授权站点", settings.OAuthBaseURL}, {"GitHub API", settings.APIBaseURL + "/rate_limit"}} {
		ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
		check, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.address, nil)
		check.Header.Set("Accept", "application/json")
		started := time.Now()
		response, err := client.Do(check)
		status, message := 0, ""
		if err != nil {
			message = "连接失败，请检查网络、代理和服务地址"
			if errors.Is(err, context.DeadlineExceeded) {
				message = "连接超时，请检查网络或代理"
			}
		} else {
			status = response.StatusCode
			response.Body.Close()
			if status < 200 || status >= 400 {
				message = fmt.Sprintf("服务返回 HTTP %d", status)
			}
		}
		cancel()
		checks = append(checks, map[string]any{"label": target.label, "ok": message == "", "status": status, "elapsed_ms": time.Since(started).Milliseconds(), "error": message})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"source_read_only": true, "remote_write": false, "checks": checks})
}
