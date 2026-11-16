// 中文说明：验证有界分页、来源锚点、追加/截断、超长消息及历史依赖，全部使用隔离样本。
package reader

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func source(t *testing.T, dir, name, data string) Source {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return Source{Relative: name, Path: p}
}
func message(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": text}}}})
	return string(b) + "\n"
}
func TestPagingSeeksAndAppends(t *testing.T) {
	dir := t.TempDir()
	data := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"a\"}}\n"
	for i := 0; i < 300; i++ {
		data += message(fmt.Sprint(i))
	}
	s := source(t, dir, "a.jsonl", data)
	plan := BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	page, err := ReadPage(context.Background(), plan, Position{}, 80, nil, false)
	if err != nil || len(page.Items) != 80 || !page.HasMore {
		t.Fatalf("page: %+v %v", page, err)
	}
	p, err := DecodePosition(page.Next)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ReadPage(context.Background(), plan, p, 80, nil, false)
	if err != nil || len(next.Items) != 80 || next.Items[0].Text != "80" {
		t.Fatalf("next %v %+v", err, next)
	}
	if next.BytesRead >= int64(len(data)) {
		t.Fatal("pagination rescanned the prefix")
	}
	f, _ := os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(message("追加"))
	f.Close()
	plan = BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	if _, err := ReadPage(context.Background(), plan, p, 80, nil, false); err != nil {
		t.Fatal("append rejected", err)
	}
	_ = os.WriteFile(s.Path, []byte(data[:50]), 0600)
	plan = BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	if _, err := ReadPage(context.Background(), plan, p, 80, nil, false); err == nil {
		t.Fatal("truncation accepted")
	}
}
func TestAnchorsPathsAndLongText(t *testing.T) {
	dir := t.TempDir()
	s := source(t, dir, "a.jsonl", message(strings.Repeat("长", 30000)+"末尾关键词"))
	plan := BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	p, err := ReadPage(context.Background(), plan, Position{}, 80, nil, false)
	if err != nil || len(p.Items) != 1 || !p.Items[0].Truncated {
		t.Fatalf("preview %v", err)
	}
	full, err := ReadPage(context.Background(), plan, Position{}, 1, nil, true)
	if err != nil || !strings.HasSuffix(full.Items[0].Text, "末尾关键词") || full.Items[0].Truncated {
		t.Fatal("full missing", err)
	}
	if p.Items[0].ID != full.Items[0].ID || p.Items[0].RelativePath != "a.jsonl" {
		t.Fatal("unstable identity")
	}
	if _, err := ReadPage(context.Background(), plan, Position{Path: "../secret", Offset: 0}, 80, nil, false); err == nil {
		t.Fatal("foreign path accepted")
	}
	if _, err := ReadPage(context.Background(), plan, Position{Path: "a.jsonl", Offset: 2}, 80, nil, false); err == nil {
		t.Fatal("partial record accepted")
	}
}
func TestPendingCancellationAndOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	s := source(t, dir, "p.jsonl", message("完整")+"{\"type\":")
	plan := BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	page, err := ReadPage(context.Background(), plan, Position{}, 80, nil, false)
	if err != nil || len(page.Items) != 1 || !page.Pending {
		t.Fatalf("pending %v %+v", err, page)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadPage(ctx, plan, Position{}, 80, nil, false); err == nil {
		t.Fatal("cancellation ignored")
	}
	s = source(t, dir, "large.jsonl", message(strings.Repeat("x", MaxLineBytes+1))+message("下一条"))
	plan = BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	page, err = ReadPage(context.Background(), plan, Position{}, 80, nil, false)
	if err != nil || len(page.Diagnostics) == 0 {
		t.Fatal("oversized not diagnosed", err)
	}
	position, _ := DecodePosition(page.Next)
	next, err := ReadPage(context.Background(), plan, position, 80, nil, false)
	if err != nil || len(next.Items) != 1 || next.Items[0].Text != "下一条" {
		t.Fatalf("oversized continuation: %+v %v", next, err)
	}
}

func TestMirrorsPreserveRepeatedQuestionsAndTurnsAcrossPages(t *testing.T) {
	dir := t.TempDir()
	event := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"重复\"}}\n"
	data := event + message("重复") + event + message("重复")
	s := source(t, dir, "mirrors.jsonl", data)
	plan := BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	first, err := ReadPage(context.Background(), plan, Position{}, 1, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := DecodePosition(first.Next)
	second, err := ReadPage(context.Background(), plan, p, 80, nil, false)
	if err != nil || len(second.Items) != 1 || first.Items[0].TurnID == second.Items[0].TurnID {
		t.Fatalf("distinct repeats lost: %+v %v", second, err)
	}
	long := strings.Repeat("x", 70000)
	b, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]string{"type": "user_message", "message": long + "甲"}})
	s = source(t, dir, "long-mirror.jsonl", string(b)+"\n"+message(long+"乙"))
	plan = BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	page, err := ReadPage(context.Background(), plan, Position{}, 80, nil, false)
	if err != nil || len(page.Items) != 2 {
		t.Fatal("preview text caused false deduplication", err)
	}
	s = source(t, dir, "turns.jsonl", "{\"type\":\"turn_context\",\"payload\":{\"turn_id\":\"turn-1\"}}\n"+message("问题")+message("继续"))
	plan = BuildPlan(context.Background(), []Source{s}, []string{s.Relative})
	first, _ = ReadPage(context.Background(), plan, Position{}, 1, nil, false)
	p, _ = DecodePosition(first.Next)
	second, err = ReadPage(context.Background(), plan, p, 1, nil, false)
	if err != nil || second.Items[0].TurnID != "turn-1" || second.Items[0].TurnInferred {
		t.Fatal("explicit turn lost across cursor", err)
	}
}

