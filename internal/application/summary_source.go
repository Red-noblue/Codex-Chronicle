// 中文说明：摘要与阅读器共用有效历史投影，只取有界开头窗口，避免引用已撤回或重复的正文。
package application

import (
	"context"
	"fmt"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
)

const (
	defaultSummaryEventLimit = 8
	maxSummaryEventLimit     = 50
)

func LoadSummaryEvents(ctx context.Context, root string, conversation domain.ConversationSummary, limit int) ([]domain.ConversationEvent, []string, error) {
	if limit <= 0 {
		limit = defaultSummaryEventLimit
	}
	if limit > maxSummaryEventLimit {
		limit = maxSummaryEventLimit
	}
	events := make([]domain.ConversationEvent, 0, limit)
	warnings := []string{}
	sources, err := reader.DiscoverSources(ctx, root)
	if err != nil {
		return events, warnings, err
	}
	targets := []string{}
	for _, relative := range conversation.RolloutPaths {
		if _, e := ResolveReadOnlyPath(root, relative); e != nil {
			warnings = append(warnings, relative+"："+e.Error())
			continue
		}
		targets = append(targets, relative)
	}
	plan := reader.BuildPlan(ctx, sources, targets)
	warnings = append(warnings, plan.Diagnostics...)
	if len(plan.Segments) == 0 {
		return events, warnings, nil
	}
	plan.Projection, err = reader.DefaultManager.Wait(ctx, plan, nil)
	if err != nil {
		return events, warnings, err
	}
	warnings = append(warnings, plan.Projection.Diagnostics...)
	var position reader.Position
	var bytes int64
	for len(events) < limit && bytes < reader.PageReadBytes {
		page, e := reader.ReadPage(ctx, plan, position, limit-len(events), nil, false)
		if e != nil {
			return events, warnings, e
		}
		bytes += page.BytesRead
		for _, event := range page.Items {
			if event.Kind != "injected_context" && event.Role != "developer" {
				events = append(events, event)
			}
		}
		if !page.HasMore {
			break
		}
		position, e = reader.DecodePosition(page.Next)
		if e != nil {
			return events, warnings, e
		}
	}
	warnings = append(warnings, fmt.Sprintf("正文取样为有效主线开头的 %d 条事件；token 与规模统计保留实际记录口径，撤回不会返还已消耗 token。", len(events)))
	return events, warnings, nil
}
