// 中文说明：会话正文搜索、目录发现进度和已复核结果的流式响应。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codex-chronicle.local/internal/application"
	copypkg "codex-chronicle.local/internal/copy"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/searchtext"
	"codex-chronicle.local/internal/store"
)

type searchStreamEvent struct {
	Type        string                       `json:"type"`
	SearchMode  string                       `json:"search_mode,omitempty"`
	Stage       domain.SearchMatchOrigin     `json:"stage,omitempty"`
	FilesSeen   int                          `json:"files_seen,omitempty"`
	FilesDone   int                          `json:"files_done,omitempty"`
	FilesTotal  int                          `json:"files_total,omitempty"`
	RecordsDone int64                        `json:"records_done,omitempty"`
	SourceBytes *int64                       `json:"source_bytes_read,omitempty"`
	PrefixBytes *int64                       `json:"prefix_bytes_read,omitempty"`
	GeneratedAt *time.Time                   `json:"snapshot_generated_at,omitempty"`
	Hits        []domain.SearchHit           `json:"hits,omitempty"`
	Items       []domain.ConversationSummary `json:"items,omitempty"`
	Limited     bool                         `json:"candidate_limit_reached,omitempty"`
	Error       string                       `json:"error,omitempty"`
}

func (s *Server) searchHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	workspace := s.currentWorkspace()
	root := workspace.Definition.Root
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读扫描根目录")
		return
	}
	values := request.URL.Query()
	query := strings.TrimSpace(values.Get("q"))
	if searchtext.Normalize(query) == "" {
		writeError(writer, http.StatusBadRequest, "搜索关键词不能为空")
		return
	}
	limit, err := parseOptionalPositiveInt(values.Get("limit"), 100, 500)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	conversationScope, err := parseConversationFilter(values, "conversation_")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	technical := values.Get("scope") == "technical"
	indexedOnly := values.Get("scope") == "indexed"
	sourceOnly := values.Get("scope") == "source"
	ctx := request.Context()
	var emit func(searchStreamEvent) error
	if values.Get("stream") == "1" {
		writer.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		encoder := json.NewEncoder(writer)
		controller := http.NewResponseController(writer)
		emit = func(event searchStreamEvent) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := encoder.Encode(event); err != nil {
				return err
			}
			return controller.Flush()
		}
		if err := emit(searchStreamEvent{Type: "catalog"}); err != nil {
			return
		}
	}
	fail := func(status int, err error) {
		if ctx.Err() != nil {
			return
		}
		if emit != nil {
			_ = emit(searchStreamEvent{Type: "error", Error: err.Error()})
		} else {
			writeError(writer, status, err.Error())
		}
	}
	snapshot, err := s.searchSnapshot(ctx, root, emit)
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}

	// 先筛选会话再扫描正文，结果上限只在当前范围内统计。
	hasScope := conversationScope != (conversationFilter{})
	scopedPaths := make(map[string]bool)
	byID := make(map[string]domain.ConversationSummary)
	if hasScope || emit != nil {
		var sizeByPath map[string]int64
		if conversationScope.MinBytes > 0 || conversationScope.MaxBytes > 0 {
			sizeByPath = fileSizeMap(snapshot.Report.Files)
		}
		for _, item := range snapshot.Report.Conversations {
			if !conversationMatchesFilter(item, conversationScope, sizeByPath) {
				continue
			}
			if emit != nil {
				byID[item.ID] = item
			}
			if hasScope {
				for _, path := range item.RolloutPaths {
					scopedPaths[path] = true
				}
			}
		}
	}
	files := make([]domain.FileInventory, 0)
	paths := make([]string, 0)
	for _, file := range snapshot.Report.Files {
		if file.Kind == domain.AssetKindJSONL && file.Rollout != nil && (!hasScope || scopedPaths[file.RelativePath]) {
			files = append(files, file)
			paths = append(paths, file.RelativePath)
		}
	}
	var filter application.SearchHitFilter
	if !technical {
		filter = effectiveSearchFilter(ctx, root, query, snapshot.Report, workspace.Archive)
	}
	mode := "live_scan"
	stage := domain.SearchMatchSource
	if indexedOnly {
		if workspace.Archive == nil {
			fail(http.StatusBadRequest, store.ErrSearchIndexNotReady)
			return
		}
		mode, stage = "indexed_only", domain.SearchMatchIndex
	} else if workspace.Archive != nil && !technical && !sourceOnly {
		// 自动搜索在索引暂不可用时仍可直接读取原文。
		scope, _ := workspace.Archive.SearchIndexScope(ctx, root)
		switch scope {
		case "all":
			mode, stage = "archive_fts", ""
		case "selected":
			mode, stage = "selected_then_source", domain.SearchMatchIndex
		}
	}
	generatedAt := snapshot.Report.GeneratedAt
	announce := func(stage domain.SearchMatchOrigin) error {
		if emit == nil {
			return nil
		}
		return emit(searchStreamEvent{Type: "search", SearchMode: mode, Stage: stage, FilesTotal: len(files), GeneratedAt: &generatedAt})
	}
	var observe application.SearchObserver
	var sourceBytes, prefixBytes int64
	if emit != nil {
		if err := announce(stage); err != nil {
			return
		}
		lastProgress := time.Now()
		observe = func(progress application.SearchProgress) error {
			sourceBytes += progress.SourceBytesRead
			prefixBytes += progress.PrefixBytesRead
			if len(progress.Hits) == 0 && progress.FilesDone != progress.FilesTotal && time.Since(lastProgress) < 150*time.Millisecond {
				return nil
			}
			lastProgress = time.Now()
			var items []domain.ConversationSummary
			for _, hit := range progress.Hits {
				if item, ok := byID[hit.ConversationID]; ok {
					items = append(items, item)
					delete(byID, hit.ConversationID)
				}
			}
			return emit(searchStreamEvent{
				Type: "search", Stage: progress.Stage, FilesDone: progress.FilesDone, FilesTotal: progress.FilesTotal, RecordsDone: progress.RecordsDone,
				Hits: progress.Hits, Items: conversationListItems(items),
			})
		}
	}
	var hits []domain.SearchHit
	switch mode {
	case "indexed_only", "selected_then_source":
		hits, err = workspace.Archive.SearchIndexOnlyObserved(ctx, root, query, limit, files, filter, observe)
		if mode == "selected_then_source" && (errors.Is(err, store.ErrSearchIndexChanged) || errors.Is(err, store.ErrSearchIndexNotReady)) {
			// 已输出的命中仍可查看；未完成文件重新从原文复核。
			filter = effectiveSearchFilter(ctx, root, query, snapshot.Report, workspace.Archive, hits...)
			err = nil
		}
		if mode == "selected_then_source" && err == nil && len(hits) < limit {
			// 自选索引先返回已有结果，原文补搜沿用相同的有效历史去重。
			if err = announce(domain.SearchMatchSource); err == nil {
				var extra []domain.SearchHit
				extra, err = application.SearchJSONLFilesObserved(ctx, root, paths, query, limit-len(hits), false, filter, observe)
				hits = append(hits, extra...)
			}
		}
	case "archive_fts":
		hits, err = workspace.Archive.SearchWithFilesObserved(ctx, root, query, limit, files, filter, observe)
	default:
		hits, err = application.SearchJSONLFilesObserved(ctx, root, paths, query, limit, technical, filter, observe)
	}
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	if emit != nil {
		_ = emit(searchStreamEvent{Type: "done", Limited: len(hits) == limit, SourceBytes: &sourceBytes, PrefixBytes: &prefixBytes})
		return
	}
	for i := range hits {
		if hits[i].SourceStatus == "" {
			hits[i].SourceStatus = domain.StatusUnknown
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only": true, "mode": copypkg.LiveRead, "search_mode": mode, "index_stale": snapshot.Metadata.Stale,
		"snapshot_status": snapshot.Metadata.Status, "snapshot_generated_at": generatedAt,
		"query": query, "hits": hits, "count": len(hits),
		"history_scope":           map[bool]string{true: "technical", false: "effective"}[technical],
		"candidate_limit_reached": len(hits) == limit,
	})
}

