// 中文说明：验证合成目录的只读 inventory，不触碰用户真实 .codex。
package application

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"codex-chronicle.local/internal/domain"
	_ "modernc.org/sqlite"
)

func TestScannerDiscoversJSONLAndSQLiteWithoutWriting(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(sessions, "sample.jsonl")
	jsonlContent := "{\"type\":\"session_meta\"}\n{\"type\":\"response_item\"}"
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE sample(id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	beforeEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if !report.ReadOnly || len(report.Files) != 2 || len(report.Errors) != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.Files[0].RelativePath != "sessions/sample.jsonl" {
		t.Fatalf("unexpected sorted first path: %+v", report.Files)
	}
	jsonlFile := report.Files[0]
	if jsonlFile.Status != "stable" || jsonlFile.JSONL == nil || jsonlFile.JSONL.LineCount != 2 || jsonlFile.JSONL.PendingLines != 0 {
		t.Fatalf("unexpected JSONL inventory: %+v", jsonlFile)
	}
	if report.Files[1].Kind != "sqlite" || report.Files[1].Status != "stable" {
		t.Fatalf("unexpected SQLite inventory: %+v", report.Files[1])
	}
	afterEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeEntries) != len(afterEntries) {
		t.Fatalf("scanner created or removed root entries: before=%d after=%d", len(beforeEntries), len(afterEntries))
	}
}

func TestScannerDowngradesSQLiteWithWALDiagnostic(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY); INSERT INTO threads VALUES('thread-wal');"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner()
	initial, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Files) != 1 || initial.Files[0].Status != domain.StatusStable {
		t.Fatalf("ordinary SQLite should initially be stable: %+v", initial.Files)
	}
	auxiliary := map[string][]byte{
		dbPath + "-wal": []byte("wal evidence"),
		dbPath + "-shm": []byte("shm evidence"),
	}
	for path, content := range auxiliary {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	report, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var databaseFile *domain.FileInventory
	for index := range report.Files {
		if report.Files[index].RelativePath == "state.sqlite" {
			databaseFile = &report.Files[index]
			break
		}
	}
	if databaseFile == nil || databaseFile.SQLite == nil {
		t.Fatalf("SQLite inventory missing: %+v", report.Files)
	}
	if databaseFile.Status != domain.StatusUnknown {
		t.Fatalf("SQLite with immutable WAL view was reported as %q: %+v", databaseFile.Status, databaseFile)
	}
	if databaseFile.SQLite.Diagnostic == nil || databaseFile.SQLite.Diagnostic.Code != "SQLITE_IMMUTABLE_WAL_VIEW" {
		t.Fatalf("immutable WAL diagnostic missing: %+v", databaseFile.SQLite)
	}
	for path, expected := range auxiliary {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != string(expected) {
			t.Fatalf("scanner modified SQLite auxiliary file %s: %q", path, content)
		}
	}
	for path := range auxiliary {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	final, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Files) != 1 || final.Files[0].Status != domain.StatusStable {
		t.Fatalf("SQLite should return to stable after WAL/SHM disappear: %+v", final.Files)
	}
}

func TestScannerRefreshDropsThreadsDeletedInWAL(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"deleted", "protected"} {
		data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", id)
		if err := os.WriteFile(filepath.Join(root, id+".jsonl"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT, title TEXT);
		INSERT INTO threads VALUES('deleted','deleted.jsonl','删除目标'),('protected','protected.jsonl','保留对话');
		PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;`); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner()
	initial, err := scanner.ScanWithoutGraphs(t.Context(), root)
	if err != nil || len(initial.Conversations) != 2 {
		t.Fatalf("初始会话读取失败: report=%+v err=%v", initial, err)
	}
	if _, err := writer.Exec(`DELETE FROM threads WHERE id='deleted'; UPDATE threads SET title='更新后的保留对话' WHERE id='protected';`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "deleted.jsonl")); err != nil {
		t.Fatal(err)
	}
	// 重复使用同一扫描器，保证线程缓存也跟随 WAL 提交失效；writer 始终保持运行。
	for attempt := 0; attempt < 2; attempt++ {
		refreshed, err := scanner.ScanWithoutGraphs(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(refreshed.Conversations) != 1 || refreshed.Conversations[0].SourceID != "protected" || refreshed.Conversations[0].Title != "更新后的保留对话" {
			t.Fatalf("刷新仍生成已删除线程的幽灵会话，或损失未删除线程: %+v", refreshed.Conversations)
		}
	}
}

func TestScannerPropagatesPendingJSONLStatusToFileInventory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "active.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"active-thread"}}`+"\n"+`{"type":"pending"`), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 1 || report.Files[0].Status != domain.StatusUnknown {
		t.Fatalf("半截 JSONL 的物理文件仍被报告为稳定：%+v", report.Files)
	}
	if report.Files[0].Rollout == nil || report.Files[0].Rollout.Status != domain.StatusUnknown {
		t.Fatalf("半截 JSONL 的 rollout 状态缺失：%+v", report.Files[0])
	}
}

