// 中文说明：为桌面后台调度自动更新与延迟清理；可见网页和未完成任务会推迟自动重启。
package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"time"
)

const updateIdleWindow = 5 * time.Minute

func (s *Server) updateAutomaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "只支持 PUT")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "更新设置必须使用 application/json")
		return
	}
	if s.updater == nil || s.updateAutomaticRestart == nil {
		writeError(w, http.StatusConflict, "当前运行方式不支持自动更新")
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Enabled == nil {
		writeError(w, http.StatusBadRequest, "必须明确设置自动更新是否开启")
		return
	}
	if err := s.updater.SetAutomatic(r.Context(), *input.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status, err := s.updater.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) updateActivityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "页面心跳必须使用 application/json")
		return
	}
	s.drainMu.Lock()
	s.updateLastActivity = time.Now()
	s.drainMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"running_version": s.version})
}

func (s *Server) automaticUpdateIdle() bool {
	s.drainMu.Lock()
	busy := s.draining || time.Since(s.updateLastActivity) < updateIdleWindow
	s.drainMu.Unlock()
	return !busy && s.idleForShutdown()
}

// RunAutomaticUpdates 只由已发布桌面实例启动；取消后先等待下载和预检退出，才能关闭数据库。
func (s *Server) RunAutomaticUpdates(ctx context.Context, paused func() bool) {
	if s.updater == nil || s.updateAutomaticRestart == nil {
		return
	}
	s.drainMu.Lock()
	s.updateLastActivity = time.Now()
	s.drainMu.Unlock()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	idleTicker := time.NewTicker(time.Second)
	defer idleTicker.Stop()
	var stepDone <-chan struct{}
	for {
		select {
		case <-ctx.Done():
			if stepDone != nil {
				<-stepDone
			}
			return
		case <-idleTicker.C:
			// 下载可持续较久，空闲计时独立于下载推进，任务刚结束时不能立即重启。
			if !s.idleForShutdown() {
				s.drainMu.Lock()
				s.updateLastActivity = time.Now()
				s.drainMu.Unlock()
			}
		case <-stepDone:
			stepDone = nil
		case <-timer.C:
			if stepDone == nil && !paused() {
				done := make(chan struct{})
				stepDone = done
				go func() { defer close(done); s.runAutomaticUpdateStep(ctx) }()
			}
			timer.Reset(time.Minute)
		}
	}
}

func (s *Server) runAutomaticUpdateStep(ctx context.Context) {
	status, err := s.updater.Status(ctx)
	if err != nil {
		log.Printf("读取自动更新状态失败：%v", err)
		return
	}
	// 关闭自动更新不关闭已成功安装后的缓存清理。
	if status.State.ActiveVersion == s.version && (status.State.ConfirmedAt == nil || status.State.CleanupAfter != nil && !time.Now().Before(*status.State.CleanupAfter)) {
		if check, available, err := s.currentMigrationPreflight(ctx); err == nil && (!available || check.Status == "ready") && s.registry.Health().Valid {
			if _, err := s.updater.ConfirmRunning(ctx, s.version); err != nil {
				log.Printf("更新清理稍后重试：%v", err)
			}
		}
	}
	stepCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := s.updater.AutomaticStep(stepCtx, s.version, s.automaticUpdateIdle, s.updateHealthCheck(), s.updateAutomaticRestart); err != nil && ctx.Err() == nil {
		log.Printf("自动更新暂未完成，将稍后重试：%v", err)
	}
}