func TestPaginatedRootAndDependencyGuards(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	a := source(t, dir, "sessions/root.jsonl", "{\"ordinal\":0,\"type\":\"session_meta\",\"payload\":{\"id\":\"base\",\"history_mode\":\"paginated\"}}\n"+message("提问"))
	plan := BuildPlan(context.Background(), []Source{a}, []string{a.Relative})
	if !plan.Complete || len(plan.Diagnostics) != 0 {
		t.Fatalf("paginated root marked incomplete: %+v", plan)
	}
	if err := CheckDependencies(context.Background(), dir, []string{a.Relative}); err != nil {
		t.Fatal("root blocked", err)
	}
	b := source(t, dir, "sessions/child.jsonl", "{\"ordinal\":0,\"type\":\"session_meta\",\"payload\":{\"id\":\"child\",\"history_mode\":\"paginated\",\"history_base\":{\"thread_id\":\"base\",\"end_ordinal_exclusive\":0,\"end_byte_offset\":0}}}\n")
	for _, target := range []string{a.Relative, b.Relative} {
		if CheckDependencies(context.Background(), dir, []string{target}) == nil {
			t.Fatal("shared history accepted", target)
		}
	}
	if err := os.WriteFile(b.Path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if CheckDependencies(context.Background(), dir, []string{a.Relative}) == nil {
		t.Fatal("unreadable dependency ignored")
	}
	if CheckDependencies(context.Background(), dir, []string{"../outside.jsonl"}) == nil {
		t.Fatal("outside dependency accepted")
	}
	if err := os.WriteFile(b.Path, []byte("{\"type\":\"session_meta\",\"id\":\"child\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDependencies(context.Background(), dir, []string{b.Relative}); err != nil {
		t.Fatal("legacy top-level identity blocked", err)
	}
}

func TestInventoryIdentityAvoidsOpeningUnrelatedSources(t *testing.T) {
	dir := t.TempDir()
	a := source(t, dir, "one.jsonl", "{\"type\":\"session_meta\",\"payload\":{\"id\":\"one\"}}\n"+message("正文"))
	a.ID = "one"
	missing := Source{Relative: "unrelated.jsonl", Path: filepath.Join(dir, "not-opened.jsonl"), ID: "unrelated"}
	plan := BuildPlan(t.Context(), []Source{a, missing}, []string{a.Relative})
	if !plan.Complete || len(plan.Segments) != 1 {
		t.Fatalf("unrelated file affected reading: %+v", plan)
	}
}

func BenchmarkReaderWindows(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "large.jsonl")
	file, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	w := bufio.NewWriter(file)
	header := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"bench\"}}\n"
	row := message(strings.Repeat("x", 4096))
	_, _ = w.WriteString(header)
	for i := 0; i < 14000; i++ {
		_, _ = w.WriteString(row)
	}
	if err := w.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	plan := BuildPlan(b.Context(), []Source{{Relative: "large.jsonl", Path: path, ID: "bench"}}, []string{"large.jsonl"})
	for name, offset := range map[string]int64{"first": 0, "middle": int64(len(header) + 7000*len(row))} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			var page Page
			for i := 0; i < b.N; i++ {
				page, err = ReadPage(b.Context(), plan, Position{Path: "large.jsonl", Offset: offset}, 80, nil, false)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(page.BytesRead), "source-B/page")
			b.ReportMetric(float64(plan.Segments[0].End), "source-B/total")
		})
	}
}
func TestHistoryPrefixBoundariesMissingAndCycles(t *testing.T) {
	dir := t.TempDir()
	base := "{\"ordinal\":0,\"type\":\"session_meta\",\"payload\":{\"id\":\"base\"}}\n" + "{\"ordinal\":1,\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"text\":\"前文\"}]}}\n"
	a := source(t, dir, "base.jsonl", base+message("不应继承"))
	meta := fmt.Sprintf("{\"ordinal\":2,\"type\":\"session_meta\",\"payload\":{\"id\":\"child\",\"history_mode\":\"paginated\",\"history_base\":{\"thread_id\":\"base\",\"end_ordinal_exclusive\":2,\"end_byte_offset\":%d}}}\n", len(base))
	b := source(t, dir, "child.jsonl", meta+message("后文"))
	plan := BuildPlan(context.Background(), []Source{a, b}, []string{b.Relative})
	if !plan.Complete || len(plan.Segments) != 2 || plan.Segments[0].End != int64(len(base)) {
		t.Fatalf("history %+v", plan)
	}
	page, err := ReadPage(context.Background(), plan, Position{}, 80, nil, false)
	if err != nil || len(page.Items) != 2 || page.Items[0].Text != "前文" || page.Items[1].Text != "后文" {
		t.Fatalf("history page %+v %v", page, err)
	}
	missing := BuildPlan(context.Background(), []Source{b}, []string{b.Relative})
	if missing.Complete || len(missing.Diagnostics) == 0 {
		t.Fatal("missing history not shown")
	}
	bad := source(t, dir, "bad.jsonl", strings.ReplaceAll(meta, fmt.Sprint(len(base)), "1"))
	if BuildPlan(context.Background(), []Source{a, bad}, []string{bad.Relative}).Complete {
		t.Fatal("bad boundary accepted")
	}
	self := source(t, dir, "self.jsonl", "{\"ordinal\":0,\"type\":\"session_meta\",\"payload\":{\"id\":\"self\",\"history_mode\":\"paginated\",\"history_base\":{\"thread_id\":\"self\",\"end_ordinal_exclusive\":0,\"end_byte_offset\":0}}}\n")
	if BuildPlan(context.Background(), []Source{self}, []string{self.Relative}).Complete {
		t.Fatal("cycle accepted")
	}
}