func TestScannerAggregatesConversationProjection(t *testing.T) {
	root := t.TempDir()
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread-aggregate\",\"cwd\":\"/work\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"查找历史对话\"}]}}\n"
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conversations) != 1 {
		t.Fatalf("expected one conversation, got %+v", report.Conversations)
	}
	conversation := report.Conversations[0]
	if conversation.CWD != "/work" || conversation.UserMessages != 1 || conversation.AssetSetHash == "" {
		t.Fatalf("unexpected conversation summary: %+v", conversation)
	}
	if conversation.AssetGraph == nil || conversation.AssetGraph.Version != 1 || conversation.AssetGraph.Hash == "" || len(conversation.AssetGraph.Nodes) < 2 || len(conversation.AssetGraph.Edges) < 1 {
		t.Fatalf("canonical asset graph missing: %+v", conversation.AssetGraph)
	}
	if conversation.AssetSetHash != conversation.AssetGraph.Hash {
		t.Fatalf("asset set hash is not bound to graph hash: %q vs %q", conversation.AssetSetHash, conversation.AssetGraph.Hash)
	}
	if len(report.Files) != 1 || report.Files[0].Rollout == nil {
		t.Fatalf("rollout projection missing: %+v", report.Files)
	}
}

func TestScannerRetainsDuplicateRolloutPathsWithoutDoubleCounting(t *testing.T) {
	root := t.TempDir()
	primary := filepath.Join(root, "sessions", "2026", "10", "26", "rollout-primary.jsonl")
	backup := filepath.Join(root, "backups", "codex-thread-clone", "rollout-backup.jsonl")
	if err := os.MkdirAll(filepath.Dir(primary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestRollout(t, primary, "thread-duplicate", "重复副本")
	if err := os.WriteFile(backup, mustReadFile(t, primary), 0o644); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(root, "tui-thread-reference-capabilities", "thread-duplicate")
	if err := os.MkdirAll(filepath.Dir(unknown), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conversations) != 1 {
		t.Fatalf("duplicate rollout should remain one logical conversation: %+v", report.Conversations)
	}
	foundUnknownReference := false
	for _, file := range report.Files {
		if file.RelativePath == "tui-thread-reference-capabilities/thread-duplicate" {
			foundUnknownReference = file.Kind == domain.AssetKindUnknown && file.Status == domain.StatusStable && file.SizeBytes == 0
			break
		}
	}
	if !foundUnknownReference {
		t.Fatalf("空 reference 文件没有作为 unknown 资产保留: %+v", report.Files)
	}
	conversation := report.Conversations[0]
	if len(conversation.RolloutPaths) != 2 || conversation.RolloutPaths[0] != "backups/codex-thread-clone/rollout-backup.jsonl" || conversation.RolloutPaths[1] != "sessions/2026/10/26/rollout-primary.jsonl" {
		t.Fatalf("duplicate physical paths were not retained: %+v", conversation.RolloutPaths)
	}
	if conversation.EventCount != 2 || conversation.UserMessages != 1 {
		t.Fatalf("duplicate rollout inflated projection: events=%d users=%d", conversation.EventCount, conversation.UserMessages)
	}
	foundDiagnostic := false
	for _, diagnostic := range conversation.Diagnostics {
		if diagnostic.Code == "ROLLOUT_DUPLICATE_CONTENT" {
			foundDiagnostic = true
			break
		}
	}
	if !foundDiagnostic {
		t.Fatalf("duplicate rollout diagnostic missing: %+v", conversation.Diagnostics)
	}
	if conversation.AssetGraph == nil || len(conversation.AssetGraph.Nodes) < 3 {
		t.Fatalf("asset graph lost duplicate rollout or conversation node: %+v", conversation.AssetGraph)
	}
}

func TestScannerSuppressesBackupPrefixProjection(t *testing.T) {
	root := t.TempDir()
	primary := filepath.Join(root, "sessions", "current.jsonl")
	backup := filepath.Join(root, "backups", "clone", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(primary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestRollout(t, primary, "thread-prefix", "共同历史")
	if err := os.WriteFile(backup, mustReadFile(t, primary), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(primary, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"type\":\"event_msg\",\"payload\":{\"type\":\"context_compacted\"}}\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conversations) != 1 {
		t.Fatalf("expected one prefix conversation: %+v", report.Conversations)
	}
	conversation := report.Conversations[0]
	if conversation.EventCount != 3 || conversation.UserMessages != 1 {
		t.Fatalf("backup prefix was counted twice: events=%d users=%d", conversation.EventCount, conversation.UserMessages)
	}
	foundPrefixDiagnostic := false
	for _, diagnostic := range conversation.Diagnostics {
		if diagnostic.Code == "ROLLOUT_BACKUP_PREFIX" {
			foundPrefixDiagnostic = true
			break
		}
	}
	if !foundPrefixDiagnostic {
		t.Fatalf("backup prefix diagnostic missing: %+v", conversation.Diagnostics)
	}
}

func TestScannerKeepsDifferentBackupRolloutProjection(t *testing.T) {
	root := t.TempDir()
	primary := filepath.Join(root, "sessions", "current.jsonl")
	backup := filepath.Join(root, "backups", "clone", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(primary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestRollout(t, primary, "thread-different-backup", "当前内容")
	writeTestRollout(t, backup, "thread-different-backup", "不同内容")

	report, err := NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conversations) != 1 || report.Conversations[0].EventCount != 4 || report.Conversations[0].UserMessages != 2 {
		t.Fatalf("不同内容的 backup 被错误去重: %+v", report.Conversations)
	}
	for _, diagnostic := range report.Conversations[0].Diagnostics {
		if diagnostic.Code == "ROLLOUT_BACKUP_PREFIX" {
			t.Fatalf("不同内容的 backup 被错误标记为前缀: %+v", diagnostic)
		}
	}
}

func TestDetectBackupRolloutOverlapsReportsProbeBudget(t *testing.T) {
	conversationID := "conversation-budget"
	files := []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "backups/old.jsonl", SizeBytes: maxBackupPrefixProbeBytes + 1}, Rollout: &domain.RolloutSummary{ConversationID: conversationID}},
		{Asset: domain.Asset{RelativePath: "sessions/current.jsonl", SizeBytes: maxBackupPrefixProbeBytes + 2}, Rollout: &domain.RolloutSummary{ConversationID: conversationID}},
	}
	evidence := detectBackupRolloutOverlaps(t.TempDir(), files)
	if evidence["backups/old.jsonl"].Diagnostic.Code != "ROLLOUT_BACKUP_OVERLAP_UNVERIFIED" || evidence["backups/old.jsonl"].Suppress {
		t.Fatalf("超出预算的前缀探测未降级为只读诊断: %+v", evidence)
	}
}

func TestFilesSharePrefixRejectsSymlinkReplacement(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(sessionsDir, "current.jsonl")
	writeTestRollout(t, current, "thread-symlink", "安全边界")
	backupTarget := filepath.Join(root, "backup-target.jsonl")
	writeTestRollout(t, backupTarget, "thread-symlink", "安全边界")
	backup := filepath.Join(backupDir, "old.jsonl")
	if err := os.Symlink(backupTarget, backup); err != nil {
		t.Skipf("当前平台不支持符号链接: %v", err)
	}
	short := domain.FileInventory{Asset: domain.Asset{RelativePath: "backups/old.jsonl", SizeBytes: mustFileSize(t, backupTarget)}, Rollout: &domain.RolloutSummary{ConversationID: "conversation-symlink"}}
	long := domain.FileInventory{Asset: domain.Asset{RelativePath: "sessions/current.jsonl", SizeBytes: mustFileSize(t, current)}, Rollout: &domain.RolloutSummary{ConversationID: "conversation-symlink"}}
	if _, err := filesSharePrefix(root, short, long); err == nil {
		t.Fatal("前缀探测错误跟随符号链接")
	}
}

func TestScannerMergesSQLiteThreadProjectionAndMissingRolloutDiagnostic(t *testing.T) {
	root := t.TempDir()
	rolloutDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(rolloutDir, "one.jsonl")
	if err := os.WriteFile(rollout, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread-a\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT, title TEXT, first_user_message TEXT, cwd TEXT, tokens_used INTEGER, archived INTEGER);
		INSERT INTO threads VALUES('thread-a','sessions/one.jsonl','SQLite 标题','SQLite 首条','/sqlite/project',99,0);
		INSERT INTO threads VALUES('thread-missing','sessions/missing.jsonl','缺失 rollout','', '/sqlite/project',4,0);`)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conversations) != 2 {
		t.Fatalf("expected two conversations, got %+v", report.Conversations)
	}
	var found, missing bool
	for _, conversation := range report.Conversations {
		if conversation.SourceID == "thread-a" {
			found = true
			if conversation.Title != "SQLite 标题" || conversation.CWD != "/sqlite/project" || conversation.TokenUsage.TotalTokens != 99 {
				t.Fatalf("SQLite metadata did not win merge: %+v", conversation)
			}
			if len(conversation.RelatedAssetPaths) != 1 || conversation.RelatedAssetPaths[0] != "state_5.sqlite" {
				t.Fatalf("SQLite asset relation missing: %+v", conversation)
			}
			if conversation.AssetGraph == nil || conversation.AssetGraph.Hash == "" {
				t.Fatalf("SQLite conversation asset graph missing: %+v", conversation.AssetGraph)
			}
		}
		if conversation.SourceID == "thread-missing" {
			missing = true
			if conversation.Status != "conflict" || len(conversation.Diagnostics) == 0 || len(conversation.RelatedAssetPaths) != 1 {
				t.Fatalf("missing rollout was not blocked: %+v", conversation)
			}
		}
	}
	if !found || !missing {
		t.Fatalf("thread projections missing: %+v", report.Conversations)
	}
}

func TestScannerKeepsForkAndAncestorAssetsSeparate(t *testing.T) {
	for _, keepAncestor := range []bool{false, true} {
		t.Run(fmt.Sprintf("ancestor_present=%t", keepAncestor), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			fork := "sessions/fork.jsonl"
			ancestor := "sessions/ancestor.jsonl"
			data := `{"type":"session_meta","payload":{"id":"current-fork","forked_from_id":"ancestor","source":"cli"}}
{"type":"session_meta","payload":{"id":"ancestor","source":"cli"}}
{"type":"event_msg","payload":{"type":"user_message","message":"相同的历史提问"}}
`
			if err := os.WriteFile(filepath.Join(root, fork), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			database, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			if _, err := database.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT, title TEXT);
				INSERT INTO threads VALUES('current-fork','sessions/fork.jsonl','相同的历史提问');`); err != nil {
				t.Fatal(err)
			}
			wantPaths := map[string]string{"current-fork": fork}
			if keepAncestor {
				writeTestRollout(t, filepath.Join(root, ancestor), "ancestor", "相同的历史提问")
				if _, err := database.Exec(`INSERT INTO threads VALUES('ancestor','sessions/ancestor.jsonl','相同的历史提问')`); err != nil {
					t.Fatal(err)
				}
				wantPaths["ancestor"] = ancestor
			}
			scanner := NewScanner()
			for pass := 0; pass < 2; pass++ {
				report, err := scanner.Scan(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				if len(report.Conversations) != len(wantPaths) {
					t.Fatalf("fork 历史复活了不存在的祖先: %+v", report.Conversations)
				}
				for _, conversation := range report.Conversations {
					want, exists := wantPaths[conversation.SourceID]
					if !exists || len(conversation.RolloutPaths) != 1 || conversation.RolloutPaths[0] != want || conversation.ThreadKind == "subagent" || conversation.DescendantCount != 0 {
						t.Fatalf("fork 与祖先共享或混用了资产范围: %+v", conversation)
					}
				}
			}
		})
	}
}