func (s *Server) searchSnapshot(ctx context.Context, root string, emit func(searchStreamEvent) error) (application.InventorySnapshot, error) {
	snapshot, found, err := s.tasks.Snapshot(ctx, root, true)
	if err != nil || found && !snapshot.Metadata.Stale {
		return snapshot, err
	}
	task := s.tasks.StartInventoryWithOptions(root, application.ScanOptions{})
	if task.Status == domain.TaskFailed {
		return snapshot, errors.New(task.Error)
	}
	for {
		wait := ctx
		var cancel context.CancelFunc
		if emit != nil {
			wait, cancel = context.WithTimeout(ctx, 400*time.Millisecond)
		}
		completed, waitErr := s.tasks.Wait(wait, task.ID)
		if cancel != nil {
			cancel()
		}
		if waitErr == nil {
			if completed.Status != domain.TaskCompleted {
				message := completed.Error
				if message == "" {
					message = string(completed.Status)
				}
				return snapshot, fmt.Errorf("会话目录未能更新：%s", message)
			}
			return s.tasks.ReadReport(ctx, root, application.ScanOptions{}, false)
		}
		if emit == nil || ctx.Err() != nil || !errors.Is(waitErr, context.DeadlineExceeded) {
			return snapshot, waitErr
		}
		if current, ok := s.tasks.Get(task.ID); ok {
			if err := emit(searchStreamEvent{Type: "catalog", FilesSeen: current.FilesSeen}); err != nil {
				return snapshot, err
			}
		}
	}
}
