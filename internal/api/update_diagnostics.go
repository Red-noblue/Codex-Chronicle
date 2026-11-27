// 中文说明：提供只读、脱敏的更新诊断包下载；沿用现有认证边界，不上传数据，不依赖状态文件完整。
package api

import (
	"net/http"
	"strconv"

	updatepkg "codex-chronicle.local/internal/update"
)

func (s *Server) updateDiagnosticsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	if s.updater == nil {
		writeError(w, http.StatusConflict, "当前服务未配置安装目录，请使用安装版启动器的 --export-diagnostics 离线导出")
		return
	}
	archive, err := updatepkg.BuildDiagnostics(r.Context(), s.updater.Root(), s.version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "诊断包生成失败，请尝试启动器离线导出")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+archive.Filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(archive.Data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Chronicle-Diagnostic-ID", archive.ID)
	_, _ = w.Write(archive.Data)
}
