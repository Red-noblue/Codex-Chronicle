// 中文说明：把 GitHub 凭证存储边界接到平台实现；不把访问令牌写入普通配置、日志或归档。
package platform

import (
	"context"
	"fmt"
	"strings"

	"codex-chronicle.local/internal/archive"
)

// SystemGitHubTokenStore 使用当前平台的系统凭证库保存 GitHub 访问令牌。
// 具体系统调用由带平台构建标签的文件提供，未实现的平台会明确报错。
type SystemGitHubTokenStore struct {
	Service string
}

// NewSystemGitHubTokenStore 创建系统凭证库适配器；不会立即访问或修改凭证库。
func NewSystemGitHubTokenStore(service string) (*SystemGitHubTokenStore, error) {
	service = strings.TrimSpace(service)
	if service == "" {
		service = "Codex Chronicle GitHub"
	}
	if len(service) > 256 || strings.ContainsAny(service, "\r\n") {
		return nil, fmt.Errorf("系统凭证服务名过长")
	}
	return &SystemGitHubTokenStore{Service: service}, nil
}

func (s *SystemGitHubTokenStore) Load(ctx context.Context, account string) (string, error) {
	if err := validateSystemCredentialInput(s, account, ""); err != nil {
		return "", err
	}
	return loadSystemGitHubCredential(ctx, s.Service, strings.TrimSpace(account))
}

func (s *SystemGitHubTokenStore) Save(ctx context.Context, account, token string) error {
	if err := validateSystemCredentialInput(s, account, token); err != nil {
		return err
	}
	return saveSystemGitHubCredential(ctx, s.Service, strings.TrimSpace(account), token)
}

func (s *SystemGitHubTokenStore) Delete(ctx context.Context, account string) error {
	if err := validateSystemCredentialInput(s, account, ""); err != nil {
		return err
	}
	return deleteSystemGitHubCredential(ctx, s.Service, strings.TrimSpace(account))
}

func validateSystemCredentialInput(store *SystemGitHubTokenStore, account, token string) error {
	if store == nil || strings.TrimSpace(store.Service) == "" {
		return fmt.Errorf("系统凭证存储未初始化")
	}
	if strings.TrimSpace(account) == "" || strings.ContainsAny(account, "\r\n") {
		return fmt.Errorf("GitHub 凭证账户无效")
	}
	if token != "" && strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("GitHub 凭证内容无效")
	}
	return nil
}

var _ archive.GitHubTokenStore = (*SystemGitHubTokenStore)(nil)
