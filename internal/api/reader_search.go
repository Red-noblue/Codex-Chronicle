// 中文说明：普通搜索复核有效主线与原始证据；技术搜索保留原始记录口径，二者不会混淆撤回内容。
package api

import (
	"context"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
	"codex-chronicle.local/internal/searchtext"
	"codex-chronicle.local/internal/store"
)

func effectiveSearchFilter(ctx context.Context, root, query string, report domain.InventoryReport, archive *store.Store, delivered ...domain.SearchHit) application.SearchHitFilter {
	needle := searchtext.Normalize(query)
	var sources []reader.Source
	byPath := map[string]domain.ConversationSummary{}
	for _, conversation := range report.Conversations {
		for _, path := range conversation.RolloutPaths {
			byPath[path] = conversation
		}
	}
	var plan reader.Plan
	var plannedID string
	var historyEnds map[string]int64
	type position struct {
		path   string
		offset int64
	}
	seen := make(map[position]bool)
	for _, hit := range delivered {
		seen[position{hit.RelativePath, hit.StartOffset}] = true
	}
	var cache reader.ProjectionCache
	if archive != nil {
		cache = archive
	}
	return func(hit domain.SearchHit) (domain.SearchHit, bool, error) {
		if err := ctx.Err(); err != nil {
			return hit, false, err
		}
		conversation, ok := byPath[hit.RelativePath]
		if !ok {
			return hit, false, nil
		}
		// 候选按来源文件排列。只持有当前会话计划，避免稀有词搜索保留大量历史计划。
		if plannedID != conversation.ID {
			if sources == nil {
				sources = make([]reader.Source, 0, len(byPath))
				for _, file := range report.Files {
					if file.Kind != domain.AssetKindJSONL || file.Rollout == nil {
						continue
					}
					if path, err := application.ResolveReadOnlyPath(root, file.RelativePath); err == nil {
						sources = append(sources, reader.Source{Relative: file.RelativePath, Path: path, ID: file.Rollout.SourceID})
					}
				}
			}
			plan = reader.BuildPlan(ctx, sources, conversation.RolloutPaths)
			projection, err := reader.DefaultManager.Wait(ctx, plan, cache)
			if err != nil {
				return hit, false, err
			}
			plan.Projection, plannedID = projection, conversation.ID
			historyEnds = make(map[string]int64, len(plan.Segments))
			for _, segment := range plan.Segments {
				historyEnds[segment.Relative] = segment.End
			}
		}
		ref := domain.EventReference{Path: hit.RelativePath, Start: hit.StartOffset, End: hit.EndOffset, Line: hit.Line}
		if plan.Projection.Excluded(ref.Path, ref.Start) {
			pair := plan.Projection.CallForResult(ref.Path, ref.Start)
			if pair == nil || plan.Projection.Excluded(pair.Call.Path, pair.Call.Start) {
				return hit, false, nil
			}
			ref = pair.Call
		}
		if ref.End > historyEnds[ref.Path] {
			return hit, false, nil
		}
		// 精确复核候选地址，不能把已跳过的记录误算成下一条消息的命中。
		page, err := reader.ReadPage(ctx, plan, reader.Position{Path: ref.Path, Offset: ref.Start, Line: ref.Line - 1}, 1, nil, true)
		if err != nil {
			return hit, false, err
		}
		if len(page.Items) == 0 {
			return hit, false, nil
		}
		event := page.Items[0]
		if event.RelativePath != ref.Path || event.StartOffset != ref.Start || event.Kind == "injected_context" || event.Role == "developer" {
			return hit, false, nil
		}
		match := searchtext.Snippet(searchtext.Normalize(reader.SearchableText(event)), needle)
		if match == "" {
			return hit, false, nil
		}
		key := position{ref.Path, ref.Start}
		if seen[key] {
			return hit, false, nil
		}
		seen[key] = true
		hit.Snippet = match
		hit.RelativePath, hit.StartOffset, hit.EndOffset, hit.Line = ref.Path, ref.Start, ref.End, ref.Line
		hit.ConversationID = conversation.ID
		return hit, true, nil
	}
}
