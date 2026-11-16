// 中文说明：模拟 GitHub CLI 命令输出，验证新旧版本兼容、固定主机和错误脱敏；不读取真实登录。
package platform

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestGitHubCLITokenHandlesVersionsAndHidesFailures(t *testing.T) {
	for _, help := range []string{"--hostname --user", "--hostname --no-refresh"} {
		t.Run(help, func(t *testing.T) {
			calls := 0
			token, err := githubCLIToken(t.Context(), func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					if strings.Join(args, " ") != "auth token --help" {
						t.Fatal("应先检查参数支持")
					}
					return []byte(help), nil
				}
				want := "auth token --hostname github.com"
				if strings.Contains(help, "--no-refresh") {
					want += " --no-refresh"
				}
				if strings.Join(args, " ") != want {
					t.Fatalf("不正确的 CLI 调用: %v", args)
				}
				return []byte("runtime-token\n"), nil
			})
			if err != nil || token != "runtime-token" || calls != 2 {
				t.Fatal("未能读取模拟 CLI 凭证")
			}
		})
	}
	for _, output := range []string{"", "two\nlines", "two tokens", strings.Repeat("x", 4097)} {
		_, err := githubCLIToken(t.Context(), func(_ context.Context, args ...string) ([]byte, error) {
			if args[len(args)-1] == "--help" {
				return []byte("--hostname"), nil
			}
			return []byte(output), nil
		})
		if err == nil {
			t.Fatal("应拒绝无效凭证输出")
		}
	}
	_, err := githubCLIToken(t.Context(), func(context.Context, ...string) ([]byte, error) {
		return []byte("runtime-token"), fmt.Errorf("runtime-token")
	})
	if err == nil || strings.Contains(err.Error(), "runtime-token") {
		t.Fatal("错误不得包含命令输出或凭证")
	}
}
