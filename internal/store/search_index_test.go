// 中文说明：用合成 JSONL 验证压缩检索、空间硬上限、增量游标、重启续建及仅清理派生数据的边界。
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/domain"
)

func TestSearchContinuesInSourceWhenIndexClearsDuringVerification(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	paths := []string{"rollout-a.jsonl", "rollout-b.jsonl"}
	for _, path := range paths {
		writeSearchFixture(t, root, path, 180)
	}
	report := scanSearchFixture(t, root)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.BuildSearchIndex(ctx, report, nil); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"中文搜索", "搜索"} {
		want, err := application.SearchJSONLFiles(ctx, root, paths, query, 175)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		// 只限制可能互锁的搜索与清理，不让测试数据构建消耗超时预算。
		searchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		got, err := s.SearchWithFilesFiltered(searchCtx, root, query, 175, report.Files, func(hit domain.SearchHit) (domain.SearchHit, bool, error) {
			n++
			if n == 1 {
				return hit, true, s.ClearSearchIndex(searchCtx)
			}
			return hit, true, nil
		})
		cancel()
		if err != nil || len(got) != len(want) {
			t.Fatalf("搜索持有数据库连接，或清理后未继续搜索：%q %d/%d %v", query, len(got), len(want), err)
		}
		for i := range got {
			got[i].LiveConsistency, want[i].LiveConsistency = "", ""
			got[i].SourceStatus, want[i].SourceStatus = "", ""
			got[i].ActivityStatus, want[i].ActivityStatus = "", ""
			got[i].MatchOrigin, want[i].MatchOrigin = "", ""
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("清理索引后有漏行、重复或字节定位变化：%q", query)
		}
		if _, err := s.BuildSearchIndex(ctx, report, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSearchCandidateBatchesAvoidGlobalSort(t *testing.T) {
	s, err := Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.index.prepareProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{shortCandidateSQL, []any{1, 0, searchCandidateBatch}},
		{fmt.Sprintf(termCandidateSQL, "terms"), []any{"abc", 0, 1000, 1, searchCandidateBatch}},
		{fmt.Sprintf(termCandidateSQL, "han_terms"), []any{"1234", 0, 1000, 1, searchCandidateBatch}},
	} {
		rows, err := s.index.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.sql, query.args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(detail, "TEMP B-TREE") {
				rows.Close()
				t.Fatalf("候选查询引入了额外排序：%s", detail)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSearchUsesCompleteIndexToSkipFilesWithoutCandidates(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	writeSearchFixture(t, root, "rollout-absence.jsonl", 800)
	report := scanSearchFixture(t, root)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.BuildSearchIndex(ctx, report, nil); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"qzjxv-never-present", "绝无"} {
		var indexedProgress, sourceBytes int64
		hits, err := s.SearchWithFilesObserved(ctx, root, query, 20, report.Files, nil, func(progress application.SearchProgress) error {
			indexedProgress += progress.RecordsDone
			sourceBytes += progress.SourceBytesRead
			return nil
		})
		if err != nil || len(hits) != 0 || indexedProgress != 0 || sourceBytes != 0 {
			t.Fatalf("完整索引已排除候选，不应逐行重扫源文件：%q hits=%d records=%d bytes=%d err=%v", query, len(hits), indexedProgress, sourceBytes, err)
		}
	}
	if err := s.ClearSearchIndex(ctx); err != nil {
		t.Fatal(err)
	}
	var sourceProgress int64
	hits, err := s.SearchWithFilesObserved(ctx, root, "qzjxv-never-present", 20, report.Files, nil, func(progress application.SearchProgress) error {
		sourceProgress += progress.RecordsDone
		return nil
	})
	if err != nil || len(hits) != 0 || sourceProgress == 0 {
		t.Fatalf("索引缺失时应回退检查源文件：hits=%d records=%d err=%v", len(hits), sourceProgress, err)
	}
}

func writeSearchFixture(t *testing.T, root, name string, count int) []byte {
	t.Helper()
	var content strings.Builder
	fmt.Fprintf(&content, "{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", name)
	for i := 0; i < count; i++ {
		var words strings.Builder
		for j := 0; j < 20; j++ {
			hash := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", i, j)))
			words.WriteString(hex.EncodeToString(hash[:8]))
			words.WriteByte(' ')
		}
		text := fmt.Sprintf("第%d轮 中文搜索 第一行\n第二行 unique%d ", i, i) + words.String() + strings.Repeat("可重复但必须无损保存的正文。", 24)
		payload := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": text}}
		line, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		content.Write(line)
		content.WriteByte('\n')
	}
	data := []byte(content.String())
	if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func scanSearchFixture(t *testing.T, root string) domain.InventoryReport {
	t.Helper()
	report, err := application.NewScanner().ScanWithoutGraphs(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestSearchMergesFullLongRecordsWithSourceFallback(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	paths := []string{"rollout-a.jsonl", "rollout-b.jsonl", "rollout-c.jsonl"}
	var noise strings.Builder
	for i := 0; i < 600; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprint(i)))
		noise.WriteString(hex.EncodeToString(sum[:]))
	}
	for _, name := range paths {
		content := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", name) +
			"{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"前段\\u0000" +
			strings.Repeat("长文", 12000) + noise.String() + " 跨越旧上限\\u56de收站\"}}\n"
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report := scanSearchFixture(t, root)
	var partial domain.InventoryReport = report
	partial.Files = nil
	for _, file := range report.Files {
		if file.RelativePath == paths[1] {
			partial.Files = append(partial.Files, file)
		}
	}
	for _, variant := range []domain.InventoryReport{partial, report} {
		if _, err := s.BuildSearchIndex(ctx, variant, nil); err != nil {
			t.Fatal(err)
		}
		var references int
		if err := s.index.db.QueryRowContext(ctx, "SELECT count(*) FROM documents WHERE length(text)=0").Scan(&references); err != nil || references != len(variant.Files) {
			t.Fatalf("超长正文回读分支未被覆盖：%d %v", references, err)
		}
		for _, query := range []string{"跨越旧上限回收站", "回收", "收", "前段\x00长", "没有该正文"} {
			for _, limit := range []int{2, 10} {
				got, err := s.SearchWithFiles(ctx, root, query, limit, report.Files)
				if err != nil {
					t.Fatal(err)
				}
				want, err := application.SearchJSONLFiles(ctx, root, paths, query, limit)
				if err != nil || len(got) != len(want) {
					t.Fatalf("覆盖不一致：%q %d != %d (%v)", query, len(got), len(want), err)
				}
				for i, hit := range want {
					actual := got[i]
					if actual.RelativePath != hit.RelativePath || actual.Line != hit.Line || actual.StartOffset != hit.StartOffset || actual.EndOffset != hit.EndOffset || actual.Snippet != hit.Snippet {
						t.Fatalf("顺序或定位不一致：%q %q:%d != %q:%d", query, actual.RelativePath, actual.Line, hit.RelativePath, hit.Line)
					}
				}
				if query == "跨越旧上限回收站" && len(got) != min(limit, len(paths)) {
					t.Fatalf("未命中长记录末尾：%d", len(got))
				}
			}
		}
	}
	// 即使调用方还在使用旧目录快照，源文件追加也要立即回退源文件。
	appended, err := os.OpenFile(filepath.Join(root, paths[1]), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = appended.WriteString("{\"type\":\"event_msg\",\"payload\":{\"message\":\"新追加的搜索词\"}}\n")
	if closeErr := appended.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	hits, err := s.SearchWithFiles(ctx, root, "新追加的搜索词", 10, report.Files)
	if err != nil || len(hits) != 1 || hits[0].Line != 3 || hits[0].RelativePath != paths[1] {
		t.Fatalf("旧快照遮住了新内容: %v %v", hits, err)
	}
}

func TestOldSearchProjectionFallsBackUntilRebuilt(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	writeSearchFixture(t, root, "rollout-old.jsonl", 10)
	report := scanSearchFixture(t, root)
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SyncInventoryWithoutSearch(ctx, report); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildSearchIndex(ctx, report, nil); err != nil {
		t.Fatal(err)
	}
	settings := SearchIndexSettings{BudgetBytes: 32 << 20}
	if err := s.SetSearchIndexSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	// 模拟只有三字词表的旧版加速库，再走一次正常的打开路径。
	if _, err := s.index.db.ExecContext(ctx, "DROP TABLE han_terms; PRAGMA user_version=1; VACUUM;"); err != nil {
		t.Fatal(err)
	}
	var oldPages int
	if err := s.index.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&oldPages); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var reopenedPages int
	if err := s.index.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&reopenedPages); err != nil || reopenedPages != oldPages {
		t.Fatalf("仅打开旧库便占用额外索引预算：%d/%d %v", reopenedPages, oldPages, err)
	}
	if ready, err := s.HasSearchIndex(ctx, root); err != nil || ready {
		t.Fatalf("沿用了旧搜索投影：%v %v", ready, err)
	}
	status, err := s.SearchIndexStatus(ctx, root)
	if err != nil || !status.RebuildRequired || status.IndexedFiles != 0 || status.Documents != 0 || status.Settings != settings {
		t.Fatalf("旧索引状态不正确：%+v %v", status, err)
	}
	hits, err := s.Search(ctx, root, "搜索", 20)
	if err != nil || len(hits) != 10 || hits[0].LiveConsistency == "" {
		t.Fatalf("升级后未走源文件搜索：%+v %v", hits, err)
	}
	progress, err := s.BuildSearchIndex(ctx, report, nil)
	if err != nil || progress.DocumentsAdded != 11 || progress.BudgetReached {
		t.Fatalf("旧索引重建失败：%+v %v", progress, err)
	}
	status, err = s.SearchIndexStatus(ctx, root)
	if err != nil || status.RebuildRequired || status.Documents != 11 || status.IndexedFiles != 1 || status.Settings != settings {
		t.Fatalf("重建后状态不正确：%+v %v", status, err)
	}
	after, err := s.Search(ctx, root, "搜索", 20)
	if err != nil || len(after) != len(hits) {
		t.Fatalf("两字索引重建后丢失结果：%d/%d %v", len(after), len(hits), err)
	}
	for i := range after {
		if after[i].StartOffset != hits[i].StartOffset || after[i].EndOffset != hits[i].EndOffset || after[i].Snippet != hits[i].Snippet {
			t.Fatalf("重建改变了命中定位或片段：%+v / %+v", after[i], hits[i])
		}
	}
}

func TestCompactIndexKeepsSearchAndUsesLessSpace(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	source := writeSearchFixture(t, root, "rollout-space.jsonl", 600)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings, err := s.SearchIndexSettings(ctx)
	if err != nil || settings.Automatic {
		t.Fatalf("新工作区应默认手动构建: %+v %v", settings, err)
	}
	report := scanSearchFixture(t, root)
	if _, err := s.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	if indexed, err := s.HasSearchIndex(ctx, root); err != nil || indexed {
		t.Fatalf("目录同步不应自动构建正文索引: %v %v", indexed, err)
	}
	first, err := s.BuildSearchIndex(ctx, report, nil)
	if err != nil || first.DocumentsAdded != 601 {
		t.Fatalf("build: %+v %v", first, err)
	}
	for _, query := range []string{"unique519", "中文搜索", "文搜索", "第一行   第二行", "可重复但必须"} {
		indexed, err := s.SearchWithFiles(ctx, root, query, 20, report.Files)
		if err != nil || len(indexed) == 0 {
			t.Fatalf("indexed search %q: %+v %v", query, indexed, err)
		}
		live, err := application.SearchJSONLFiles(ctx, root, []string{"rollout-space.jsonl"}, query, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(indexed) != len(live) {
			t.Fatalf("query %q count changed: %d != %d", query, len(indexed), len(live))
		}
		for i := range live {
			if indexed[i].StartOffset != live[i].StartOffset || indexed[i].EndOffset != live[i].EndOffset || indexed[i].Line != live[i].Line {
				t.Fatalf("定位证据变化: %+v != %+v", indexed[i], live[i])
			}
		}
	}
	second, err := s.BuildSearchIndex(ctx, report, nil)
	if err != nil || second.DocumentsAdded != 0 {
		t.Fatalf("重复构建不应新增记录: %+v %v", second, err)
	}
	// 用旧版的两份正文和普通 FTS 表建立相同合成数据的空间对照。
	legacy, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(`CREATE VIRTUAL TABLE terms USING fts5(content); CREATE TABLE snippets(text TEXT);`); err != nil {
		t.Fatal(err)
	}
	rows, err := s.index.db.QueryContext(ctx, "SELECT text FROM documents")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := legacy.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		text, err := s.index.decoder.DecodeAll(b, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO terms VALUES(?)", string(text)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO snippets VALUES(?)", string(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var pages, pageSize int64
	if err := legacy.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := legacy.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	status, err := s.SearchIndexStatus(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	legacyBytes := pages * pageSize
	t.Logf("合成源 %d bytes；旧版重复正文+FTS %d bytes；压缩独立索引 %d bytes；降低 %.1f%%", len(source), legacyBytes, status.DatabaseBytes, 100*(1-float64(status.DatabaseBytes)/float64(legacyBytes)))
	if status.DatabaseBytes >= legacyBytes*8/10 {
		t.Fatalf("压缩空间收益不足: %+v legacy=%d", status, legacyBytes)
	}
	actual, err := os.ReadFile(filepath.Join(root, "rollout-space.jsonl"))
	if err != nil || string(actual) != string(source) {
		t.Fatal("构建修改了源文件", err)
	}
}

func TestIndexBudgetStopsAndResumesAfterRestart(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	source := writeSearchFixture(t, root, "rollout-budget.jsonl", 3200)
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	report := scanSearchFixture(t, root)
	if _, err := s.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSearchIndexSettings(ctx, SearchIndexSettings{BudgetBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	p, err := s.BuildSearchIndex(ctx, report, nil)
	if err != nil || !p.BudgetReached {
		t.Fatalf("未在预算处停止: %+v %v", p, err)
	}
	status, err := s.SearchIndexStatus(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if status.DatabaseBytes > 1<<20 || status.Documents == 0 || status.Documents >= 3201 {
		t.Fatalf("预算未生效或未保留提交批次: %+v", status)
	}
	if status.LegacyBytes != 0 {
		t.Fatalf("空的旧搜索表不应显示为待清理正文: %+v", status)
	}
	// 未覆盖的末尾内容仍由只读扫描找到。
	hits, err := s.SearchWithFiles(ctx, root, "unique3199", 10, report.Files)
	if err != nil || len(hits) != 1 {
		t.Fatalf("预算导致漏检: %+v %v", hits, err)
	}
	before := status.Documents
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings, err := s.SearchIndexSettings(ctx)
	if err != nil || settings.BudgetBytes != 1<<20 {
		t.Fatalf("设置未持久化: %+v %v", settings, err)
	}
	if err := s.SetSearchIndexSettings(ctx, SearchIndexSettings{BudgetBytes: 16 << 20}); err != nil {
		t.Fatal(err)
	}
	p, err = s.BuildSearchIndex(ctx, report, nil)
	if err != nil || p.BudgetReached || p.DocumentsAdded != 3201-before {
		t.Fatalf("没有从已提交游标续建: %+v before=%d %v", p, before, err)
	}
	if err := s.SetSearchIndexSettings(ctx, SearchIndexSettings{BudgetBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	status, err = s.SearchIndexStatus(ctx, root)
	if err != nil || !status.OverBudget || status.Documents != 3201 {
		t.Fatalf("缩小预算不应自动删除: %+v %v", status, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO search_documents(asset_id,source_path,role,content) VALUES('legacy','rollout-budget.jsonl','user',?)`, strings.Repeat("旧版搜索正文", 5000)); err != nil {
		t.Fatal(err)
	}
	status, err = s.SearchIndexStatus(ctx, root)
	if err != nil || status.LegacyBytes == 0 {
		t.Fatalf("未识别旧版搜索数据: %+v %v", status, err)
	}
	if err := s.ClearSearchIndex(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = s.SearchIndexStatus(ctx, root)
	if err != nil || status.Documents != 0 || status.DatabaseBytes > 1<<20 || status.Settings.Automatic || status.LegacyBytes != 0 {
		t.Fatalf("清理未回收空间或未暂停: %+v %v", status, err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "rollout-budget.jsonl"))
	if err != nil || string(actual) != string(source) {
		t.Fatal("预算/清理修改了源文件", err)
	}
	start, _, err := s.ReaderFrame(ctx, root, "rollout-budget.jsonl", int64(len(source)-2), 0)
	if err != nil || start == 0 {
		t.Fatalf("清理破坏阅读定位: %d %v", start, err)
	}
}

func TestIndexSubstringRecallAndSnapshotFingerprint(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	name := "rollout-recall.jsonl"
	var content strings.Builder
	content.WriteString("{\"type\":\"session_meta\",\"payload\":{\"id\":\"recall\"}}\n")
	for _, text := range []string{"中文搜索", "关于中文搜索的实践", "x中文搜索y", "搜 索", "𠮷野", "unique519", "prefixunique519suffix", "two words", "abc\"def", "x\x00y", "oldneedle"} {
		line, err := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"message": text}})
		if err != nil {
			t.Fatal(err)
		}
		content.Write(line)
		content.WriteByte('\n')
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content.String()), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report := scanSearchFixture(t, root)
	if _, err := s.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildSearchIndex(ctx, report, nil); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"中文搜索", "搜索", "𠮷野", "文", "nique519", "unique519", "two   words", `abc"def`, "x\x00y", "notfound"} {
		indexed, err := s.SearchWithFiles(ctx, root, query, 100, report.Files)
		if err != nil {
			t.Fatal(query, err)
		}
		live, err := application.SearchJSONLFiles(ctx, root, []string{name}, query, 100)
		if err != nil || len(indexed) != len(live) {
			t.Fatalf("子串召回不同: %q indexed=%+v live=%+v err=%v", query, indexed, live, err)
		}
		for i := range live {
			if indexed[i].Line != live[i].Line || indexed[i].StartOffset != live[i].StartOffset || indexed[i].EndOffset != live[i].EndOffset {
				t.Fatalf("子串定位不同: %q %+v %+v", query, indexed[i], live[i])
			}
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(content.String(), "oldneedle", "newneedle")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	report = scanSearchFixture(t, root)
	if _, err := s.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	status, err := s.SearchIndexStatus(ctx, root)
	if err != nil || status.IndexedFiles != 0 {
		t.Fatalf("同尺寸同时间的重写未失效: %+v %v", status, err)
	}
	for _, query := range []string{"oldneedle", "newneedle"} {
		hits, err := s.Search(ctx, root, query, 100)
		if err != nil || (query == "oldneedle" && len(hits) != 0) || (query == "newneedle" && len(hits) != 1) {
			t.Fatalf("失效后没有回退源搜索: %q %+v %v", query, hits, err)
		}
	}
}

func TestIndexStopAfterCommitResumes(t *testing.T) {
	root := t.TempDir()
	writeSearchFixture(t, root, "rollout-stopped.jsonl", 400)
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	report := scanSearchFixture(t, root)
	if _, err := s.SyncInventory(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p, err := s.BuildSearchIndex(ctx, report, func(p SearchIndexProgress) {
		if p.DocumentsAdded >= 128 {
			cancel()
		}
	})
	if err == nil || p.DocumentsAdded != 128 || p.BytesCovered <= 0 {
		t.Fatalf("未在提交后停止: %+v %v", p, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restored, err := s.BuildSearchIndex(t.Context(), report, nil)
	if err != nil || restored.DocumentsAdded != 401-p.DocumentsAdded {
		t.Fatalf("停止后不能按游标续建: %+v %v", restored, err)
	}
	status, err := s.SearchIndexStatus(t.Context(), root)
	if err != nil || status.Documents != 401 || status.IndexedFiles != 1 {
		t.Fatalf("续建产生重复或遗漏: %+v %v", status, err)
	}
}

func TestSearchCommittedPrefixUsesPhysicalOffsetsAndRechecksRewrites(t *testing.T) {
	root := t.TempDir()
	const name = "rollout-live-prefix.jsonl"
	source := writeSearchFixture(t, root, name, 160)
	path := filepath.Join(root, name)
	s, err := Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report := scanSearchFixture(t, root)
	buildCtx, stop := context.WithCancel(t.Context())
	defer stop()
	build, err := s.BuildSearchIndex(buildCtx, report, func(p SearchIndexProgress) { stop() })
	if err == nil || build.DocumentsAdded != 128 || build.FilesDone != 0 {
		t.Fatalf("应只发布第一个批次：%+v %v", build, err)
	}
	status, err := s.SearchIndexStatusWithFiles(t.Context(), root, report.Files)
	if err != nil || status.IndexedFiles != 0 || status.CoveredBytes != 0 || status.PartialFiles != 1 || status.PartialBytes != build.BytesCovered {
		t.Fatalf("前缀不能算成整文件覆盖：%+v %v", status, err)
	}
	check := func(query string, wantCount int, filter application.SearchHitFilter) (sourceBytes, prefixBytes int64) {
		t.Helper()
		got, err := s.SearchWithFilesObserved(t.Context(), root, query, 500, report.Files, filter, func(progress application.SearchProgress) error {
			sourceBytes += progress.SourceBytesRead
			prefixBytes += progress.PrefixBytesRead
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want, err := application.SearchJSONLFiles(t.Context(), root, []string{name}, query, 500)
		if err != nil || len(want) != wantCount || len(got) != len(want) {
			t.Fatalf("查询 %q 命中数不一致：%d / %d（原文期望 %d）%v", query, len(got), len(want), wantCount, err)
		}
		for i := range got {
			got[i].LiveConsistency, got[i].SourceStatus, got[i].ActivityStatus = "", "", ""
			want[i].LiveConsistency, want[i].SourceStatus, want[i].ActivityStatus = "", "", ""
			got[i].MatchOrigin, want[i].MatchOrigin = "", ""
			if got[i] != want[i] {
				t.Fatalf("查询 %q 第 %d 个命中的定位或片段不同", query, i)
			}
		}
		return
	}
	for _, query := range []string{"中文搜索", "搜索", "搜"} {
		if read, prefix := check(query, 160, nil); read != int64(len(source))+1 || prefix != build.BytesCovered+1 {
			t.Fatalf("应验证一次前缀并只扫描余下正文：%s %d / %d", query, read, prefix)
		}
	}
	check("unique126", 1, nil)
	check("unique127", 1, nil)
	check("找不到的单词", 0, nil)

	// 保持 inode、大小与 mtime 相同的改写必须被指纹发现，旧索引不能跳过新词。
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(source, []byte("unique126"), []byte("changed00"), 1)
	if err = os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if read, prefix := check("changed00", 1, nil); read <= prefix || prefix != build.BytesCovered+1 {
		t.Fatalf("指纹不同时应回退原文：%d / %d", read, prefix)
	}
	check("unique126", 0, nil)

	if err = os.WriteFile(path, append(append([]byte(nil), source...), []byte(`{"type":"event_msg","payload":{"message":"pendingneedle`)...), 0600); err != nil {
		t.Fatal(err)
	}
	check("pendingneedle", 0, nil)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\"}}\n")
	if closeErr := f.Close(); closeErr != nil || err != nil {
		t.Fatal(err, closeErr)
	}
	check("pendingneedle", 1, nil)

	cleared := false
	check("中文搜索", 160, func(hit domain.SearchHit) (domain.SearchHit, bool, error) {
		if !cleared {
			cleared = true
			return hit, true, s.ClearSearchIndex(t.Context())
		}
		return hit, true, nil
	})
	if err := os.Rename(path, filepath.Join(root, "moved-out-of-scope")); err != nil {
		t.Fatal(err)
	}
	check("中文搜索", 0, nil)
}

func TestIndexIncrementalAppendRewriteAndPendingTail(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	writeSearchFixture(t, root, "rollout-live.jsonl", 140)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report := scanSearchFixture(t, root)
	if _, err := s.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildSearchIndex(ctx, report, nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "rollout-live.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"type\":\"event_msg\",\"payload\":{\"message\":\"newneedle\"}}\n{\"type\":"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	report = scanSearchFixture(t, root)
	result, err := s.BuildSearchIndex(ctx, report, nil)
	if err != nil || result.DocumentsAdded != 1 {
		t.Fatalf("增量追加/半行: %+v %v", result, err)
	}
	if _, err := s.BuildSearchIndex(ctx, report, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"rewritten\"}}\n{\"type\":\"event_msg\",\"payload\":{\"message\":\"replacementneedle\"}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report = scanSearchFixture(t, root)
	if _, err := s.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	result, err = s.BuildSearchIndex(ctx, report, nil)
	if err != nil || result.DocumentsAdded != 2 {
		t.Fatalf("重写未失效: %+v %v", result, err)
	}
	hits, err := s.Search(ctx, root, "newneedle", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("保留了旧正文: %+v %v", hits, err)
	}
	hits, err = s.Search(ctx, root, "replacementneedle", 10)
	if err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.BuildSearchIndex(cancelled, report, nil); err == nil {
		t.Fatal("取消上下文未生效")
	}
}
