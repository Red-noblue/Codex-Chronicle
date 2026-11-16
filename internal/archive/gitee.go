// 中文说明：提供 Gitee Contents API 归档 provider；复用已验证的 manifest、分片和校验流程，但保持独立身份。
package archive

import "fmt"

// GiteeConfig 是 Gitee 私有仓库归档的运行时配置；Token 只保存在当前进程内存。
type GiteeConfig struct {
	BaseURL    string
	Owner      string
	Repository string
	RootPath   string
	Token      string
	Branch     string
	ProxyURL   string
	ChunkBytes int64
}

// GiteeProvider 使用 Gitee Contents API 保存归档；其底层协议与 GitHub Contents API 兼容。
type GiteeProvider struct {
	*GitHubProvider
}

// NewGiteeProvider 创建 Gitee provider。默认 API 根地址为 gitee.com/api/v5。
func NewGiteeProvider(config GiteeConfig) (*GiteeProvider, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://gitee.com/api/v5"
	}
	provider, err := NewGitHubProvider(GitHubConfig{
		BaseURL: config.BaseURL, Owner: config.Owner, Repository: config.Repository,
		RootPath: config.RootPath, Token: config.Token, Branch: config.Branch,
		ProxyURL: config.ProxyURL, ChunkBytes: config.ChunkBytes, ProviderID: "gitee",
	})
	if err != nil {
		return nil, fmt.Errorf("创建 Gitee provider 失败: %w", err)
	}
	return &GiteeProvider{GitHubProvider: provider}, nil
}

// ProviderInfo 返回 Gitee provider 的通用身份和能力声明。
func (p *GiteeProvider) ProviderInfo() RemoteArchiveProviderInfo {
	return RemoteArchiveProviderInfo{
		ID: "gitee", Name: "Gitee", TargetRequired: false,
		SupportsListing: true, SupportsRecovery: true, SupportsDelete: true,
	}
}