func TestInventoryCanSkipAssetGraphConstruction(t *testing.T) {
	root := t.TempDir()
	content := `{"type":"session_meta","payload":{"id":"graph-optional-thread"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"按需图"}]}}
`
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := NewScanner().ScanWithoutGraphs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conversations) != 1 || report.Conversations[0].AssetGraph != nil || report.Conversations[0].AssetSetHash != "" {
		t.Fatalf("graphless inventory unexpectedly built asset graph: %+v", report.Conversations)
	}
	full, err := NewScanner().Scan(context.Background(), root)
	if err != nil || len(full.Conversations) != 1 || full.Conversations[0].AssetGraph == nil {
		t.Fatalf("full inventory did not build comparison graph: err=%v conversations=%d", err, len(full.Conversations))
	}
	target := BuildConversationAssetGraph(report.Root, report.Files, report.Conversations[0])
	if target.Hash != full.Conversations[0].AssetGraph.Hash || target.Completeness != full.Conversations[0].AssetGraph.Completeness {
		t.Fatalf("targeted graph differs from full scan: targeted=%s/%s full=%s/%s", target.Hash, target.Completeness, full.Conversations[0].AssetGraph.Hash, full.Conversations[0].AssetGraph.Completeness)
	}
}

func TestScannerAssociatesUnboundSQLiteReferencesWithConversationAssets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread-a\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), []byte("{\"id\":\"thread-a\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT); INSERT INTO threads VALUES('thread-a','rollout.jsonl');"); err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	history, err := sql.Open("sqlite", filepath.Join(root, "thread_history_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := history.Exec("CREATE TABLE thread_items(thread_id TEXT, item_json TEXT); INSERT INTO thread_items VALUES('thread-a', '{}');"); err != nil {
		history.Close()
		t.Fatal(err)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conversations) != 1 {
		t.Fatalf("expected one conversation: %+v", report.Conversations)
	}
	conversation := report.Conversations[0]
	if len(conversation.RelatedAssetPaths) != 3 || conversation.RelatedAssetPaths[0] != "session_index.jsonl" || conversation.RelatedAssetPaths[1] != "state_5.sqlite" || conversation.RelatedAssetPaths[2] != "thread_history_1.sqlite" {
		t.Fatalf("unbound SQLite asset was not associated: %+v", conversation)
	}
}

