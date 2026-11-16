// 中文说明：将用户选择展开为去重的 spawned 后代列表，计数和确认使用实际线程范围。
package api

import (
	"codex-chronicle.local/internal/domain"
	"fmt"
)

func expandFamilyTargets(conversations []domain.ConversationSummary, requested []string) ([]string, error) {
	byID := map[string]domain.ConversationSummary{}
	for _, c := range conversations {
		byID[c.ID] = c
	}
	visited, visiting := map[string]bool{}, map[string]bool{}
	var ids []string
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("子代理关系存在环，请重新扫描")
		}
		if visited[id] {
			return nil
		}
		c, ok := byID[id]
		if !ok {
			return fmt.Errorf("目标或子代理已不存在，请重新扫描会话库")
		}
		if c.FamilyConflict {
			return fmt.Errorf("子代理关系冲突，停止生成执行计划")
		}
		visiting[id] = true
		for _, child := range c.ChildConversationIDs {
			if err := visit(child); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		ids = append(ids, id)
		if len(ids) > maxBatchOperationItems {
			return fmt.Errorf("包含子代理后超过 %d 个实际线程，请缩小选择范围", maxBatchOperationItems)
		}
		return nil
	}
	for _, id := range requested {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}
