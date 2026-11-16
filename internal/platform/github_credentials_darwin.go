//go:build darwin

// 中文说明：通过 macOS security 命令访问用户默认钥匙串；令牌通过标准输入传递，不放入进程参数。
package platform

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"codex-chronicle.local/internal/archive"
)

func loadSystemGitHubCredential(ctx context.Context, service, account string) (string, error) {
	command := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w")
	output, err := command.Output()
	if err != nil {
		if isMacOSCredentialNotFound(err) {
			return "", archive.ErrGitHubCredentialNotFound
		}
		return "", fmt.Errorf("读取 macOS Keychain 凭证失败")
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", archive.ErrGitHubCredentialNotFound
	}
	return token, nil
}

func saveSystemGitHubCredential(ctx context.Context, service, account, token string) error {
	// security 的密码提示不读取管道输入；改用交互命令流，并将密码编码为十六进制参数。
	// 命令流仅通过 stdin 传入，避免凭证进入进程参数或被命令解析器解释。
	quote := func(value string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
	}
	command := exec.CommandContext(ctx, "security", "-i")
	command.Stdin = strings.NewReader("add-generic-password -U -s " + quote(service) + " -a " + quote(account) + " -X " + hex.EncodeToString([]byte(token)) + "\n")
	if err := command.Run(); err != nil {
		return fmt.Errorf("保存 macOS Keychain 凭证失败")
	}
	stored, err := loadSystemGitHubCredential(ctx, service, account)
	if err != nil || stored != token {
		return fmt.Errorf("macOS Keychain 凭证写入校验失败")
	}
	return nil
}

func deleteSystemGitHubCredential(ctx context.Context, service, account string) error {
	command := exec.CommandContext(ctx, "security", "delete-generic-password", "-s", service, "-a", account)
	if err := command.Run(); err != nil && !isMacOSCredentialNotFound(err) {
		return fmt.Errorf("删除 macOS Keychain 凭证失败")
	}
	return nil
}

func isMacOSCredentialNotFound(err error) bool {
	if err == nil {
		return false
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 44 {
		return true
	}
	return false
}
