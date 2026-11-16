// 中文说明：退出前停止接收新工作，并等待已有请求、会话操作和后台任务完成，避免安装更新中断数据处理。
package api

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func (s *Server) drainMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		s.drainMu.Lock()
		if s.draining {
			s.drainMu.Unlock()
			w.Header().Set("Retry-After", "2")
			writeError(w, http.StatusServiceUnavailable, "后台正在等待任务完成并退出，请稍后重新打开应用")
			return
		}
		// SSE 连接只订阅状态，不能使退出无限等待；HTTP 关闭阶段会结束这些连接。
		tracked := !strings.HasSuffix(r.URL.Path, "/events")
		if tracked {
			s.activeRequests++
		}
		s.drainMu.Unlock()
		if tracked {
			defer func() {
				s.drainMu.Lock()
				s.activeRequests--
				s.drainMu.Unlock()
			}()
		}
		next.ServeHTTP(w, r)
	})
}

// Drain 一旦开始便拒绝新任务；调用方超时不会取消或强杀已有的数据任务。
func (s *Server) Drain(ctx context.Context) error {
	s.drainMu.Lock()
	s.draining = true
	s.drainMu.Unlock()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.idleForShutdown() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) idleForShutdown() bool {
	s.drainMu.Lock()
	requests := s.activeRequests
	s.drainMu.Unlock()
	if requests != 0 {
		return false
	}
	s.executionMu.Lock()
	executing := s.executionActive != ""
	s.executionMu.Unlock()
	if executing {
		return false
	}
	s.cloudMu.Lock()
	cloud := s.cloud
	s.cloudMu.Unlock()
	if cloud != nil {
		cloud.mu.Lock()
		busy := cloud.active != ""
		cloud.mu.Unlock()
		if busy {
			return false
		}
	}
	return s.copyTasks.ActiveCount() == 0 && s.liveCopyTasks.ActiveCount() == 0 &&
		s.summaryTasks.ActiveCount() == 0 && s.tasks.ActiveCount() == 0
}

// SetVersion 在开始监听前设置健康检查中的实际构建版本。
func (s *Server) SetVersion(version string) { s.version = version }

// CloseWorkspaceStores 只在 HTTP 和后台任务全部结束后调用，确保释放实例锁前已关闭所有项目数据库连接。
func (s *Server) CloseWorkspaceStores() {
	s.workspaceMu.Lock()
	defer s.workspaceMu.Unlock()
	for _, database := range s.workspaceArchives {
		if database != nil {
			_ = database.Close()
		}
	}
}
