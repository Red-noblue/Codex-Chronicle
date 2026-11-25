// 中文说明：提供独立安装根的离线更新预检、切换、回滚和状态查询；不会修改真实 .codex。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

type updatePrepareRequest struct {
	BundleRoot     string `json:"bundle_root"`
	BundlePath     string `json:"bundle_path,omitempty"`
	ArchivePath    string `json:"archive_path,omitempty"`
	AllowDowngrade bool   `json:"allow_downgrade"`
}

type updatePlanRequest struct {
	PlanID  string `json:"plan_id"`
	Restart bool   `json:"restart,omitempty"`
}

type updateProxyRequest struct {
	ProxyURL string `json:"proxy_url"`
}

func (s *Server) updateProxyHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 PUT")
		return
	}
	if s.updater == nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"available": false, "error": "当前服务未配置独立更新管理器"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	var input updateProxyRequest
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&input); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		writeError(writer, http.StatusBadRequest, "代理设置必须是有效 JSON")
		return
	}
	if err := s.updater.SetOnlineProxy(input.ProxyURL); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	status, err := s.updater.Status(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"available": true, "status": status})
}

func (s *Server) updateStatusHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	if s.updater == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"available": false, "read_only": true, "reason": "daemon 未配置独立安装根"})
		return
	}
	status, err := s.updater.Status(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"available": true, "read_only": true, "status": status, "running_version": s.version, "restart_available": s.updateRestart != nil, "restart_required": status.State.ActiveVersion != "" && status.State.ActiveVersion != s.version})
}

// updateCheckHandler 只读取配置的在线 feed，不下载或切换版本。
func (s *Server) updateCheckHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	if s.updater == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"available": false, "configured": false, "read_only": true, "reason": "daemon 未配置独立安装根"})
		return
	}
	check, err := s.updater.CheckOnline(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{"available": false, "configured": true, "read_only": true, "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"available": check.Available, "configured": true, "read_only": true, "check": check})
}

// updateDownloadHandler 下载 feed 选出的最新版本，但不会预检、安装或切换版本。
func (s *Server) updateDownloadHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.updater == nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "daemon 未配置独立安装根"})
		return
	}
	check, err := s.updater.CheckOnline(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	if !check.Available || check.Release == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "blocked": false, "available": false, "check": check})
		return
	}
	downloaded, err := s.updater.DownloadOnline(request.Context(), *check.Release)
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{"read_only": true, "blocked": true, "check": check, "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "check": check, "download": downloaded})
}

// updatePrepareOnlineHandler 下载并生成待人工确认的离线更新计划；不会切换当前版本。
func (s *Server) updatePrepareOnlineHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.updater == nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "daemon 未配置独立安装根"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
	var input updatePrepareRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil && err != io.EOF {
		writeError(writer, http.StatusBadRequest, "在线更新预检请求必须是有效 JSON")
		return
	}
	plan, downloaded, err := s.updater.PrepareOnline(request.Context(), input.AllowDowngrade)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "plan": plan, "download": downloaded})
}

func (s *Server) updatePrepareHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.updater == nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "daemon 未配置独立安装根"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input updatePrepareRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "bundle_path（兼容 bundle_root）或 archive_path 必须是非空路径，且请求必须是有效 JSON")
		return
	}
	path := strings.TrimSpace(input.BundlePath)
	if path == "" {
		path = strings.TrimSpace(input.BundleRoot)
	}
	if strings.TrimSpace(input.ArchivePath) != "" {
		if path != "" {
			writeError(writer, http.StatusBadRequest, "bundle_path/bundle_root 与 archive_path 只能选择一个")
			return
		}
		path = strings.TrimSpace(input.ArchivePath)
	}
	if path == "" {
		writeError(writer, http.StatusBadRequest, "bundle_path（兼容 bundle_root）或 archive_path 必须是非空路径，且请求必须是有效 JSON")
		return
	}
	plan, err := s.updater.PreparePath(request.Context(), path, input.AllowDowngrade)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "plan": plan})
}

func (s *Server) updateActivateHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.updater == nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "daemon 未配置独立安装根"})
		return
	}
	input, ok := decodeUpdatePlanRequest(writer, request)
	if !ok {
		return
	}
	if input.Restart && s.updateRestart == nil {
		writeError(writer, http.StatusConflict, "当前运行方式不支持应用内重启，请选择稍后重启")
		return
	}
	result, err := s.updater.ActivateByID(request.Context(), input.PlanID, s.updateHealthCheck())
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "source_read_only": true, "error": err.Error()})
		return
	}
	response := map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "restart_required": true, "result": result}
	if input.Restart {
		if err := s.updateRestart(request.Context(), result.ActiveVersion); err != nil {
			response["restart_error"] = "更新已准备好，但未能自动重启：" + err.Error()
		} else {
			response["restart_scheduled"] = true
		}
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) updateRestartHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.updater == nil || s.updateRestart == nil {
		writeError(writer, http.StatusConflict, "当前运行方式不支持应用内重启")
		return
	}
	status, err := s.updater.Status(request.Context())
	if err != nil || status.State.ActiveVersion == "" || status.State.ActiveVersion == s.version || status.RecoveryRequired {
		writeError(writer, http.StatusConflict, "没有已准备好且等待重启的版本")
		return
	}
	if err := s.updateRestart(request.Context(), status.State.ActiveVersion); err != nil {
		writeError(writer, http.StatusConflict, "未能安排重启："+err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"restart_scheduled": true, "version": status.State.ActiveVersion})
}

func (s *Server) updateRollbackHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.updater == nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "daemon 未配置独立安装根"})
		return
	}
	result, err := s.updater.Rollback(request.Context(), s.updateHealthCheck())
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "source_read_only": true, "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "restart_required": true, "result": result})
}

func decodeUpdatePlanRequest(writer http.ResponseWriter, request *http.Request) (updatePlanRequest, bool) {
	request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
	var input updatePlanRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || strings.TrimSpace(input.PlanID) == "" {
		writeError(writer, http.StatusBadRequest, "plan_id 必须是非空值，且请求必须是有效 JSON")
		return updatePlanRequest{}, false
	}
	return input, true
}
