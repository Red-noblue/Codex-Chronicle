// 中文说明：管理 GitHub OAuth Device Flow 的短期会话；访问令牌只交给凭证存储，不进入会话结果。
package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrGitHubDeviceSessionNotFound = errors.New("GitHub 设备授权会话不存在")

const (
	GitHubDeviceStatusPending    = "pending"
	GitHubDeviceStatusAuthorized = "authorized"
	GitHubDeviceStatusFailed     = "failed"
	GitHubDeviceStatusExpired    = "expired"
	GitHubDeviceStatusCanceled   = "canceled"
)

// GitHubDeviceAuthSession 是可返回给 UI 的设备授权会话；其中没有 device_code 或 access_token。
type GitHubDeviceAuthSession struct {
	ID                      string    `json:"id"`
	Status                  string    `json:"status"`
	Account                 string    `json:"account"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	ExpiresAt               time.Time `json:"expires_at"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
	Error                   string    `json:"error,omitempty"`
}

type githubDeviceAuthSession struct {
	GitHubDeviceAuthSession
	code GitHubDeviceCode
}

// GitHubDeviceAuthService 将设备码请求和后台轮询绑定为本地应用会话。
// 默认凭证存储是进程内存，仅用于开发或显式短生命周期运行；生产环境应注入系统凭证库实现。
type GitHubDeviceAuthService struct {
	authorizer *GitHubDeviceAuthorizer
	store      GitHubTokenStore
	account    string
	ctx        context.Context
	cancel     context.CancelFunc

	mu       sync.RWMutex
	sessions map[string]*githubDeviceAuthSession
}

// GitHubCredentialArchiveProvider 延迟从凭证存储构造 GitHub provider，允许 daemon 在首次绑定前正常启动。
type GitHubCredentialArchiveProvider struct {
	auth           *GitHubDeviceAuthService
	config         GitHubConfig
	selectionStore GitHubArchiveSelectionStore
	mu             sync.RWMutex
	selection      *GitHubArchiveSelection
}

// ProviderInfo 返回延迟凭证 provider 的通用身份；凭证状态仍由专用认证 API 返回。
func (p *GitHubCredentialArchiveProvider) ProviderInfo() RemoteArchiveProviderInfo {
	return RemoteArchiveProviderInfo{
		ID: "github", Name: "GitHub", TargetRequired: true,
		SupportsListing: true, SupportsRecovery: true, SupportsDelete: true,
	}
}

// NewGitHubCredentialArchiveProvider 创建不持有访问令牌的远端 provider 代理。
func NewGitHubCredentialArchiveProvider(auth *GitHubDeviceAuthService, config GitHubConfig) (*GitHubCredentialArchiveProvider, error) {
	return NewGitHubCredentialArchiveProviderWithSelectionStore(auth, config, nil)
}

// NewGitHubCredentialArchiveProviderWithSelectionStore 创建支持运行时仓库选择的远端 provider。
// 已有选择优先于启动参数；启动参数仍可作为首次运行时的默认目标。
func NewGitHubCredentialArchiveProviderWithSelectionStore(auth *GitHubDeviceAuthService, config GitHubConfig, selectionStore GitHubArchiveSelectionStore) (*GitHubCredentialArchiveProvider, error) {
	if auth == nil {
		return nil, fmt.Errorf("GitHub 设备授权服务未初始化")
	}
	if strings.TrimSpace(config.Token) != "" {
		return nil, fmt.Errorf("GitHub provider token 必须来自凭证存储")
	}
	provider := &GitHubCredentialArchiveProvider{auth: auth, config: config, selectionStore: selectionStore}
	if selectionStore != nil {
		selection, err := selectionStore.Load(context.Background())
		if err != nil && !errors.Is(err, ErrGitHubArchiveSelectionNotFound) {
			return nil, err
		}
		if err == nil {
			provider.selection = &selection
		}
	}
	return provider, nil
}

func (p *GitHubCredentialArchiveProvider) provider(ctx context.Context) (*GitHubProvider, error) {
	if p == nil || p.auth == nil {
		return nil, fmt.Errorf("GitHub 远端 provider 未初始化")
	}
	p.mu.RLock()
	config := p.config
	if p.selection != nil {
		config.Owner = p.selection.Owner
		config.Repository = p.selection.Repository
	}
	p.mu.RUnlock()
	if strings.TrimSpace(config.Owner) == "" || strings.TrimSpace(config.Repository) == "" {
		return nil, fmt.Errorf("尚未选择 GitHub 归档仓库")
	}
	return p.auth.NewArchiveProvider(ctx, config)
}