func TestScannerKeepsBackupControlFilesOutOfLiveConversations(t *testing.T) {
	for _, liveState := range []bool{false, true} {
		t.Run(fmt.Sprintf("live_state=%t", liveState), func(t *testing.T) {
			root := t.TempDir()
			backup := filepath.Join(root, "backups", "old-snapshot")
			if err := os.MkdirAll(backup, 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestRollout(t, filepath.Join(root, "rollout.jsonl"), "thread-kept", "当前内容")
			createDB := func(path, statement string) {
				t.Helper()
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if liveState {
				createDB(filepath.Join(root, "state_5.sqlite"), "CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT, title TEXT); INSERT INTO threads VALUES('thread-kept','rollout.jsonl','当前标题');")
			}
			createDB(filepath.Join(backup, "state_5.sqlite"), "CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT, title TEXT); INSERT INTO threads VALUES('thread-kept','old.jsonl','过时标题'),('thread-deleted','deleted.jsonl','已删除');")
			createDB(filepath.Join(backup, "thread_history_1.sqlite"), "CREATE TABLE thread_items(thread_id TEXT, item_json TEXT); INSERT INTO thread_items VALUES('thread-kept','{}'),('thread-deleted','{}');")
			if err := os.WriteFile(filepath.Join(backup, "session_index.jsonl"), []byte("{\"id\":\"thread-kept\"}\n{\"id\":\"thread-deleted\"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := NewScanner().Scan(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Conversations) != 1 || report.Conversations[0].SourceID != "thread-kept" {
				t.Fatalf("备份控制文件使已删除会话复活: %+v", report.Conversations)
			}
			conversation := report.Conversations[0]
			if len(conversation.RolloutPaths) != 1 || conversation.RolloutPaths[0] != "rollout.jsonl" || conversation.Orphan || conversation.Status != domain.StatusStable {
				t.Fatalf("备份控制文件污染当前会话状态或路径: %+v", conversation)
			}
			expectedRelated := 0
			if liveState {
				expectedRelated = 1
				if conversation.Title != "当前标题" || conversation.RelatedAssetPaths[0] != "state_5.sqlite" {
					t.Fatalf("备份覆盖了当前元数据: %+v", conversation)
				}
			}
			if len(conversation.RelatedAssetPaths) != expectedRelated {
				t.Fatalf("备份控制文件被关联到当前会话: %+v", conversation.RelatedAssetPaths)
			}
			if conversation.AssetGraph == nil || conversation.AssetGraph.Completeness != graphComplete {
				t.Fatalf("备份控制记录污染了当前资产图: %+v", conversation.AssetGraph)
			}
			for _, node := range conversation.AssetGraph.Nodes {
				if isBackupRolloutPath(node.RelativePath) {
					t.Fatalf("备份控制记录进入当前资产图: %+v", node)
				}
			}
			expectedFiles := 4 + expectedRelated
			if len(report.Files) != expectedFiles {
				t.Fatalf("备份控制文件仍须保留在文件 inventory: got=%d want=%d", len(report.Files), expectedFiles)
			}
		})
	}
}

func TestScannerRejectsSymlinkRoot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewScanner().Scan(context.Background(), link); err == nil {
		t.Fatal("expected symlink root to be rejected")
	}
}

func TestScannerCacheReusesUnchangedFileAndRefreshesChangedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	writeTestRollout(t, path, "thread-before", "第一次读取")

	scanner := NewScanner()
	first, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Files) != 1 || first.Files[0].Rollout == nil {
		t.Fatalf("initial rollout missing: %+v", first)
	}
	firstProjection := first.Files[0].Rollout

	second, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Files) != 1 || second.Files[0].Rollout != firstProjection {
		t.Fatalf("unchanged file was reparsed instead of using cache: first=%p second=%p", firstProjection, second.Files[0].Rollout)
	}

	writeTestRollout(t, path, "thread-after", "第二次读取，内容长度发生变化")
	third, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Files) != 1 || third.Files[0].Rollout == nil || third.Files[0].Rollout.SourceID != "thread-after" {
		t.Fatalf("changed file did not refresh cache: %+v", third.Files)
	}
	if third.Files[0].Rollout == firstProjection {
		t.Fatal("changed file unexpectedly reused the old rollout projection")
	}
}

