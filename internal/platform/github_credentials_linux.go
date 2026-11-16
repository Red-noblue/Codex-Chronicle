//go:build linux

// 中文说明：通过 secret-tool 访问 Linux Secret Service；令牌经标准输入传递，不写入文件或进程参数。
package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"codex-chronicle.local/internal/archive"
)

func loadSystemGitHubCredential(ctx context.Context, service, account string) (string, error) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return "", fmt.Errorf("Linux Secret Service 不可用：未找到 secret-tool")
	}
	command := exec.CommandContext(ctx, "secret-tool", "lookup", "service", service, "account", account)
	output, err := command.Output()
	if err != nil {
		if isSecretToolNotFound(err) {
			return "", archive.ErrGitHubCredentialNotFound
		}
		return "", fmt.Errorf("读取 Linux Secret Service 凭证失败")
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", archive.ErrGitHubCredentialNotFound
	}
	return token, nil
}

func saveSystemGitHubCredential(ctx context.Context, service, account, token string) error {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return fmt.Errorf("Linux Secret Service 不可用：未找到 secret-tool")
	}
	command := exec.CommandContext(ctx, "secret-tool", "store", "--label=Codex Chronicle GitHub", "service", service, "account", account)
	command.Stdin = strings.NewReader(token + "\n")
	if err := command.Run(); err != nil {
		return fmt.Errorf("保存 Linux Secret Service 凭证失败")
	}
	return nil
}

func deleteSystemGitHubCredential(ctx context.Context, service, account string) error {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return fmt.Errorf("Linux Secret Service 不可用：未找到 secret-tool")
	}
	command := exec.CommandContext(ctx, "secret-tool", "clear", "service", service, "account", account)
	if err := command.Run(); err != nil && !isSecretToolNotFound(err) {
		return fmt.Errorf("删除 Linux Secret Service 凭证失败")
	}
	return nil
}

func isSecretToolNotFound(err error) bool {
	if err == nil {
		return false
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode() == 1
	}
	return false
}
