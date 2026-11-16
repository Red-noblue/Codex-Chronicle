// 中文说明：仅为发生历史编辑的会话校正首问与消息规模；复用阅读投影，已消耗 token 和原始事件数原样保留。
package application

import (
	"context"
	"strings"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
)

func applyEffectiveMetadata(ctx context.Context, report *domain.InventoryReport) {
	sources := []reader.Source{}
	edited := map[string]bool{}
	for _, file := range report.Files {
		if file.Rollout == nil {
			continue
		}
		path, err := ResolveReadOnlyPath(report.Root, file.RelativePath)
		if err != nil {
			continue
		}
		sources = append(sources, reader.Source{Relative: file.RelativePath, Path: path, ID: file.Rollout.SourceID})
		if file.Rollout.HistoryEdited {
			edited[file.Rollout.ConversationID] = true
		}
	}
	for i := range report.Conversations {
		conversation := &report.Conversations[i]
		if !edited[conversation.ID] {
			continue
		}
		plan := reader.BuildPlan(ctx, sources, conversation.RolloutPaths)
		if !plan.Complete {
			continue
		}
		projection, err := reader.DefaultManager.Wait(ctx, plan, nil)
		if err != nil || len(projection.Diagnostics) > 0 {
			continue
		}
		first := ""
		users, assistants, turns := 0, 0, 0
		for _, turn := range projection.Turns {
			if turn.Status == "rolled_back" {
				continue
			}
			if first == "" && turn.Caption != "" {
				first = turn.Caption
			}
			users += turn.UserMessages
			assistants += turn.AssistantMessages
			turns++
		}
		// 保留用户自行命名的标题，只替换由旧首问自动提取的标题。
		if conversation.Title == conversation.FirstUserText || conversation.FirstUserText != "" && strings.HasPrefix(conversation.FirstUserText, strings.TrimSuffix(conversation.Title, "…")) {
			conversation.Title = first
			if first == "" {
				conversation.Title = "已撤回全部正文"
			}
		}
		conversation.FirstUserText = first
		conversation.UserMessages, conversation.AssistantMessages, conversation.TurnCount = users, assistants, turns
	}
}
