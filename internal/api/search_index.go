// 中文说明：提供当前工作区搜索索引预算、后台增量构建、停止和显式清理入口；所有写入限于本项目派生数据库。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/searchtext"
	"codex-chronicle.local/internal/store"
)

type searchIndexTask struct {
	Root      string                    `json:"root"`
	Status    string                    `json:"status"`
	Phase     string                    `json:"phase"`
	Progress  store.SearchIndexProgress `json:"progress"`
	Error     string                    `json:"error,omitempty"`
	StartedAt time.Time                 `json:"started_at"`
}

func (s *Server) searchIndexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	action := strings.TrimPrefix(r.URL.Path, "/api/v1/search-index")
	if action != "" && action != "/build" && action != "/stop" && action != "/clear" && action != "/compact" {
		writeError(w, http.StatusNotFound, "索引操作不存在")
		return
	}
	if (action == "" && r.Method != http.MethodGet && r.Method != http.MethodPut) || (action != "" && r.Method != http.MethodPost) {
		writeError(w, http.StatusMethodNotAllowed, "不支持此索引请求方法")
		return
	}
	workspace := s.currentWorkspace()
	if workspace.Archive == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "error": "当前工作区未配置独立目录数据库"})
		return
	}
	if r.Method != http.MethodGet {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var input struct {
			Root     string                    `json:"root"`
			Settings store.SearchIndexSettings `json:"settings"`
			Confirm  bool                      `json:"confirm"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请求不是有效 JSON")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeError(w, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
			return
		}
		if input.Root != workspace.Definition.Root {
			writeError(w, http.StatusConflict, "工作区已变化，请刷新索引设置后重试")
			return
		}
		s.indexTaskMu.Lock()
		current := s.currentWorkspace()
		if current.Archive != workspace.Archive || current.Definition.Root != input.Root {
			s.indexTaskMu.Unlock()
			writeError(w, http.StatusConflict, "工作区已变化，请重新读取索引设置")
			return
		}
		busy := s.indexTaskCancel != nil
		if action == "/stop" && r.Method == http.MethodPost {
			if busy && s.indexTask.Root == input.Root {
				s.indexTaskCancel()
			}
			s.indexTaskMu.Unlock()
			s.writeSearchIndexStatus(w, r, workspace)
			return
		}
		if busy {
			s.indexTaskMu.Unlock()
			writeError(w, http.StatusConflict, "索引任务正在进行，请先停止或等待完成")
			return
		}
		if action == "/build" && r.Method == http.MethodPost {
			if _, err := s.checkResources(r.Context()); err != nil {
				s.indexTaskMu.Unlock()
				writeError(w, http.StatusInsufficientStorage, err.Error())
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			s.indexTaskCancel = cancel
			s.indexTaskDone = make(chan struct{})
			s.indexTask = searchIndexTask{Root: input.Root, Status: "running", Phase: "scan", StartedAt: time.Now().UTC()}
			done := s.indexTaskDone
			s.indexTaskMu.Unlock()
			go s.runSearchIndex(ctx, workspace, done)
			s.writeSearchIndexStatus(w, r, workspace)
			return
		}
		// 设置/清理的短临界区防止同一 API 在构建启动时发生竞态。
		var err error
		switch {
		case action == "" && r.Method == http.MethodPut:
			err = workspace.Archive.SetSearchIndexSettings(r.Context(), input.Settings)
		case action == "/clear" && r.Method == http.MethodPost:
			if !input.Confirm {
				s.indexTaskMu.Unlock()
				writeError(w, http.StatusBadRequest, "请先确认仅清理搜索加速数据")
				return
			}
			err = workspace.Archive.ClearSearchIndex(r.Context())
			s.indexTask = searchIndexTask{}
		case action == "/compact" && r.Method == http.MethodPost:
			err = workspace.Archive.CompactSearchIndex(r.Context())
		default:
			s.indexTaskMu.Unlock()
			writeError(w, http.StatusMethodNotAllowed, "不支持此索引操作")
			return
		}
		s.indexTaskMu.Unlock()
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.writeSearchIndexStatus(w, r, workspace)
}

func (s *Server) writeSearchIndexStatus(w http.ResponseWriter, r *http.Request, workspace workspaceSnapshot) {
	// 先记录任务状态：若读取用量时任务结束，仍提示客户端再轮询一次，以取得最终统计。
	s.indexTaskMu.Lock()
	task := s.indexTask
	busy := s.indexTaskCancel != nil
	s.indexTaskMu.Unlock()
	root, ctx := workspace.Definition.Root, r.Context()
	snapshot, found, err := s.tasks.Snapshot(ctx, root, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var status store.SearchIndexStatus
	if found && !snapshot.Metadata.Stale {
		files := make([]domain.FileInventory, 0, len(snapshot.Report.Conversations))
		for _, file := range snapshot.Report.Files {
			if file.Rollout != nil && file.Kind == domain.AssetKindJSONL {
				files = append(files, file)
			}
		}
		status, err = workspace.Archive.SearchIndexStatusWithFiles(ctx, root, files)
	} else {
		status, err = workspace.Archive.SearchIndexStatus(ctx, root)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task.Root != workspace.Definition.Root && !busy {
		task = searchIndexTask{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true, "source_read_only": true, "root": workspace.Definition.Root, "index": status,
		"task": task, "busy": busy, "content_tree": searchtext.ContentTree, "compact_content": searchtext.CompactContent,
	})
}

func (s *Server) runSearchIndex(ctx context.Context, workspace workspaceSnapshot, done chan struct{}) {
	var progress store.SearchIndexProgress
	report, err := s.scanner.ScanWithoutGraphs(ctx, workspace.Definition.Root)
	if err == nil {
		s.indexTaskMu.Lock()
		s.indexTask.Phase = "index"
		s.indexTaskMu.Unlock()
		_, err = workspace.Archive.SyncInventoryWithoutSearch(ctx, report)
	}
	if err == nil {
		progress, err = workspace.Archive.BuildSearchIndex(ctx, report, func(p store.SearchIndexProgress) {
			s.indexTaskMu.Lock()
			s.indexTask.Progress = p
			s.indexTaskMu.Unlock()
		})
	}
	s.indexTaskMu.Lock()
	s.indexTask.Progress = progress
	switch {
	case errors.Is(err, context.Canceled):
		s.indexTask.Status = "paused"
	case err != nil:
		s.indexTask.Status = "failed"
		s.indexTask.Error = fmt.Sprint(err)
	case progress.BudgetReached:
		s.indexTask.Status = "budget_reached"
	default:
		s.indexTask.Status = "completed"
	}
	s.indexTaskCancel = nil
	close(done)
	s.indexTaskMu.Unlock()
}
