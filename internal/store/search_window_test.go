// 中文说明：验证会话窗口跨越压缩点、随新消息滚动，且各轮摘要可以独立索引。
package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestRecentSearchWindowAndIndependentSummaries(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	file := filepath.Join(root, "rollout-window.jsonl")
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	message := func(needle string, padding int) string {
		return fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`+"\n", needle+" "+strings.Repeat("a", padding))
	}
	window := func(tokens int) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"model_context_window":%d,"last_token_usage":{"input_tokens":9999,"cached_input_tokens":8000}}}}`+"\n", tokens)
	}
	tool := "{\"type\":\"response_item\",\"payload\":{\"type\":\"function_call\",\"name\":\"exec_command\",\"call_id\":\"old-call\",\"arguments\":\"{\\\"cmd\\\":\\\"printf oldtoolargument\\\"}\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"function_call_output\",\"call_id\":\"old-call\",\"output\":\"oldtooloutput\"}}\n"
	source := `{"type":"session_meta","payload":{"id":"history-window"}}` + "\n" + message("oldoutsidewindow", 320) + `{"type":"compacted","payload":{"message":"summaryfromfirstround"}}
{"type":"event_msg","payload":{"type":"context_compacted","message":"legacycompactionnote"}}
` + tool + message("beforelastcompaction", 240) + `{"type":"compacted","payload":{"message":"summaryfromlastround"}}
{"type":"compacted","payload":{"message":"","replacement_history":[{"type":"compaction","encrypted_content":"opaquecompaction"}]}}
` + message("afterlastcompaction", 120) + window(100)
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(source)
	open := func() *Store {
		t.Helper()
		s, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	s := open()
	var report domain.InventoryReport
	build := func() SearchIndexProgress {
		t.Helper()
		report = scanSearchFixture(t, root)
		progress, err := s.BuildSearchIndex(ctx, report, nil)
		if err != nil || progress.BudgetReached {
			t.Fatalf("构建窗口索引失败：%+v %v", progress, err)
		}
		return progress
	}
	assertIndexed := func(needle string, count int) {
		t.Helper()
		hits, err := s.SearchIndexOnlyObserved(ctx, root, needle, 10, report.Files, nil, nil)
		if err != nil || len(hits) != count {
			t.Fatalf("仅索引搜索 %s：预期 %d 个命中，得到 %d，错误：%v", needle, count, len(hits), err)
		}
	}
	recent := true
	ids := []string{"user", "summary", "command.input", "command.output"}
	settings := SearchIndexSettings{BudgetBytes: 4 << 20, Content: &ids, RecentContext: &recent}
	if err := s.SetSearchIndexSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	build()
	assertIndexed("oldoutsidewindow", 0)
	assertIndexed("oldtoolargument", 1)
	assertIndexed("oldtooloutput", 1)
	assertIndexed("beforelastcompaction", 1)
	assertIndexed("afterlastcompaction", 1)
	assertIndexed("summaryfromfirstround", 1)
	assertIndexed("summaryfromlastround", 1)
	assertIndexed("opaquecompaction", 0)
	if hits, err := s.SearchWithFiles(ctx, root, "oldoutsidewindow", 10, report.Files); err != nil || len(hits) != 1 || hits[0].MatchOrigin != domain.SearchMatchSource {
		t.Fatalf("全部搜索未补回界外原文：%+v %v", hits, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	persisted, err := s.SearchIndexSettings(ctx)
	if err != nil || persisted.RecentContext == nil || !*persisted.RecentContext {
		t.Fatalf("重开后丢失窗口设置：%+v %v", persisted, err)
	}
	source += message("appendednewreply", 248)
	write(source)
	if progress := build(); progress.DocumentsAdded != 1 {
		t.Fatalf("窗口前移不应重复写入保留的记录：%+v", progress)
	}
	assertIndexed("beforelastcompaction", 0)
	assertIndexed("afterlastcompaction", 1)
	assertIndexed("appendednewreply", 1)
	assertIndexed("summaryfromfirstround", 1)
	assertIndexed("legacycompactionnote", 1)
	assertIndexed("oldtoolargument", 1)
	assertIndexed("oldtooloutput", 1)

	source += window(500)
	write(source)
	build()
	assertIndexed("oldoutsidewindow", 1)

	ids = []string{"user"}
	if err := s.SetSearchIndexSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	build()
	assertIndexed("summaryfromfirstround", 0)
	assertIndexed("appendednewreply", 1)
	assertIndexed("oldtooloutput", 0)
	ids = []string{"summary"}
	if err := s.SetSearchIndexSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	build()
	assertIndexed("summaryfromfirstround", 1)
	assertIndexed("appendednewreply", 0)
	if got, err := os.ReadFile(file); err != nil || !bytes.Equal(got, []byte(source)) {
		t.Fatalf("索引构建修改源文件：%v", err)
	}
}

func TestRecentSearchWindowKeepsDefaultContent(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	path := filepath.Join(root, "rollout-default-window.jsonl")
	message := func(role, phase, value string) string {
		return fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":%q,"phase":%q,"content":[{"type":"input_text","text":%q}]}}`+"\n", role, phase, value)
	}
	source := `{"type":"session_meta","payload":{"id":"default-window"}}
{"type":"compacted","payload":{"message":"alwaysfindsummary"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"first-call","output":"alwaysfindtool"}}
` + message("assistant", "commentary", "earlierprogress "+strings.Repeat("a", 320)) +
		message("user", "", "beforeappend "+strings.Repeat("b", 224)) + `{"type":"event_msg","payload":{"type":"token_count","info":{"model_context_window":50}}}
`
	s, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	setRange := func(recent bool) {
		t.Helper()
		if err := s.SetSearchIndexSettings(ctx, SearchIndexSettings{BudgetBytes: 4 << 20, RecentContext: &recent}); err != nil {
			t.Fatal(err)
		}
		settings, err := s.SearchIndexSettings(ctx)
		if err != nil || settings.Content != nil || (settings.RecentContext != nil && *settings.RecentContext) != recent {
			t.Fatalf("切换时间范围改动了默认类别：%+v %v", settings, err)
		}
	}
	var report domain.InventoryReport
	build := func() {
		t.Helper()
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		report = scanSearchFixture(t, root)
		if progress, err := s.BuildSearchIndex(ctx, report, nil); err != nil || progress.BudgetReached {
			t.Fatalf("构建失败：%+v %v", progress, err)
		}
	}
	check := func(needle string, indexed int) {
		t.Helper()
		hits, err := s.SearchIndexOnlyObserved(ctx, root, needle, 10, report.Files, nil, nil)
		if err != nil || len(hits) != indexed {
			t.Fatalf("仅索引搜索 %s：预期 %d 处，实际 %d：%v", needle, indexed, len(hits), err)
		}
	}
	setRange(true)
	build()
	check("earlierprogress", 0)
	check("beforeappend", 1)
	check("alwaysfindsummary", 1)
	check("alwaysfindtool", 1)
	source += message("assistant", "commentary", "latestprogress "+strings.Repeat("c", 224))
	build()
	check("beforeappend", 0)
	check("latestprogress", 1)
	check("alwaysfindtool", 1)
	if hits, err := s.SearchWithFiles(ctx, root, "beforeappend", 10, report.Files); err != nil || len(hits) != 1 || hits[0].MatchOrigin != domain.SearchMatchSource {
		t.Fatalf("全部搜索遗漏界外正文：%+v %v", hits, err)
	}
	setRange(false)
	build()
	check("earlierprogress", 1)
	check("beforeappend", 1)
}
