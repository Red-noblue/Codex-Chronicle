// 中文说明：提供显式 GitHub CLI 快捷绑定与账户核验；凭证只在系统凭证库和请求内存中流动。
package api

import (
	"context"
	"net/http"

	"codex-chronicle.local/internal/archive"
)

func (state *githubSettingsState) cliAvailable() bool {
	// CLI 凭证只可发送给 GitHub 官方端点，防止自定义 API 地址接收已有账户凭证。
	return state != nil && state.editable && state.cli != nil && state.settings.OAuthBaseURL == "https://github.com" && state.settings.APIBaseURL == "https://api.github.com" && state.cli.Available()
}

func (s *Server) githubCLIAuthHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	state := s.githubSettings
	if !state.cliAvailable() || state.auth == nil {
		writeError(writer, http.StatusConflict, "服务所在设备未检测到 GitHub CLI，或当前使用自定义 GitHub 地址；请使用设备授权")
		return
	}
	token, err := state.cli.Token(request.Context())
	if err != nil {
		writeError(writer, http.StatusConflict, "无法读取 GitHub CLI 登录；请先在服务所在设备运行 gh auth login，或使用设备授权")
		return
	}
	client, err := archive.NewGitHubRepositoryClient(archive.GitHubRepositoryClientConfig{BaseURL: state.settings.APIBaseURL, HTTPClient: state.client}, token)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "无法核验 GitHub CLI 账户")
		return
	}
	account, err := client.CurrentAccount(request.Context())
	if err != nil {
		writeError(writer, http.StatusBadGateway, "GitHub CLI 账户核验失败，请检查登录、网络或代理；原绑定保持有效")
		return
	}
	if err := state.auth.SaveCredential(request.Context(), token); err != nil {
		writeError(writer, http.StatusInternalServerError, "GitHub 凭证保存失败；原绑定保持有效")
		return
	}
	s.invalidateRemotePreviews()
	writeJSON(writer, http.StatusOK, map[string]any{"source_read_only": true, "credential_available": true, "account": account})
}

func (s *Server) githubAccountHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	catalog, ok := s.githubRepositoryCatalog().(interface {
		CurrentAccount(context.Context) (archive.GitHubAccount, error)
	})
	if !ok {
		writeError(writer, http.StatusServiceUnavailable, "GitHub 账户查询未配置")
		return
	}
	account, err := catalog.CurrentAccount(request.Context())
	if err != nil {
		writeError(writer, http.StatusBadGateway, "无法核验 GitHub 账户，请检查登录、网络或代理")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"source_read_only": true, "account": account})
}
