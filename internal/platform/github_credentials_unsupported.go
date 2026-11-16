//go:build !darwin && !linux && !windows

// 中文说明：尚未实现的平台凭证库显式失败，避免把 GitHub 令牌降级写入普通文件。
package platform

import (
	"context"
	"fmt"
)

func loadSystemGitHubCredential(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("当前平台尚未实现系统 GitHub 凭证库")
}

func saveSystemGitHubCredential(context.Context, string, string, string) error {
	return fmt.Errorf("当前平台尚未实现系统 GitHub 凭证库")
}

func deleteSystemGitHubCredential(context.Context, string, string) error {
	return fmt.Errorf("当前平台尚未实现系统 GitHub 凭证库")
}
