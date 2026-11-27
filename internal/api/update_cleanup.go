// 中文说明：提供更新文件保留时长设置，修改当前清理期限但不在 HTTP 请求中执行删除。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	updatepkg "codex-chronicle.local/internal/update"
)

func (s *Server) updateCleanupSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "只支持 PUT")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "清理设置必须使用 application/json")
		return
	}
	if s.updater == nil {
		writeError(w, http.StatusConflict, "当前服务未配置独立更新管理器")
		return
	}
	var input struct {
		RetentionMinutes *int `json:"retention_minutes"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.RetentionMinutes == nil {
		writeError(w, http.StatusBadRequest, "必须明确设置旧版本保留分钟数")
		return
	}
	if err := s.updater.SetCleanupRetention(r.Context(), *input.RetentionMinutes); err != nil {
		code := http.StatusConflict
		if errors.Is(err, updatepkg.ErrInvalidCleanupRetention) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err.Error())
		return
	}
	status, err := s.updater.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}
