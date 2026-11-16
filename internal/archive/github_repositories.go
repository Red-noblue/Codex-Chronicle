// 中文说明：提供用户自有 GitHub 仓库的只读列表和私有仓库创建边界；令牌只在运行时内存中使用。
package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GitHubRepositorySummary 是仓库选择界面所需的最小元数据，不包含访问凭证。
type GitHubRepositorySummary struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description,omitempty"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch,omitempty"`
	HTMLURL       string `json:"html_url,omitempty"`
}

// GitHubAccount 仅提供连接向导所需的公开账户标识。
type GitHubAccount struct {
	Login string `json:"login"`
	Name  string `json:"name,omitempty"`
}

// CurrentAccount 使用 GitHub 核验当前令牌；不返回邮箱、权限清单或凭证。
func (c *GitHubRepositoryClient) CurrentAccount(ctx context.Context) (GitHubAccount, error) {
	request, err := c.newRequest(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return GitHubAccount{}, err
	}
	var account GitHubAccount
	if err := c.doJSON(request, &account); err != nil {
		return GitHubAccount{}, err
	}
	if strings.TrimSpace(account.Login) == "" {
		return GitHubAccount{}, fmt.Errorf("GitHub 未返回有效的账户标识")
	}
	return account, nil
}

// GitHubRepositoryClientConfig 是 GitHub API 的运行时连接配置。
type GitHubRepositoryClientConfig struct {
	BaseURL    string
	ProxyURL   string
	HTTPClient *http.Client
}

// GitHubRepositoryCatalog 是仓库选择/创建的应用边界。
type GitHubRepositoryCatalog interface {
	ListRepositories(ctx context.Context) ([]GitHubRepositorySummary, error)
	CreatePrivateRepository(ctx context.Context, name, description string) (GitHubRepositorySummary, error)
}

// GitHubRepositoryClient 通过 authenticated user API 管理用户自己的仓库。
type GitHubRepositoryClient struct {
	baseURL *url.URL
	client  *http.Client
	token   string
}

// NewGitHubRepositoryClient 创建 API 客户端；token 只保存在当前实例内存。
func NewGitHubRepositoryClient(config GitHubRepositoryClientConfig, token string) (*GitHubRepositoryClient, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("GitHub token 不能为空")
	}
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("GitHub API BaseURL 无效")
	}
	client, err := newGitHubHTTPClient(config.ProxyURL, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	return &GitHubRepositoryClient{baseURL: parsed, client: client, token: token}, nil
}

// ListRepositories 列出当前用户有权访问的仓库，按 GitHub 分页读取。
func (c *GitHubRepositoryClient) ListRepositories(ctx context.Context) ([]GitHubRepositorySummary, error) {
	if c == nil {
		return nil, fmt.Errorf("GitHub 仓库客户端未初始化")
	}
	repositories := make([]GitHubRepositorySummary, 0)
	for page := 1; page <= 1000; page++ {
		request, err := c.newRequest(ctx, http.MethodGet, "/user/repos", nil)
		if err != nil {
			return nil, err
		}
		query := request.URL.Query()
		query.Set("per_page", "100")
		query.Set("page", fmt.Sprintf("%d", page))
		query.Set("sort", "updated")
		query.Set("direction", "desc")
		request.URL.RawQuery = query.Encode()
		var batch []githubRepositoryResponse
		if err := c.doJSON(request, &batch); err != nil {
			return nil, fmt.Errorf("读取 GitHub 仓库列表失败: %w", err)
		}
		for _, item := range batch {
			repositories = append(repositories, item.summary())
		}
		if len(batch) < 100 {
			return repositories, nil
		}
	}
	return nil, fmt.Errorf("GitHub 仓库列表分页超过安全上限")
}

// PrivateArchiveRepository 在上传前重新核实目标仍为私有仓库，避免仓库可见性变化后泄露归档。
func (c *GitHubRepositoryClient) PrivateArchiveRepository(ctx context.Context, owner, name string) error {
	if !validGitHubIdentifier(owner) || !validGitHubIdentifier(name) {
		return fmt.Errorf("GitHub 仓库标识无效")
	}
	request, err := c.newRequest(ctx, http.MethodGet, "/repos/"+owner+"/"+name, nil)
	if err != nil {
		return err
	}
	var repository githubRepositoryResponse
	if err := c.doJSON(request, &repository); err != nil {
		return err
	}
	if !repository.Private {
		return fmt.Errorf("归档上传被阻断：目标 GitHub 仓库已不是私有仓库")
	}
	return nil
}

// CreatePrivateRepository 创建当前用户的私有仓库并自动初始化默认分支。
func (c *GitHubRepositoryClient) CreatePrivateRepository(ctx context.Context, name, description string) (GitHubRepositorySummary, error) {
	if c == nil {
		return GitHubRepositorySummary{}, fmt.Errorf("GitHub 仓库客户端未初始化")
	}
	name = strings.TrimSpace(name)
	if !validGitHubRepositoryName(name) {
		return GitHubRepositorySummary{}, fmt.Errorf("GitHub 仓库名无效")
	}
	payload := map[string]any{
		"name": name, "description": strings.TrimSpace(description), "private": true, "auto_init": true,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return GitHubRepositorySummary{}, err
	}
	request, err := c.newRequest(ctx, http.MethodPost, "/user/repos", strings.NewReader(string(data)))
	if err != nil {
		return GitHubRepositorySummary{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	var response githubRepositoryResponse
	if err := c.doJSON(request, &response); err != nil {
		return GitHubRepositorySummary{}, fmt.Errorf("创建 GitHub 私有仓库失败: %w", err)
	}
	return response.summary(), nil
}

func (c *GitHubRepositoryClient) newRequest(ctx context.Context, method, pathValue string, body io.Reader) (*http.Request, error) {
	endpoint := strings.TrimRight(c.baseURL.String(), "/") + "/" + strings.TrimLeft(pathValue, "/")
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return request, nil
}

func (c *GitHubRepositoryClient) doJSON(request *http.Request, target any) error {
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("GitHub API 请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, sanitizeGitHubMessage(string(message), c.token))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(target); err != nil {
		return fmt.Errorf("GitHub API 响应无法解析: %w", err)
	}
	return nil
}

type githubRepositoryResponse struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	Owner         struct {
		Login string `json:"login"`
		Name  string `json:"name"`
	} `json:"owner"`
}

func (r githubRepositoryResponse) summary() GitHubRepositorySummary {
	owner := strings.TrimSpace(r.Owner.Login)
	if owner == "" {
		owner = strings.TrimSpace(r.Owner.Name)
	}
	return GitHubRepositorySummary{
		Owner: owner, Name: r.Name, FullName: r.FullName, Description: r.Description,
		Private: r.Private, DefaultBranch: r.DefaultBranch, HTMLURL: r.HTMLURL,
	}
}

func validGitHubRepositoryName(name string) bool {
	if name == "" || len(name) > 100 || name == "." || name == ".." {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func sanitizeGitHubMessage(message, token string) string {
	message = strings.TrimSpace(message)
	if token != "" {
		message = strings.ReplaceAll(message, token, "[已隐藏]")
	}
	if message == "" {
		return "GitHub API 返回错误"
	}
	return message
}

// GitHubCredentialRepositoryClient 延迟从授权服务加载凭证，允许绑定完成后立即列出/创建仓库。
type GitHubCredentialRepositoryClient struct {
	auth   *GitHubDeviceAuthService
	config GitHubRepositoryClientConfig
}

// NewGitHubCredentialRepositoryClient 创建不持有访问令牌的仓库客户端代理。
func NewGitHubCredentialRepositoryClient(auth *GitHubDeviceAuthService, config GitHubRepositoryClientConfig) (*GitHubCredentialRepositoryClient, error) {
	if auth == nil {
		return nil, fmt.Errorf("GitHub 设备授权服务未初始化")
	}
	return &GitHubCredentialRepositoryClient{auth: auth, config: config}, nil
}

func (c *GitHubCredentialRepositoryClient) client(ctx context.Context) (*GitHubRepositoryClient, error) {
	if c == nil || c.auth == nil {
		return nil, fmt.Errorf("GitHub 仓库客户端未初始化")
	}
	token, err := c.auth.store.Load(ctx, c.auth.account)
	if err != nil {
		return nil, err
	}
	return NewGitHubRepositoryClient(c.config, token)
}

// ListRepositories 延迟加载凭证后读取仓库列表。
func (c *GitHubCredentialRepositoryClient) ListRepositories(ctx context.Context) ([]GitHubRepositorySummary, error) {
	client, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListRepositories(ctx)
}

// CurrentAccount 从安全凭证库加载令牌后读取当前账户。
func (c *GitHubCredentialRepositoryClient) CurrentAccount(ctx context.Context) (GitHubAccount, error) {
	client, err := c.client(ctx)
	if err != nil {
		return GitHubAccount{}, err
	}
	return client.CurrentAccount(ctx)
}

// CreatePrivateRepository 延迟加载凭证后创建私有仓库。
func (c *GitHubCredentialRepositoryClient) CreatePrivateRepository(ctx context.Context, name, description string) (GitHubRepositorySummary, error) {
	client, err := c.client(ctx)
	if err != nil {
		return GitHubRepositorySummary{}, err
	}
	return client.CreatePrivateRepository(ctx, name, description)
}
