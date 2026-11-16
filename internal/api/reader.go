// 中文说明：提供会话阅读、定位和可取消的全文搜索；所有来源严格来自当前工作区库存。
package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
	"codex-chronicle.local/internal/searchtext"
)

func (s *Server) conversationReaderHandler(w http.ResponseWriter, r *http.Request, encodedID string, search bool) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	id, err := url.PathUnescape(encodedID)
	if err != nil || strings.TrimSpace(id) == "" {
		writeError(w, 400, "对话 ID 无效")
		return
	}
	report, re := s.readOnlyReportWithOptionsAndFreshness(r, application.ScanOptions{}, true)
	if re.status != 0 {
		writeError(w, re.status, re.message)
		return
	}
	var target *domain.ConversationSummary
	for i := range report.report.Conversations {
		if report.report.Conversations[i].ID == id {
			target = &report.report.Conversations[i]
			break
		}
	}
	if target == nil {
		writeError(w, 404, "未找到该对话")
		return
	}
	var sources []reader.Source
	for _, file := range report.report.Files {
		if file.Kind != domain.AssetKindJSONL {
			continue
		}
		if file.Rollout == nil {
			continue
		}
		path, err := application.ResolveReadOnlyPath(report.report.Root, file.RelativePath)
		if err == nil {
			sources = append(sources, reader.Source{Relative: file.RelativePath, Path: path, ID: file.Rollout.SourceID})
		}
	}
	query := r.URL.Query()
	var position reader.Position
	if raw := query.Get("cursor"); raw != "" {
		position, err = reader.DecodePosition(raw)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	selected := query.Get("source")
	if selected == "" {
		selected = position.Source
	}
	plan := reader.BuildPlanForSource(r.Context(), sources, target.RolloutPaths, selected)
	if query.Get("probe") == "true" {
		writeJSON(w, 200, map[string]any{"source_stamp": reader.Stamp(plan), "complete": plan.Complete, "diagnostics": plan.Diagnostics})
		return
	}
	archive := s.currentWorkspace().Archive
	var cache reader.ProjectionCache
	if archive != nil {
		cache = archive
	}
	projection, progress, buildErr := reader.DefaultManager.Prepare(r.Context(), plan, cache, 200*time.Millisecond)
	if buildErr != nil {
		writeError(w, 409, buildErr.Error())
		return
	}
	if projection == nil {
		var total int64
		for _, segment := range plan.Segments {
			total += segment.End
		}
		writeJSON(w, http.StatusAccepted, reader.Page{Items: []domain.ConversationEvent{}, Indexing: true, IndexedBytes: progress, TotalBytes: total, Source: plan.Leaf, Versions: plan.Versions, Diagnostics: plan.Diagnostics})
		return
	}
	plan.Projection = projection
	if overview := query.Get("overview"); overview != "" {
		if overview != "turns" && overview != "files" {
			writeError(w, 400, "目录类型无效")
			return
		}
		offset, e := strconv.Atoi(query.Get("offset"))
		if query.Get("offset") == "" {
			offset = 0
			e = nil
		}
		if e != nil || offset < 0 {
			writeError(w, 400, "目录分页无效")
			return
		}
		out := projection.Overview(offset, 100, overview == "files")
		out["source_stamp"], out["source"] = reader.Stamp(plan), plan.Leaf
		writeJSON(w, 200, out)
		return
	}
	locator := reader.FrameLocator(func(path string, offset int64, previous int) (int64, uint64, error) {
		if archive != nil {
			if start, line, err := archive.ReaderFrame(r.Context(), report.report.Root, path, offset, previous); err == nil && (start > 0 || offset == 0) {
				return start, line, nil
			}
		}
		for _, source := range plan.Segments {
			if source.Relative == path {
				return reader.LocateBefore(source.Source, offset, previous+1)
			}
		}
		return 0, 0, fmt.Errorf("来源不属于当前会话")
	})
	if query.Get("path") != "" {
		position.Path = query.Get("path")
		position.Offset, err = strconv.ParseInt(query.Get("start"), 10, 64)
		if err != nil || position.Offset < 0 {
			writeError(w, 400, "消息定位无效")
			return
		}
		if locator != nil {
			if off, line, e := locator(position.Path, position.Offset, 0); e == nil && off == position.Offset {
				position.Line = line
			}
		}
	}
	if query.Get("tail") == "true" && len(plan.Segments) > 0 {
		last := plan.Segments[len(plan.Segments)-1]
		position = reader.Position{Path: last.Relative}
		if locator != nil {
			if off, line, e := locator(last.Relative, last.End, 60); e == nil {
				position.Offset = off
				position.Line = line
			}
		}
	}
	if query.Get("before") == "true" && locator != nil {
		if off, line, e := locator(position.Path, position.Offset, 60); e == nil {
			position.Offset = off
			position.Line = line
		}
	}
	if search {
		needle := searchtext.Normalize(query.Get("q"))
		role := query.Get("role")
		if needle == "" || len(needle) > 4096 || (role != "" && role != "user" && role != "assistant") {
			writeError(w, 400, "请输入关键词并选择有效角色")
			return
		}
		// 先用现有磁盘索引给出经过原文复核的快速命中；后续游标仍扫描全文，
		// 因为派生索引可能尚未覆盖活动尾部或被截断的超长消息。
		if archive != nil && query.Get("cursor") == "" && len(plan.Segments) > 0 {
			allowed := map[string]bool{}
			paths := []string{}
			for _, segment := range plan.Segments {
				allowed[segment.Relative] = true
				paths = append(paths, segment.Relative)
			}
			if candidates, e := archive.ReaderSearchCandidates(r.Context(), report.report.Root, paths, query.Get("q")); e == nil {
				fast := reader.Page{Items: []domain.ConversationEvent{}, Complete: plan.Complete, Diagnostics: plan.Diagnostics, HasMore: true, SourceStamp: reader.Stamp(plan), Source: plan.Leaf, Mainline: plan.Mainline, Versions: plan.Versions, Next: reader.EncodePosition(reader.Position{Path: plan.Segments[0].Relative, Source: plan.Leaf, ProjectionStamp: projection.Revision})}
				for _, candidate := range candidates {
					if !allowed[candidate.RelativePath] {
						continue
					}
					page, e := reader.ReadPage(r.Context(), plan, reader.Position{Path: candidate.RelativePath, Offset: candidate.StartOffset, Line: candidate.Line - 1}, 1, locator, true)
					if e != nil {
						continue
					}
					fast.BytesRead += page.BytesRead
					for _, event := range page.Items {
						if event.StartOffset == candidate.StartOffset && event.RelativePath == candidate.RelativePath && (role == "" || event.Role == role) && event.Kind != "injected_context" && strings.Contains(searchtext.Normalize(reader.SearchableText(event)), needle) {
							event.Text = readerSearchSnippet(reader.SearchableText(event), needle)
							fast.Items = append(fast.Items, event)
						}
					}
					if len(fast.Items) >= 12 || fast.BytesRead >= reader.PageReadBytes {
						break
					}
				}
				if len(fast.Items) > 0 {
					writeJSON(w, 200, fast)
					return
				}
			}
		}
		// 每次只扫描有限窗口；无命中也返回继续游标，不把未完成搜索伪装成空结果。
		page, e := reader.ReadPage(r.Context(), plan, position, 80, locator, true)
		if e != nil {
			writeError(w, 409, e.Error())
			return
		}
		matches := make([]domain.ConversationEvent, 0)
		for _, event := range page.Items {
			if role != "" && event.Role != role {
				continue
			}
			if event.Kind == "injected_context" {
				continue
			}
			if strings.Contains(searchtext.Normalize(reader.SearchableText(event)), needle) {
				event.Text = readerSearchSnippet(reader.SearchableText(event), needle)
				matches = append(matches, event)
			}
		}
		page.Items = matches
		writeJSON(w, 200, page)
		return
	}
	full := query.Get("full") == "true"
	limit := 80
	if full {
		limit = 1
	}
	page, e := reader.ReadPage(r.Context(), plan, position, limit, locator, full)
	if e != nil {
		writeError(w, 409, e.Error())
		return
	}
	if expected := query.Get("anchor"); expected != "" && (len(page.Items) == 0 || page.Items[0].Anchor != expected) {
		writeError(w, 409, "消息锚点已变化，请重新搜索或加载")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, page)
}

func readerSearchSnippet(text, needle string) string {
	value := searchtext.Normalize(text)
	at := strings.Index(value, needle)
	if at < 0 {
		return ""
	}
	runes := []rune(value)
	index := len([]rune(value[:at]))
	start := index - 80
	if start < 0 {
		start = 0
	}
	end := index + len([]rune(needle)) + 160
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[start:end])
}
