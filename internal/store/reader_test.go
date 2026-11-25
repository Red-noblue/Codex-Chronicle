// 中文说明：验证阅读器只搜索指定历史文件的派生索引，并使用 frame 索引定位。
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/reader"
)

func TestReaderCandidatesStayWithinHistoryAndLocateFrames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		data := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"" + name + "\"}}\n" +
			"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"text\":\"reader-keyword 中文关键词\"}]}}\n"
		if err := os.WriteFile(filepath.Join(root, name+".jsonl"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := application.NewScanner().Scan(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	enableSearchIndex(t, s)
	if _, err = s.SyncInventory(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"reader-keyword", "中文关键词"} {
		hits, err := s.ReaderSearchCandidates(t.Context(), root, []string{"b.jsonl"}, query)
		if err != nil || len(hits) != 1 || hits[0].RelativePath != "b.jsonl" {
			t.Fatalf("scoped candidates: %+v %v", hits, err)
		}
		start, line, err := s.ReaderFrame(t.Context(), root, "b.jsonl", hits[0].StartOffset, 0)
		if err != nil || start != hits[0].StartOffset || line != 1 {
			t.Fatalf("indexed frame: %d %d %v", start, line, err)
		}
	}
}

// 中文说明：缓存跨进程复用且损坏可重建，删除对话后不得残留导航短标题。
func TestProjectionCacheRestoresAndDerivedCleanupInvalidates(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	data := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"projection-cache\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"cache_caption\"}}\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := application.NewScanner().Scan(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	enableSearchIndex(t, s)
	if _, err = s.SyncInventory(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	plan := reader.BuildPlan(t.Context(), []reader.Source{{Path: path, Relative: "rollout.jsonl"}}, []string{"rollout.jsonl"})
	first, err := reader.NewManager().Wait(t.Context(), plan, s)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := s.LoadReaderProjection(t.Context(), reader.ScopeKey(plan), reader.Stamp(plan))
	if err != nil || len(payload) == 0 {
		t.Fatal(err)
	}
	second, err := reader.NewManager().Wait(t.Context(), plan, s)
	if err != nil || len(second.Turns) != 1 || second.Turns[0].Caption != first.Turns[0].Caption {
		t.Fatal(second, err)
	}
	for _, corruption := range []string{"invalid_json", "foreign_reference", "negative_range", "missing_bytes"} {
		bad := []byte("corrupt")
		if corruption != "invalid_json" {
			var projection reader.Projection
			if err = json.Unmarshal(payload, &projection); err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "foreign_reference":
				projection.Turns[0].Message.Path = "other-conversation.jsonl"
			case "negative_range":
				projection.Ranges = append(projection.Ranges, reader.ExcludedRange{Path: "rollout.jsonl", Start: -1, End: 10, EndLine: 1, Reason: "rollback"})
			case "missing_bytes":
				projection.BytesRead = 0
			}
			bad, _ = json.Marshal(projection)
		}
		if err = s.SaveReaderProjection(t.Context(), reader.ScopeKey(plan), reader.Stamp(plan), bad); err != nil {
			t.Fatal(err)
		}
		manager := reader.NewManager()
		rebuilt, e := manager.Wait(t.Context(), plan, s)
		manager.Invalidate()
		if e != nil || len(rebuilt.Turns) != 1 || rebuilt.Turns[0].Message.Path != "rollout.jsonl" || len(rebuilt.Ranges) != 0 || rebuilt.BytesRead != int64(len(data)) {
			t.Fatal(corruption, rebuilt, e)
		}
	}
	manager := reader.NewManager()
	defer manager.Invalidate()
	if _, err = manager.Wait(t.Context(), plan, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveReaderProjection(t.Context(), reader.ScopeKey(plan), reader.Stamp(plan), []byte("not_saved_yet")); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Wait(t.Context(), plan, s); err != nil {
		t.Fatal(err)
	}
	latePayload, err := s.LoadReaderProjection(t.Context(), reader.ScopeKey(plan), reader.Stamp(plan))
	if err != nil || !json.Valid(latePayload) {
		t.Fatal("memory projection was not persisted on reader request", err)
	}
	if err = s.RemoveConversationDerived(t.Context(), root, report.Conversations[0].ID); err != nil {
		t.Fatal(err)
	}
	payload, err = s.LoadReaderProjection(t.Context(), reader.ScopeKey(plan), reader.Stamp(plan))
	if err != nil || len(payload) != 0 {
		t.Fatal("deleted caption remained", err, string(payload))
	}
}