// SelectedArchiveRepository 返回当前有效目标；未选择且没有启动默认值时返回 false。
func (p *GitHubCredentialArchiveProvider) SelectedArchiveRepository(ctx context.Context) (GitHubRepositorySummary, bool, error) {
	if err := ctx.Err(); err != nil {
		return GitHubRepositorySummary{}, false, err
	}
	if p == nil {
		return GitHubRepositorySummary{}, false, fmt.Errorf("GitHub 远端 provider 未初始化")
	}
	p.mu.RLock()
	config := p.config
	if p.selection != nil {
		config.Owner = p.selection.Owner
		config.Repository = p.selection.Repository
	}
	p.mu.RUnlock()
	if strings.TrimSpace(config.Owner) == "" || strings.TrimSpace(config.Repository) == "" {
		return GitHubRepositorySummary{}, false, nil
	}
	return GitHubRepositorySummary{Owner: config.Owner, Name: config.Repository, FullName: config.Owner + "/" + config.Repository}, true, nil
}

// SelectArchiveRepository 保存并立即应用用户选中的仓库。
func (p *GitHubCredentialArchiveProvider) SelectArchiveRepository(ctx context.Context, owner, repository string) (GitHubRepositorySummary, error) {
	selection := GitHubArchiveSelection{Owner: strings.TrimSpace(owner), Repository: strings.TrimSpace(repository), UpdatedAt: time.Now().UTC()}
	if err := validateGitHubArchiveSelection(selection); err != nil {
		return GitHubRepositorySummary{}, err
	}
	if err := ctx.Err(); err != nil {
		return GitHubRepositorySummary{}, err
	}
	if p == nil {
		return GitHubRepositorySummary{}, fmt.Errorf("GitHub 远端 provider 未初始化")
	}
	if p.selectionStore != nil {
		if err := p.selectionStore.Save(ctx, selection); err != nil {
			return GitHubRepositorySummary{}, err
		}
	}
	p.mu.Lock()
	p.selection = &selection
	p.mu.Unlock()
	return GitHubRepositorySummary{Owner: selection.Owner, Name: selection.Repository, FullName: selection.Owner + "/" + selection.Repository}, nil
}

// ClearArchiveRepository 清除本地选择；不会删除远端仓库或归档对象。
func (p *GitHubCredentialArchiveProvider) ClearArchiveRepository(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("GitHub 远端 provider 未初始化")
	}
	if p.selectionStore != nil {
		if err := p.selectionStore.Clear(ctx); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.selection = nil
	p.mu.Unlock()
	return nil
}

// UploadDirectory 在上传动作开始时读取系统凭证，不把令牌交给调用方。
func (p *GitHubCredentialArchiveProvider) UploadDirectory(ctx context.Context, directory, archiveID string) (RemoteArchiveResult, error) {
	provider, err := p.provider(ctx)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	repositories, err := NewGitHubRepositoryClient(GitHubRepositoryClientConfig{BaseURL: provider.config.BaseURL, HTTPClient: provider.client}, provider.config.Token)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	if err := repositories.PrivateArchiveRepository(ctx, provider.config.Owner, provider.config.Repository); err != nil {
		return RemoteArchiveResult{}, err
	}
	return provider.UploadDirectory(ctx, directory, archiveID)
}

// VerifyArchive 在校验动作开始时读取系统凭证。
func (p *GitHubCredentialArchiveProvider) VerifyArchive(ctx context.Context, archiveID string) (RemoteArchiveResult, error) {
	provider, err := p.provider(ctx)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	return provider.VerifyArchive(ctx, archiveID)
}

// ListArchives 在列表动作开始时读取系统凭证。
func (p *GitHubCredentialArchiveProvider) ListArchives(ctx context.Context) ([]GitHubArchiveSummary, error) {
	provider, err := p.provider(ctx)
	if err != nil {
		return nil, err
	}
	return provider.ListArchives(ctx)
}

// DownloadArchive 在恢复动作开始时读取系统凭证，并把归档写入调用方指定的独立目录。
func (p *GitHubCredentialArchiveProvider) DownloadArchive(ctx context.Context, archiveID, destination string) (RemoteArchiveResult, error) {
	provider, err := p.provider(ctx)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	return provider.DownloadArchive(ctx, archiveID, destination)
}

// DeleteArchive 仅供显式远端删除流程调用，不由列表接口触发。
func (p *GitHubCredentialArchiveProvider) DeleteArchive(ctx context.Context, archiveID string) error {
	provider, err := p.provider(ctx)
	if err != nil {
		return err
	}
	return provider.DeleteArchive(ctx, archiveID)
}