func TestScannerProgressReportsCacheStatistics(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	writeTestRollout(t, path, "thread-progress", "进度统计")
	scanner := NewScanner()

	var firstProgress ScanProgress
	if _, err := scanner.ScanWithProgress(context.Background(), root, func(progress ScanProgress) {
		firstProgress = progress
	}); err != nil {
		t.Fatal(err)
	}
	if firstProgress.FilesParsed != 1 || firstProgress.CacheHits != 0 {
		t.Fatalf("unexpected first scan cache statistics: %+v", firstProgress)
	}

	var secondProgress ScanProgress
	if _, err := scanner.ScanWithProgress(context.Background(), root, func(progress ScanProgress) {
		secondProgress = progress
	}); err != nil {
		t.Fatal(err)
	}
	if secondProgress.FilesParsed != 0 || secondProgress.CacheHits != 1 {
		t.Fatalf("unexpected cached scan statistics: %+v", secondProgress)
	}
}

func TestScannerCacheRejectsOldFileAfterSamePathReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	writeTestRollout(t, path, "thread-old", "旧内容")

	scanner := NewScanner()
	if _, err := scanner.Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	cached, ok := scanner.state.cache[path]
	if !ok {
		t.Fatal("expected cached rollout")
	}

	replacement := path + ".replacement"
	writeTestRollout(t, replacement, "thread-new", "新文件内容")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(cached.info, newInfo) {
		t.Skip("当前平台无法区分替换前后的文件身份")
	}

	report, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 1 || report.Files[0].Rollout == nil || report.Files[0].Rollout.SourceID != "thread-new" {
		t.Fatalf("same-path replacement returned stale cached projection: %+v", report.Files)
	}
}

func TestScannerCacheDoesNotReturnDeletedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	writeTestRollout(t, path, "thread-delete", "待删除内容")
	scanner := NewScanner()
	if _, err := scanner.Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	report, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 0 || len(report.Conversations) != 0 {
		t.Fatalf("deleted file was returned from cache: %+v", report)
	}
}

func TestScannerCacheSupportsConcurrentScans(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 8; index++ {
		writeTestRollout(t, filepath.Join(root, fmt.Sprintf("rollout-%d.jsonl", index)), fmt.Sprintf("thread-%d", index), "并发扫描")
	}
	scanner := NewScanner()
	const workers = 8
	errs := make(chan error, workers)
	var waitGroup sync.WaitGroup
	for index := 0; index < workers; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			report, err := scanner.Scan(context.Background(), root)
			if err != nil {
				errs <- err
				return
			}
			if len(report.Files) != 8 || len(report.Conversations) != 8 {
				errs <- fmt.Errorf("unexpected concurrent scan result: files=%d conversations=%d", len(report.Files), len(report.Conversations))
			}
		}()
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func writeTestRollout(t *testing.T, path, threadID, message string) {
	t.Helper()
	content := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":%q}]}}\n", threadID, message)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func mustFileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
