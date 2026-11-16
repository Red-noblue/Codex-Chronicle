// 中文说明：用模拟 CLI 和本机 GitHub 服务核验快捷绑定、账户身份、重启恢复与失败边界，不访问真实凭证。
package api

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/archive"
)

type simulatedGitHubCLI struct {
	calls int
	token string
}

func (*simulatedGitHubCLI) Available() bool { return true }
func (cli *simulatedGitHubCLI) Token(context.Context) (string, error) {
	cli.calls++
	return cli.token, nil
}

type githubRedirectTransport struct{ target *url.URL }

func (transport githubRedirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL.Scheme, copy.URL.Host = transport.target.Scheme, transport.target.Host
	return http.DefaultTransport.RoundTrip(copy)
}

func TestGitHubCLIConnectionWithoutOAuthApplication(t *testing.T) {
	remote := newSettingsGitHubServer(t)
	endpoint, _ := url.Parse(remote.URL)
	path := filepath.Join(t.TempDir(), "github.json")
	selection, err := archive.NewFileGitHubArchiveSelectionStore(filepath.Join(t.TempDir(), "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	tokens := archive.NewMemoryGitHubTokenStore()
	server := NewServer(t.TempDir())
	t.Cleanup(server.Close)
	if err := server.SetGitHubSettings(path, GitHubSettings{}, tokens, "test", selection); err != nil {
		t.Fatal(err)
	}
	cli := &simulatedGitHubCLI{token: "runtime-token"}
	server.githubSettings.cli = cli
	server.githubSettings.client.Transport = githubRedirectTransport{endpoint}
	handler := server.Handler()
	status := githubSettingsRequest(t, handler, "GET", "/api/v1/github/auth/status", nil, 200)
	if !strings.Contains(status.Body.String(), `"device_flow_available":false`) || !strings.Contains(status.Body.String(), `"cli_available":true`) || cli.calls != 0 {
		t.Fatal("读取状态不得读取 CLI 登录，且应区分设备授权与快捷绑定")
	}
	githubSettingsRequest(t, handler, "POST", "/api/v1/github/auth/start", nil, 503)
	bound := githubSettingsRequest(t, handler, "POST", "/api/v1/github/auth/cli", nil, 200)
	if !strings.Contains(bound.Body.String(), `"login":"owner"`) || cli.calls != 1 {
		t.Fatal("快捷绑定必须先核验账户，再返回公开身份")
	}
	githubSettingsRequest(t, handler, "GET", "/api/v1/github/account", nil, 200)
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/archive-target", map[string]string{"owner": "owner", "repository": "repo"}, 200)
	// 无效新登录不会覆盖已保存的有效凭证。
	cli.token = "invalid-token"
	githubSettingsRequest(t, handler, "POST", "/api/v1/github/auth/cli", nil, 502)
	githubSettingsRequest(t, handler, "GET", "/api/v1/github/account", nil, 200)
	server.remoteConfigMu.RLock()
	githubSettingsRequest(t, handler, "POST", "/api/v1/github/auth/cli", nil, 409)
	server.remoteConfigMu.RUnlock()
	if cli.calls != 2 {
		t.Fatal("处理任务期间不应读取外部凭证")
	}
	restarted := NewServer(t.TempDir())
	t.Cleanup(restarted.Close)
	if err := restarted.SetGitHubSettings(path, GitHubSettings{}, tokens, "test", selection); err != nil {
		t.Fatal(err)
	}
	status = githubSettingsRequest(t, restarted.Handler(), "GET", "/api/v1/github/auth/status", nil, 200)
	if !strings.Contains(status.Body.String(), `"credential_available":true`) {
		t.Fatal("重启后应恢复凭证")
	}
	target := githubSettingsRequest(t, restarted.Handler(), "GET", "/api/v1/github/archive-target", nil, 200)
	if !strings.Contains(target.Body.String(), `"full_name":"owner/repo"`) {
		t.Fatal("重启后应恢复仓库选择")
	}
	// 切换到自定义 API 后，不能把 gh 的 github.com 凭证交给新端点。
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/settings", GitHubSettings{OAuthBaseURL: remote.URL, APIBaseURL: remote.URL}, 200)
	githubSettingsRequest(t, handler, "POST", "/api/v1/github/auth/cli", nil, 409)
	if cli.calls != 2 {
		t.Fatal("自定义端点不得读取 CLI 凭证")
	}
}

func TestGitHubReleaseDefaultsEnableDeviceFlowWithoutSavedSettings(t *testing.T) {
	server := NewServer(t.TempDir())
	t.Cleanup(server.Close)
	if err := server.SetGitHubSettings(filepath.Join(t.TempDir(), "settings.json"), GitHubSettings{ClientID: "public-release-client"}, archive.NewMemoryGitHubTokenStore(), "test", nil); err != nil {
		t.Fatal(err)
	}
	status := githubSettingsRequest(t, server.Handler(), "GET", "/api/v1/github/auth/status", nil, 200)
	if !strings.Contains(status.Body.String(), `"device_flow_available":true`) || strings.Contains(status.Body.String(), `"credential_available":true`) {
		t.Fatal("发行包默认值应直接开放设备授权，但不能伪装为已登录")
	}
}
