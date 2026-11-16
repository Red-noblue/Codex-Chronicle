// 中文说明：普通搜索复核有效主线与原始证据；技术搜索保留原始记录口径，二者不会混淆撤回内容。
package api

import (
	"context"
	"strconv"
	"strings"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
	"codex-chronicle.local/internal/searchtext"
)

func (s *Server) effectiveSearchHits(ctx context.Context, root, query string, candidates []domain.SearchHit, limit int) ([]domain.SearchHit, error) {
	snapshot, err := s.tasks.ReadReport(ctx, root, application.ScanOptions{}, true)
	if err != nil {
		return nil, err
	}
	sources := []reader.Source{}
	byPath := map[string]domain.ConversationSummary{}
	for _, file := range snapshot.Report.Files {
		if file.Kind != domain.AssetKindJSONL || file.Rollout == nil {
			continue
		}
		path, e := application.ResolveReadOnlyPath(root, file.RelativePath)
		if e != nil {
			continue
		}
		source := reader.Source{Relative: file.RelativePath, Path: path}
		if file.Rollout != nil {
			source.ID = file.Rollout.SourceID
		}
		sources = append(sources, source)
	}
	for _, conversation := range snapshot.Report.Conversations {
		for _, path := range conversation.RolloutPaths {
			byPath[path] = conversation
		}
	}
	plans := map[string]reader.Plan{}
	out := []domain.SearchHit{}
	seen := map[string]bool{}
	archive := s.currentWorkspace().Archive
	var cache reader.ProjectionCache
	if archive != nil {
		cache = archive
	}
	for _, hit := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conversation, ok := byPath[hit.RelativePath]
		if !ok {
			continue
		}
		plan, ok := plans[conversation.ID]
		if !ok {
			plan = reader.BuildPlan(ctx, sources, conversation.RolloutPaths)
			plan.Projection, err = reader.DefaultManager.Wait(ctx, plan, cache)
			if err != nil {
				return nil, err
			}
			plans[conversation.ID] = plan
		}
		ref := domain.EventReference{Path: hit.RelativePath, Start: hit.StartOffset, End: hit.EndOffset, Line: hit.Line}
		if plan.Projection.Excluded(ref.Path, ref.Start) {
			pair := plan.Projection.CallForResult(ref.Path, ref.Start)
			if pair == nil || plan.Projection.Excluded(pair.Call.Path, pair.Call.Start) {
				continue
			}
			ref = pair.Call
		}
		// 精确复核候选地址，不能把已跳过的记录误算成下一条消息的命中。
		page, e := reader.ReadPage(ctx, plan, reader.Position{Path: ref.Path, Offset: ref.Start, Line: ref.Line - 1}, 1, nil, true)
		if e != nil || len(page.Items) == 0 {
			continue
		}
		event := page.Items[0]
		if event.RelativePath != ref.Path || event.StartOffset != ref.Start || event.Kind == "injected_context" || event.Role == "developer" {
			continue
		}
		text := reader.SearchableText(event)
		if !strings.Contains(searchtext.Normalize(text), searchtext.Normalize(query)) {
			continue
		}
		key := ref.Path + ":" + strconv.FormatInt(ref.Start, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		hit.Snippet = readerSearchSnippet(text, searchtext.Normalize(query))
		hit.RelativePath, hit.StartOffset, hit.EndOffset, hit.Line = ref.Path, ref.Start, ref.End, ref.Line
		hit.ConversationID = conversation.ID
		out = append(out, hit)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
