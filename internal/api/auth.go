// 中文说明：提供一次性绑定、密码登录、会话撤销和访问审计 API；认证控制面与真实 Codex 资产隔离。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"codex-chronicle.local/internal/auth"
)

type authPrincipalContextKey struct{}

type authBindRequest struct {
	Code     string `json:"code"`
	Device   string `json:"device"`
	Password string `json:"password,omitempty"`
}

type authLoginRequest struct {
	Device   string `json:"device"`
	Password string `json:"password"`
}

type authRevokeRequest struct {
	SessionID string `json:"session_id"`
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/v1/auth/") {
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("Pragma", "no-cache")
		}
		if s.authManager == nil || isPublicAuthPath(request) {
			next.ServeHTTP(writer, request)
			return
		}
		token := bearerToken(request)
		principal, err := s.authManager.Authenticate(token)
		if err != nil {
			writeAuthUnauthorized(writer)
			return
		}
		contextValue := context.WithValue(request.Context(), authPrincipalContextKey{}, principal)
		next.ServeHTTP(writer, request.WithContext(contextValue))
	})
}

func isPublicAuthPath(request *http.Request) bool {
	if request.Method == http.MethodGet && request.URL.Path == "/api/v1/auth/status" {
		return true
	}
	if request.Method == http.MethodPost && request.URL.Path == "/api/v1/auth/reissue-binding" && isLoopbackRequest(request) {
		return true
	}
	if request.Method != http.MethodPost {
		return false
	}
	return request.URL.Path == "/api/v1/auth/bind" || request.URL.Path == "/api/v1/auth/login"
}

func isLoopbackRequest(request *http.Request) bool {
	remote := strings.TrimSpace(request.RemoteAddr)
	if remote == "" {
		return true
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func bearerToken(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, prefix))
}

func writeAuthUnauthorized(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="codex-chronicle"`)
	writeJSON(writer, http.StatusUnauthorized, map[string]any{"read_only": true, "authenticated": false, "error": "需要有效的会话令牌"})
}

func (s *Server) authStatusHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "enabled": false, "error": "远程认证未启用"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "auth": s.authManager.Status()})
}

func (s *Server) authBindHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "远程认证未启用"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
	var input authBindRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "绑定请求不是有效 JSON")
		return
	}
	if strings.TrimSpace(input.Password) == "" {
		writeError(writer, http.StatusBadRequest, "首次绑定必须设置本地账户密码")
		return
	}
	session, err := s.authManager.Bind(auth.BindRequest{Code: input.Code, Device: input.Device, Password: input.Password, Remote: request.RemoteAddr})
	if err != nil {
		writeAuthError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"read_only": true, "bound": true, "session": session})
}

func (s *Server) authLoginHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "远程认证未启用"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
	var input authLoginRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "登录请求不是有效 JSON")
		return
	}
	session, err := s.authManager.Login(auth.LoginRequest{Device: input.Device, Password: input.Password, Remote: request.RemoteAddr})
	if err != nil {
		writeAuthError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "authenticated": true, "session": session})
}

func (s *Server) authLogoutHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "远程认证未启用"})
		return
	}
	if err := s.authManager.RevokeToken(bearerToken(request), request.RemoteAddr); err != nil {
		writeAuthError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "authenticated": false, "revoked": true})
}

func (s *Server) authSessionsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "远程认证未启用"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "sessions": s.authManager.Sessions()})
}

func (s *Server) authSessionRevokeHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "远程认证未启用"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	var input authRevokeRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "撤销请求不是有效 JSON")
		return
	}
	if err := s.authManager.RevokeSession(input.SessionID, request.RemoteAddr); err != nil {
		writeAuthError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "revoked": true})
}

func (s *Server) authAuditHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "远程认证未启用"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "audit": s.authManager.Audit(100)})
}

func (s *Server) authResetBindingHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "远程认证未启用"})
		return
	}
	if !isLoopbackRequest(request) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "重置绑定码只能由本机回环请求执行"})
		return
	}
	bootstrap, err := s.authManager.ResetBinding()
	if err != nil {
		writeAuthError(writer, err)
		return
	}
	response := map[string]any{"read_only": true, "bound": false, "expires_at": bootstrap.ExpiresAt}
	if bootstrap.Code != "" {
		response["binding_code"] = bootstrap.Code
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) authReissueBindingHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.authManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "远程认证未启用"})
		return
	}
	if !isLoopbackRequest(request) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "重新生成绑定码只能由本机回环请求执行"})
		return
	}
	bootstrap, err := s.authManager.ReissueBinding()
	if err != nil {
		writeAuthError(writer, err)
		return
	}
	response := map[string]any{"read_only": true, "bound": false, "binding_code": bootstrap.Code, "expires_at": bootstrap.ExpiresAt}
	writeJSON(writer, http.StatusOK, response)
}

func writeAuthError(writer http.ResponseWriter, err error) {
	status := http.StatusUnauthorized
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		status = http.StatusTooManyRequests
	case errors.Is(err, auth.ErrAlreadyBound), errors.Is(err, auth.ErrSessionNotFound), errors.Is(err, auth.ErrBindingStillValid):
		status = http.StatusConflict
	case errors.Is(err, auth.ErrInvalidDevice):
		status = http.StatusBadRequest
	}
	writeJSON(writer, status, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
}
