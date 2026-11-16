// 中文说明：验证多来源家族汇总、缺失父会话、普通 fork、冲突和循环关系。
package application

import (
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"testing"
)

func TestConversationFamiliesMergeAllChildren(t *testing.T) {
	byID := map[string]domain.ConversationSummary{}
	for _, id := range []string{"root", "a", "b", "c", "orphan", "fork", "memory"} {
		cid := compat.StableConversationID(id)
		byID[cid] = domain.ConversationSummary{ID: cid, SourceID: id}
	}
	files := []domain.FileInventory{
		{Rollout: &domain.RolloutSummary{SourceID: "a", ThreadOrigin: domain.ThreadOrigin{ParentThreadID: "root", ThreadKind: "subagent"}}},
		{Rollout: &domain.RolloutSummary{SourceID: "c", ThreadOrigin: domain.ThreadOrigin{ParentThreadID: "a", ThreadKind: "subagent"}}},
		{Rollout: &domain.RolloutSummary{SourceID: "orphan", ThreadOrigin: domain.ThreadOrigin{ParentThreadID: "gone", ThreadKind: "subagent"}}},
		{Rollout: &domain.RolloutSummary{SourceID: "fork", ThreadOrigin: domain.ThreadOrigin{ForkedFromID: "root"}}},
		{Rollout: &domain.RolloutSummary{SourceID: "memory", ThreadOrigin: domain.ThreadOrigin{ThreadKind: "automation"}}},
		{SQLite: &domain.SQLiteSummary{Relationships: []domain.SQLiteRelationship{{Table: "thread_spawn_edges", Column: "parent_thread_id", ThreadID: "root", RelatedThreadIDs: []string{"a", "b"}}}}},
	}
	attachConversationFamilies(byID, files)
	root := byID[compat.StableConversationID("root")]
	if root.DescendantCount != 3 || len(root.ChildConversationIDs) != 2 {
		t.Fatalf("lost children: %+v", root)
	}
	if !byID[compat.StableConversationID("orphan")].ParentMissing {
		t.Fatal("orphan not marked")
	}
	if byID[compat.StableConversationID("fork")].ParentThreadID != "" || byID[compat.StableConversationID("memory")].ParentThreadID != "" {
		t.Fatal("fork or automation included")
	}
}

func TestConversationFamilyConflictsDoNotFormTree(t *testing.T) {
	byID := map[string]domain.ConversationSummary{}
	var files []domain.FileInventory
	for child, parent := range map[string]string{"a": "b", "b": "a", "c": "a"} {
		cid := compat.StableConversationID(child)
		byID[cid] = domain.ConversationSummary{ID: cid, SourceID: child}
		files = append(files, domain.FileInventory{Rollout: &domain.RolloutSummary{SourceID: child, ThreadOrigin: domain.ThreadOrigin{ParentThreadID: parent}}})
	}
	attachConversationFamilies(byID, files)
	for _, c := range byID {
		if !c.FamilyConflict || c.DescendantCount != 0 {
			t.Fatalf("unsafe family: %+v", c)
		}
	}
}
