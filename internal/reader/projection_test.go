// 中文说明：用隔离样本验证有效主线、跨页配对、多版本选择及追加后游标，不触碰本机对话。
package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func record(kind string, payload any) string {
	b, _ := json.Marshal(map[string]any{"type": kind, "payload": payload})
	return string(b) + "\n"
}
func evt(kind string, fields map[string]any) string {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["type"] = kind
	return record("event_msg", fields)
}
func projected(t *testing.T, s Source) Plan {
	t.Helper()
	p := BuildPlan(t.Context(), []Source{s}, []string{s.Relative})
	var err error
	p.Projection, err = BuildProjection(t.Context(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func readAll(t *testing.T, p Plan, limit int) []domain.ConversationEvent {
	t.Helper()
	var pos Position
	out := []domain.ConversationEvent{}
	for i := 0; i < 1000; i++ {
		page, err := ReadPage(t.Context(), p, pos, limit, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Items...)
		if !page.HasMore {
			return out
		}
		pos, err = DecodePosition(page.Next)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("pagination never ended")
	return out
}

func TestProjectionRollbackSeesLaterPagesAndRetainsOriginal(t *testing.T) {
	data := ""
	for i := 0; i < 130; i++ {
		data += evt("task_started", map[string]any{"turn_id": fmt.Sprint(i)}) + message(fmt.Sprintf("question-%03d", i)) + evt("task_complete", nil)
	}
	data += evt("thread_rolled_back", map[string]any{"num_turns": 100}) + message("replacement")
	s := source(t, t.TempDir(), "rollout.jsonl", data)
	p := projected(t, s)
	events := readAll(t, p, 7)
	if len(events) != 31 || events[29].Text != "question-029" || events[30].Text != "replacement" {
		t.Fatalf("rollback leaked or lost events: %d", len(events))
	}
	overview := p.Projection.Overview(0, 100, false)
	if overview["total"] != 31 {
		t.Fatal(overview)
	}
	b, _ := os.ReadFile(s.Path)
	if string(b) != data {
		t.Fatal("source was modified")
	}
	for _, count := range []any{-1, 1.5, "1", uint64(1) << 32, nil} {
		s = source(t, t.TempDir(), "invalid.jsonl", message("keep")+evt("thread_rolled_back", map[string]any{"num_turns": count}))
		events = readAll(t, projected(t, s), 10)
		found := false
		for _, e := range events {
			found = found || e.Text == "keep"
		}
		if !found {
			t.Fatal("invalid marker erased history", count)
		}
	}
}

func TestProjectionMirrorsPreserveRealRepeatsAndExplicitTurns(t *testing.T) {
	mirror := evt("user_message", map[string]any{"message": "same"}) + message("same") + evt("item_completed", map[string]any{"item": map[string]any{"type": "UserMessage", "id": "u1", "content": []any{map[string]any{"type": "Text", "text": "same"}}}})
	s := source(t, t.TempDir(), "mirrors.jsonl", mirror+mirror)
	p := projected(t, s)
	events := readAll(t, p, 1)
	if len(events) != 2 || events[0].TurnID == events[1].TurnID {
		t.Fatalf("real repeats lost: %+v", events)
	}
	s = source(t, t.TempDir(), "turn.jsonl", record("turn_context", map[string]any{"turn_id": "explicit"})+message("first")+message("second"))
	events = readAll(t, projected(t, s), 1)
	if len(events) != 2 || events[0].TurnID != "explicit" || events[1].TurnID != "explicit" || events[0].TurnInferred {
		t.Fatalf("explicit turn lost: %+v", events)
	}
}

func TestProjectionToolsAcrossPagesAreHonest(t *testing.T) {
	call := record("response_item", map[string]any{"type": "custom_tool_call", "call_id": "patch", "name": "apply_patch", "input": "*** Begin Patch\n*** Update File: src/main.go\n@@\n-old\n+new\n*** End Patch"})
	output := record("response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "patch", "output": []any{map[string]any{"type": "input_text", "text": "Process exited with code 1\nfailed"}}})
	s := source(t, t.TempDir(), "tools.jsonl", message("edit")+call+message("between")+output)
	p := projected(t, s)
	events := readAll(t, p, 1)
	if len(events) != 3 || events[1].Status != "failed" || events[1].Tool.Result == nil || !strings.Contains(events[1].Tool.Output, "failed") || events[1].Changes[0].Status != "failed" {
		t.Fatalf("pair: %+v", events)
	}
	if p.Projection.Files[0].Status != "failed" {
		t.Fatal("failed edit was confirmed", p.Projection.Files)
	}
	s = source(t, t.TempDir(), "ambiguous.jsonl", message("edit")+call+output+call)
	p = projected(t, s)
	events = readAll(t, p, 10)
	if len(events) != 4 || events[1].Tool.Result != nil || events[3].Tool.Result != nil {
		t.Fatal("duplicate call ID paired", events)
	}
	s = source(t, t.TempDir(), "rollback-tool.jsonl", message("old")+call+evt("thread_rolled_back", map[string]any{"num_turns": 1})+message("new")+output)
	p = projected(t, s)
	events = readAll(t, p, 10)
	if len(events) != 2 || p.Projection.Tools[0].Result != nil {
		t.Fatal("rolled back call paired", events)
	}
	s = source(t, t.TempDir(), "out-of-order.jsonl", message("edit")+output+call)
	p = projected(t, s)
	events = readAll(t, p, 10)
	if len(events) != 3 || p.Projection.Tools[0].Result != nil || events[2].Status == "failed" {
		t.Fatal("earlier result incorrectly paired", events)
	}
}

func TestProjectionAppendKeepsCursorButRollbackInvalidates(t *testing.T) {
	s := source(t, t.TempDir(), "append.jsonl", message("one")+message("two"))
	p := projected(t, s)
	page, err := ReadPage(t.Context(), p, Position{}, 1, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cursor, _ := DecodePosition(page.Next)
	f, _ := os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(message("three"))
	f.Close()
	p = projected(t, s)
	page, err = ReadPage(t.Context(), p, cursor, 10, nil, false)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("append rejected: %v %+v", err, page)
	}
	f, _ = os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(evt("thread_rolled_back", map[string]any{"num_turns": 0}))
	f.Close()
	p = projected(t, s)
	if _, err = ReadPage(t.Context(), p, cursor, 10, nil, false); err != nil {
		t.Fatal("zero-turn marker invalidated effective history", err)
	}
	f, _ = os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(evt("thread_rolled_back", map[string]any{"num_turns": 1}))
	f.Close()
	p = projected(t, s)
	if _, err = ReadPage(t.Context(), p, cursor, 10, nil, false); err == nil {
		t.Fatal("rollback reused stale cursor")
	}
}

func TestIndependentVersionsNeverConcatenate(t *testing.T) {
	dir := t.TempDir()
	a := source(t, dir, "old.jsonl", record("session_meta", map[string]any{"id": "thread"})+message("old"))
	b := source(t, dir, "new.jsonl", record("session_meta", map[string]any{"id": "thread"})+message("new"))
	p := BuildPlan(t.Context(), []Source{a, b}, []string{a.Relative, b.Relative})
	if p.Complete || len(p.Segments) != 1 || len(p.Versions) != 2 {
		t.Fatal(p)
	}
	p = BuildPlanForSource(t.Context(), []Source{a, b}, []string{a.Relative, b.Relative}, a.Relative)
	events := readAll(t, p, 10)
	if len(events) != 1 || events[0].Text != "old" || p.Leaf != a.Relative {
		t.Fatal("old source silently changed", p, events)
	}
	p = BuildPlanForSource(context.Background(), []Source{a, b}, []string{a.Relative, b.Relative}, "foreign.jsonl")
	if len(p.Segments) != 0 {
		t.Fatal("foreign version accepted")
	}
}

func TestRecordedFileMirrorsArePairedOneToOne(t *testing.T) {
	changes := map[string]any{"src/a.go": map[string]any{"type": "update", "unified_diff": "@@\n-a\n+b"}}
	legacy := evt("patch_apply_end", map[string]any{"turn_id": "t", "success": true, "changes": changes})
	modern := evt("item_completed", map[string]any{"turn_id": "t", "item": map[string]any{"type": "FileChange", "id": "patch", "status": "completed", "changes": changes}})
	for _, data := range []string{legacy + modern + modern, legacy + legacy + modern + modern} {
		s := source(t, t.TempDir(), "files.jsonl", evt("task_started", map[string]any{"turn_id": "t"})+message("edit")+data)
		p := projected(t, s)
		if p.Projection.Overview(0, 100, true)["total"] != 2 {
			t.Fatalf("independent edits lost: %+v", p.Projection.Files)
		}
	}
}

func TestLegacyTurnContextAndHistoryBaseRollback(t *testing.T) {
	s := source(t, t.TempDir(), "context.jsonl", record("turn_context", map[string]any{"turn_id": "one"})+message("keep")+record("turn_context", map[string]any{"turn_id": "two"})+message("remove")+evt("thread_rolled_back", map[string]any{"num_turns": 1}))
	events := readAll(t, projected(t, s), 1)
	if len(events) != 1 || events[0].Text != "keep" {
		t.Fatal("legacy turn boundary lost", events)
	}
	dir := t.TempDir()
	ordinal := func(n int, kind string, payload any) string {
		b, _ := json.Marshal(map[string]any{"ordinal": n, "type": kind, "payload": payload})
		return string(b) + "\n"
	}
	prefix := ordinal(0, "session_meta", map[string]any{"id": "base"}) + ordinal(1, "response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"text": "inherited"}}})
	base := source(t, dir, "base.jsonl", prefix+message("outside-prefix"))
	child := source(t, dir, "child.jsonl", ordinal(2, "session_meta", map[string]any{"id": "child", "history_mode": "paginated", "history_base": map[string]any{"thread_id": "base", "end_ordinal_exclusive": 2, "end_byte_offset": len(prefix)}})+evt("thread_rolled_back", map[string]any{"num_turns": 1})+message("replacement"))
	plan := BuildPlan(t.Context(), []Source{base, child}, []string{child.Relative})
	if !plan.Complete {
		t.Fatal(plan)
	}
	var err error
	plan.Projection, err = BuildProjection(t.Context(), plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	events = readAll(t, plan, 1)
	if len(events) != 1 || events[0].Text != "replacement" {
		t.Fatal("history base rollback leaked", events)
	}
}

func TestProjectionSnapshotAllowsAppendAndRejectsRewrites(t *testing.T) {
	s := source(t, t.TempDir(), "active.jsonl", message("first"))
	plan := BuildPlan(t.Context(), []Source{s}, []string{s.Relative})
	f, _ := os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(message("later"))
	f.Close()
	projection, err := BuildProjection(t.Context(), plan, nil)
	if err != nil {
		t.Fatal("fixed prefix append rejected", err)
	}
	plan.Projection = projection
	events := readAll(t, plan, 10)
	if len(events) != 1 || events[0].Text != "first" {
		t.Fatal("snapshot read appended suffix", events)
	}
	if err := os.WriteFile(s.Path, []byte(message("other")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = BuildProjection(t.Context(), plan, nil); err == nil {
		t.Fatal("same-size rewrite accepted")
	}
	if _, err = ReadPage(t.Context(), plan, Position{}, 1, nil, false); err == nil {
		t.Fatal("stale projection used against rewritten source")
	}
}