// NewGitHubDeviceAuthService 创建设备授权服务并验证 OAuth 配置。
func NewGitHubDeviceAuthService(config GitHubDeviceAuthConfig, store GitHubTokenStore, account string) (*GitHubDeviceAuthService, error) {
	authorizer, err := NewGitHubDeviceAuthorizer(config)
	if err != nil {
		return nil, err
	}
	service := NewGitHubCredentialService(store, account)
	service.authorizer = authorizer
	return service, nil
}

// NewGitHubCredentialService 允许显式导入 CLI 凭证；不需要也不伪造 OAuth Client ID。
func NewGitHubCredentialService(store GitHubTokenStore, account string) *GitHubDeviceAuthService {
	if store == nil {
		store = NewMemoryGitHubTokenStore()
	}
	account = strings.TrimSpace(account)
	if account == "" {
		account = "default"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &GitHubDeviceAuthService{
		store:    store,
		account:  account,
		ctx:      ctx,
		cancel:   cancel,
		sessions: make(map[string]*githubDeviceAuthSession),
	}
}

// DeviceFlowAvailable 区分凭证存储就绪和设备授权可用。
func (s *GitHubDeviceAuthService) DeviceFlowAvailable() bool {
	return s != nil && s.authorizer != nil
}

// SaveCredential 保存已核验的外部凭证，同时取消旧授权轮询，避免旧轮询覆盖新账户。
func (s *GitHubDeviceAuthService) SaveCredential(ctx context.Context, token string) error {
	if s == nil || s.store == nil || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("GitHub 凭证无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.Save(ctx, s.account, token); err != nil {
		return fmt.Errorf("GitHub 凭证保存失败")
	}
	s.cancelPendingLocked()
	return nil
}

// Start 请求设备码并立即返回 UI 所需信息；授权轮询在后台执行。
func (s *GitHubDeviceAuthService) Start(ctx context.Context) (GitHubDeviceAuthSession, error) {
	if s == nil || s.authorizer == nil {
		return GitHubDeviceAuthSession{}, fmt.Errorf("GitHub 设备授权服务未初始化")
	}
	s.mu.RLock()
	pollContext := s.ctx
	s.mu.RUnlock()
	code, err := s.authorizer.RequestDeviceCode(ctx)
	if err != nil {
		return GitHubDeviceAuthSession{}, err
	}
	id, err := NewGitHubArchiveID()
	if err != nil {
		return GitHubDeviceAuthSession{}, err
	}
	now := time.Now().UTC()
	session := &githubDeviceAuthSession{
		GitHubDeviceAuthSession: GitHubDeviceAuthSession{
			ID: id, Status: GitHubDeviceStatusPending, Account: s.account,
			UserCode: code.UserCode, VerificationURI: code.VerificationURI,
			VerificationURIComplete: code.VerificationURIComplete,
			ExpiresAt:               now.Add(time.Duration(code.ExpiresIn) * time.Second), CreatedAt: now, UpdatedAt: now,
		},
		code: code,
	}
	s.mu.Lock()
	if pollContext != s.ctx || pollContext.Err() != nil {
		s.mu.Unlock()
		return GitHubDeviceAuthSession{}, fmt.Errorf("GitHub 授权配置已变化，请重新开始绑定")
	}
	s.pruneSessionsLocked()
	s.sessions[id] = session
	result := session.GitHubDeviceAuthSession
	s.mu.Unlock()
	go s.poll(pollContext, id, code)
	return result, nil
}

// Get 返回会话快照；访问令牌和 device_code 永远不会从该方法返回。
func (s *GitHubDeviceAuthService) Get(id string) (GitHubDeviceAuthSession, error) {
	if s == nil {
		return GitHubDeviceAuthSession{}, ErrGitHubDeviceSessionNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[strings.TrimSpace(id)]
	if !ok {
		return GitHubDeviceAuthSession{}, ErrGitHubDeviceSessionNotFound
	}
	if session.Status == GitHubDeviceStatusPending && time.Now().UTC().After(session.ExpiresAt) {
		session.Status = GitHubDeviceStatusExpired
		session.Error = "GitHub 设备授权已过期"
		session.UpdatedAt = time.Now().UTC()
	}
	return session.GitHubDeviceAuthSession, nil
}

// Latest 供刷新或重新打开页面时恢复最近的授权状态，不返回任何凭证。
func (s *GitHubDeviceAuthService) Latest() (GitHubDeviceAuthSession, error) {
	if s == nil {
		return GitHubDeviceAuthSession{}, ErrGitHubDeviceSessionNotFound
	}
	s.mu.RLock()
	var latest *githubDeviceAuthSession
	for _, session := range s.sessions {
		if latest == nil || session.CreatedAt.After(latest.CreatedAt) {
			latest = session
		}
	}
	var id string
	if latest != nil {
		id = latest.ID
	}
	s.mu.RUnlock()
	return s.Get(id)
}

// CredentialAvailable 只返回是否存在已保存凭证，不返回凭证内容。
func (s *GitHubDeviceAuthService) CredentialAvailable(ctx context.Context) (bool, error) {
	if s == nil || s.store == nil {
		return false, fmt.Errorf("GitHub 凭证存储未初始化")
	}
	_, err := s.store.Load(ctx, s.account)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrGitHubCredentialNotFound) {
		return false, nil
	}
	return false, err
}

// NewArchiveProvider 从凭证存储构造 GitHub 归档 provider；调用方配置中不得携带令牌。
func (s *GitHubDeviceAuthService) NewArchiveProvider(ctx context.Context, config GitHubConfig) (*GitHubProvider, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("GitHub 凭证存储未初始化")
	}
	if strings.TrimSpace(config.Token) != "" {
		return nil, fmt.Errorf("GitHub provider token 必须来自凭证存储")
	}
	token, err := s.store.Load(ctx, s.account)
	if err != nil {
		return nil, err
	}
	config.Token = token
	return NewGitHubProvider(config)
}

