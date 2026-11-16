// 中文说明：仅在用户点击快捷绑定时读取已登录的 GitHub CLI 凭证；捕获输出，不写日志或进程参数。
package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type GitHubCLI struct{}

func (GitHubCLI) Available() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// Token 只读取 github.com 的现有登录；支持 --no-refresh 的版本显式禁用令牌刷新。
func (GitHubCLI) Token(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return githubCLIToken(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "gh", args...).Output()
	})
}

func githubCLIToken(ctx context.Context, run func(context.Context, ...string) ([]byte, error)) (string, error) {
	help, err := run(ctx, "auth", "token", "--help")
	if err != nil {
		return "", fmt.Errorf("无法检查 GitHub CLI 版本，请使用设备授权")
	}
	args := []string{"auth", "token", "--hostname", "github.com"}
	if strings.Contains(string(help), "--no-refresh") {
		args = append(args, "--no-refresh")
	}
	output, err := run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("无法读取 GitHub CLI 登录；请先在服务所在设备运行 gh auth login，或使用设备授权")
	}
	token := strings.TrimSpace(string(output))
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n\t ") {
		return "", fmt.Errorf("GitHub CLI 未返回有效的登录凭证")
	}
	return token, nil
}
