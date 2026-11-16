// 中文说明：合并 rollout、SQLite 来源和 spawn 边；只为明确的 spawned 线程建立家族。
package application

import (
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"sort"
)

func attachConversationFamilies(byID map[string]domain.ConversationSummary, files []domain.FileInventory) {
	parents := map[string]map[string]bool{}
	addParent := func(child, parent string) {
		if child == "" || parent == "" {
			return
		}
		if parents[child] == nil {
			parents[child] = map[string]bool{}
		}
		parents[child][parent] = true
	}
	merge := func(id string, origin domain.ThreadOrigin) {
		cid := compat.StableConversationID(id)
		c, ok := byID[cid]
		if !ok {
			return
		}
		if origin.ThreadKind != "" {
			if c.ThreadKind != "" && c.ThreadKind != origin.ThreadKind {
				c.FamilyConflict = true
			}
			c.ThreadKind = origin.ThreadKind
		}
		c.AgentPath = firstNonEmpty(c.AgentPath, origin.AgentPath)
		c.AgentNickname = firstNonEmpty(c.AgentNickname, origin.AgentNickname)
		c.AutomationKind = firstNonEmpty(c.AutomationKind, origin.AutomationKind)
		c.FamilyConflict = c.FamilyConflict || origin.OriginConflict
		c.ForkedFromID = firstNonEmpty(c.ForkedFromID, origin.ForkedFromID)
		addParent(id, origin.ParentThreadID)
		byID[cid] = c
	}
	for _, file := range files {
		if isBackupRolloutPath(file.RelativePath) {
			continue
		}
		if file.Rollout != nil {
			merge(file.Rollout.SourceID, file.Rollout.ThreadOrigin)
		}
		if file.SQLite == nil {
			continue
		}
		for _, t := range file.SQLite.Threads {
			merge(t.ID, t.ThreadOrigin)
		}
		for _, r := range file.SQLite.Relationships {
			if r.Table != "thread_spawn_edges" {
				continue
			}
			ids := r.RelatedThreadIDs
			if len(ids) == 0 && r.RelatedThreadID != "" {
				ids = []string{r.RelatedThreadID}
			}
			for _, related := range ids {
				if r.Column == "child_thread_id" {
					addParent(r.ThreadID, related)
				}
				if r.Column == "parent_thread_id" {
					addParent(related, r.ThreadID)
				}
			}
		}
	}
	for id, c := range byID {
		candidates := parents[c.SourceID]
		if c.ThreadKind == "subagent" && len(candidates) == 0 {
			c.FamilyConflict = true
		}
		if c.ThreadKind == "automation" && len(candidates) > 0 {
			c.FamilyConflict = true
		}
		if len(candidates) > 1 {
			c.FamilyConflict = true
		} else {
			for parent := range candidates {
				c.ParentThreadID, c.ThreadKind = parent, "subagent"
			}
		}
		if compat.IsInjectedContext(c.FirstUserText) {
			c.FirstUserText = ""
		}
		if compat.IsInjectedContext(c.Title) || c.Title == "" {
			c.Title = firstNonEmpty(compat.OriginTitle(c.ThreadOrigin), c.FirstUserText, "未命名对话")
		}
		byID[id] = c
	}
	// 环和来源冲突只进入诊断，不能构造可展开、可级联的树。
	for id, c := range byID {
		seen := map[string]bool{id: true}
		for parent := c.ParentThreadID; parent != ""; {
			pid := compat.StableConversationID(parent)
			if seen[pid] {
				c.FamilyConflict = true
				break
			}
			seen[pid] = true
			p, ok := byID[pid]
			if !ok {
				break
			}
			if p.FamilyConflict {
				c.FamilyConflict = true
				break
			}
			parent = p.ParentThreadID
		}
		if c.FamilyConflict {
			c.Status = stricterStatus(c.Status, domain.StatusConflict)
			c.Diagnostics = appendDiagnostic(c.Diagnostics, domain.Diagnostic{Code: "SPAWN_RELATION_CONFLICT", Severity: domain.SeverityBlock, Message: "子代理关系存在冲突或环，停止级联处理", ReadOnly: true})
		} else if c.ParentThreadID != "" {
			pid := compat.StableConversationID(c.ParentThreadID)
			if _, ok := byID[pid]; ok {
				c.ParentConversationID = pid
			} else {
				c.ParentMissing = true
			}
		}
		byID[id] = c
	}
	for id, c := range byID {
		if c.ParentConversationID != "" && !c.FamilyConflict {
			p := byID[c.ParentConversationID]
			p.ChildConversationIDs = append(p.ChildConversationIDs, id)
			byID[p.ID] = p
		}
	}
	for id, c := range byID {
		sort.Strings(c.ChildConversationIDs)
		var count func(string) int
		count = func(cid string) int {
			n := 0
			for _, child := range byID[cid].ChildConversationIDs {
				n += 1 + count(child)
			}
			return n
		}
		c.DescendantCount = count(id)
		byID[id] = c
	}
}