// Revoke 删除当前账户的运行时凭证，不影响任何源 Codex 文件或远端归档。
func (s *GitHubDeviceAuthService) Revoke(ctx context.Context) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("GitHub 凭证存储未初始化")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelPendingLocked()
	if err := s.store.Delete(ctx, s.account); err != nil {
		return err
	}
	for _, session := range s.sessions {
		session.Status = GitHubDeviceStatusCanceled
		session.Error = ""
		session.UpdatedAt = time.Now().UTC()
	}
	return nil
}

func (s *GitHubDeviceAuthService) cancelPendingLocked() {
	s.cancel()
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for _, session := range s.sessions {
		if session.Status == GitHubDeviceStatusPending {
			session.Status = GitHubDeviceStatusCanceled
			session.UpdatedAt = time.Now().UTC()
		}
	}
}

// Close 取消仍在运行的轮询；不会删除已保存凭证。
func (s *GitHubDeviceAuthService) Close() {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.cancel()
		for _, session := range s.sessions {
			if session.Status == GitHubDeviceStatusPending {
				session.Status = GitHubDeviceStatusCanceled
				session.UpdatedAt = time.Now().UTC()
			}
		}
	}
}

func (s *GitHubDeviceAuthService) poll(ctx context.Context, id string, code GitHubDeviceCode) {
	token, err := s.authorizer.PollAccessToken(ctx, code)
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok || ctx != s.ctx || ctx.Err() != nil {
		return
	}
	if err != nil {
		session.UpdatedAt = time.Now().UTC()
		session.Error = safeGitHubAuthError(err, code.DeviceCode)
		switch {
		case errors.Is(err, context.Canceled):
			session.Status = GitHubDeviceStatusCanceled
		case strings.Contains(err.Error(), "已过期"):
			session.Status = GitHubDeviceStatusExpired
		default:
			session.Status = GitHubDeviceStatusFailed
		}
		return
	}
	if saveErr := s.store.Save(ctx, s.account, token.AccessToken); saveErr != nil {
		session.Status = GitHubDeviceStatusFailed
		session.Error = "GitHub 已授权，但凭证保存失败。请检查系统凭证库后重新连接。"
		session.UpdatedAt = time.Now().UTC()
		return
	}
	session.Status = GitHubDeviceStatusAuthorized
	session.Error = ""
	session.UpdatedAt = time.Now().UTC()
}

func (s *GitHubDeviceAuthService) pruneSessionsLocked() {
	const maxSessions = 32
	if len(s.sessions) < maxSessions {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, session := range s.sessions {
		if session.Status == GitHubDeviceStatusPending {
			continue
		}
		if oldestID == "" || session.UpdatedAt.Before(oldest) {
			oldestID, oldest = id, session.UpdatedAt
		}
	}
	if oldestID == "" {
		for id, session := range s.sessions {
			if oldestID == "" || session.CreatedAt.Before(oldest) {
				oldestID, oldest = id, session.CreatedAt
			}
		}
	}
	if oldestID != "" && !oldest.IsZero() {
		delete(s.sessions, oldestID)
	}
}

func safeGitHubAuthError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "GitHub 授权失败"
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[已隐藏]")
		}
	}
	return message
}
