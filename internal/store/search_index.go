// 中文说明：将可重建的搜索加速数据与目录库分离，压缩文字、限制 SQLite 文件大小并按已提交游标增量构建。
package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/searchtext"
	"github.com/klauspost/compress/zstd"
	"modernc.org/sqlite"
)

const DefaultSearchIndexBudget int64 = 512 << 20
const searchProjectionVersion = 3
const maxDecodedSearchText = 8 << 20
const maxCompressedSearchText = 16 << 10

const clearSearchProjectionSQL = `
	INSERT INTO terms(terms) VALUES('delete-all');
	DELETE FROM documents;
	DELETE FROM files;
	CREATE VIRTUAL TABLE IF NOT EXISTS han_terms USING fts5(content, content='', contentless_delete=1, tokenize='ascii', detail=none);
	INSERT INTO han_terms(han_terms) VALUES('delete-all');`

// SearchIndexSettings 控制搜索加速文件；不限制原始对话或会话目录库。
type SearchIndexSettings struct {
	BudgetBytes   int64     `json:"budget_bytes"`
	Automatic     bool      `json:"automatic"`
	Content       *[]string `json:"content,omitempty"`
	RecentContext *bool     `json:"recent_context,omitempty"`
}

type indexContentPolicy struct {
	Content       []string `json:"content"`
	RecentContext bool     `json:"recent_context"`
}

// 旧库使用空字符串或类别数组；只有启用窗口限制才写入包含两项设置的对象。
func decodeIndexContentPolicy(encoded string) (policy indexContentPolicy, err error) {
	if encoded == "" {
		return policy, nil
	}
	if strings.HasPrefix(encoded, "[") {
		err = json.Unmarshal([]byte(encoded), &policy.Content)
	} else {
		err = json.Unmarshal([]byte(encoded), &policy)
	}
	return policy, err
}

func (policy indexContentPolicy) encode() (string, error) {
	if policy.Content == nil && !policy.RecentContext {
		return "", nil
	}
	var value any = policy.Content
	if policy.RecentContext {
		value = policy
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

type SearchIndexStatus struct {
	Settings        SearchIndexSettings `json:"settings"`
	Path            string              `json:"path"`
	DatabaseBytes   int64               `json:"database_bytes"`
	UsedBytes       int64               `json:"used_bytes"`
	Reclaimable     int64               `json:"reclaimable_bytes"`
	Documents       int64               `json:"documents"`
	IndexedFiles    int64               `json:"indexed_files"`
	CatalogFiles    int64               `json:"catalog_files"`
	CoveredBytes    int64               `json:"covered_bytes"`
	PartialFiles    int                 `json:"partial_files"`
	PartialBytes    int64               `json:"partial_bytes"`
	SourceBytes     int64               `json:"source_bytes"`
	OverBudget      bool                `json:"over_budget"`
	LegacyBytes     int64               `json:"legacy_bytes"`
	RebuildRequired bool                `json:"rebuild_required"`
}

type SearchIndexProgress struct {
	FilesDone      int   `json:"files_done"`
	FilesTotal     int   `json:"files_total"`
	DocumentsAdded int64 `json:"documents_added"`
	BytesCovered   int64 `json:"bytes_covered"`
	BudgetReached  bool  `json:"budget_reached"`
}

type searchIndex struct {
	db      *sql.DB
	path    string
	mu      sync.Mutex
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func (s *Store) openSearchIndex(ctx context.Context) error {
	path := ":memory:"
	if s.path != ":memory:" {
		path = s.path + ".search.db"
		if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("搜索索引必须是普通文件")
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	dsn, err := prepareDSN(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	index := &searchIndex{db: db, path: path}
	fail := func(err error) error { _ = db.Close(); return err }
	// 独立搜索库使用短事务与回滚日志，不产生长期增长的 WAL；设置和清理都由 SQLite 正常维护文件。
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=DELETE; PRAGMA temp_store=FILE;
		CREATE TABLE IF NOT EXISTS settings(id INTEGER PRIMARY KEY CHECK(id=1), budget_bytes INTEGER NOT NULL, automatic INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS content_scope(id INTEGER PRIMARY KEY CHECK(id=1), desired TEXT NOT NULL, built TEXT NOT NULL);
		INSERT OR IGNORE INTO content_scope VALUES(1,'','');
		CREATE TABLE IF NOT EXISTS files(id INTEGER PRIMARY KEY, root TEXT NOT NULL, path TEXT NOT NULL, conversation_id TEXT NOT NULL,
			committed INTEGER NOT NULL DEFAULT 0, lines INTEGER NOT NULL DEFAULT 0, prefix_hash TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT 0, mtime INTEGER NOT NULL DEFAULT 0, complete INTEGER NOT NULL DEFAULT 0,
			window_start INTEGER NOT NULL DEFAULT 0, UNIQUE(root,path));
		CREATE TABLE IF NOT EXISTS documents(id INTEGER PRIMARY KEY, file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
			line INTEGER NOT NULL, start_offset INTEGER NOT NULL, end_offset INTEGER NOT NULL, top_type TEXT NOT NULL, text BLOB NOT NULL,
			window_limited INTEGER NOT NULL DEFAULT 0);
		CREATE INDEX IF NOT EXISTS documents_file_idx ON documents(file_id);
		CREATE VIRTUAL TABLE IF NOT EXISTS terms USING fts5(content, content='', contentless_delete=1, tokenize='trigram', detail=none);`); err != nil {
		return fail(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT OR IGNORE INTO settings VALUES(1,?,0)", DefaultSearchIndexBudget); err != nil {
		return fail(err)
	}
	for _, column := range []struct{ table, name string }{{"files", "window_start"}, {"documents", "window_limited"}} {
		var exists bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)", column.table, column.name).Scan(&exists); err != nil {
			return fail(err)
		}
		if !exists {
			if _, err := db.ExecContext(ctx, "ALTER TABLE "+column.table+" ADD COLUMN "+column.name+" INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fail(err)
			}
		}
	}
	var version int
	var built string
	if err := db.QueryRowContext(ctx, "SELECT user_version,built FROM pragma_user_version,content_scope WHERE id=1").Scan(&version, &built); err != nil {
		return fail(err)
	}
	if version == 2 {
		policy, err := decodeIndexContentPolicy(built)
		if err != nil {
			return fail(err)
		}
		// 只有旧的最近窗口索引遗漏界外工具记录；已有全历史索引无须重建。
		if !policy.RecentContext {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", searchProjectionVersion)); err != nil {
				return fail(err)
			}
		}
	}
	index.encoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(64<<10))
	if err != nil {
		return fail(err)
	}
	index.decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxDecodedSearchText))
	if err != nil {
		index.encoder.Close()
		return fail(err)
	}
	s.index = index
	return nil
}

func (s *Store) SearchIndexPath() string { return s.index.path }

func (index *searchIndex) prepareProjection(ctx context.Context) error {
	var version int
	var desired, built string
	if err := index.db.QueryRowContext(ctx, `SELECT user_version,desired,built FROM pragma_user_version,content_scope WHERE id=1`).Scan(&version, &desired, &built); err != nil {
		return err
	}
	if version == searchProjectionVersion && desired == built {
		return nil
	}
	if version > searchProjectionVersion {
		return fmt.Errorf("搜索索引由更新版本生成，请升级后重新构建")
	}
	// 先释放旧派生记录的页，再创建两字词表。打开旧库时不占用额外预算。
	tx, err := index.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, clearSearchProjectionSQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", searchProjectionVersion)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE content_scope SET built=desired WHERE id=1"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if desired != built {
		// 更换范围时释放旧索引的页，再按新预算开始构建。
		_, err := index.db.ExecContext(ctx, "VACUUM")
		return err
	}
	return nil
}

func (s *Store) resetSearchIndex(ctx context.Context) error {
	s.index.mu.Lock()
	defer s.index.mu.Unlock()
	if _, err := s.index.db.ExecContext(ctx, `BEGIN IMMEDIATE; `+clearSearchProjectionSQL+` COMMIT;`); err != nil {
		_, _ = s.index.db.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	_, err := s.index.db.ExecContext(ctx, "VACUUM")
	return err
}

func (s *Store) SearchIndexSettings(ctx context.Context) (SearchIndexSettings, error) {
	var settings SearchIndexSettings
	var content string
	err := s.index.db.QueryRowContext(ctx, `SELECT budget_bytes,automatic,desired FROM settings
		JOIN content_scope ON content_scope.id=settings.id WHERE settings.id=1`).Scan(&settings.BudgetBytes, &settings.Automatic, &content)
	if err != nil {
		return settings, err
	}
	policy, err := decodeIndexContentPolicy(content)
	if policy.Content != nil {
		settings.Content = &policy.Content
	}
	if policy.RecentContext {
		settings.RecentContext = &policy.RecentContext
	}
	return settings, err
}

func (s *Store) SetSearchIndexSettings(ctx context.Context, settings SearchIndexSettings) error {
	if settings.BudgetBytes < 1<<20 || settings.BudgetBytes > 64<<30 {
		return fmt.Errorf("索引预算须在 1 MiB 至 64 GiB 之间")
	}
	var content []string
	if settings.Content != nil {
		var err error
		content, err = searchtext.ValidateContent(*settings.Content)
		if err != nil {
			return err
		}
	}
	s.index.mu.Lock()
	defer s.index.mu.Unlock()
	tx, err := s.index.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE settings SET budget_bytes=?,automatic=? WHERE id=1", settings.BudgetBytes, settings.Automatic); err != nil {
		return err
	}
	if content != nil || settings.RecentContext != nil {
		var desired string
		if err := tx.QueryRowContext(ctx, "SELECT desired FROM content_scope WHERE id=1").Scan(&desired); err != nil {
			return err
		}
		policy, err := decodeIndexContentPolicy(desired)
		if err != nil {
			return err
		}
		if content != nil {
			policy.Content = content
		}
		if settings.RecentContext != nil {
			policy.RecentContext = *settings.RecentContext
		}
		encoded, err := policy.encode()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE content_scope SET desired=? WHERE id=1", encoded); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SearchIndexStatus(ctx context.Context, root string) (SearchIndexStatus, error) {
	files, err := s.IndexFilesFromCatalog(ctx, root)
	if err != nil {
		return SearchIndexStatus{}, err
	}
	return s.SearchIndexStatusWithFiles(ctx, root, files)
}

// SearchIndexStatusWithFiles 使用当前发现的来源计算覆盖率，目录尚未持久化时也能显示真实分母。
func (s *Store) SearchIndexStatusWithFiles(ctx context.Context, root string, files []domain.FileInventory) (SearchIndexStatus, error) {
	result := SearchIndexStatus{Path: s.index.path}
	var err error
	result.Settings, err = s.SearchIndexSettings(ctx)
	if err != nil {
		return result, err
	}
	var pages, pageSize, free int64
	for _, q := range []struct {
		sql   string
		value *int64
	}{{"PRAGMA page_count", &pages}, {"PRAGMA page_size", &pageSize}, {"PRAGMA freelist_count", &free}} {
		if err := s.index.db.QueryRowContext(ctx, q.sql).Scan(q.value); err != nil {
			return result, err
		}
	}
	result.DatabaseBytes, result.UsedBytes, result.Reclaimable = pages*pageSize, (pages-free)*pageSize, free*pageSize
	result.OverBudget = result.DatabaseBytes > result.Settings.BudgetBytes
	current := make(map[string]domain.FileInventory, len(files))
	for _, file := range files {
		if file.Kind != domain.AssetKindJSONL {
			continue
		}
		current[file.RelativePath] = file
		result.CatalogFiles++
		result.SourceBytes += file.SizeBytes
	}
	rows, err := s.index.db.QueryContext(ctx, `SELECT path,size,mtime,prefix_hash,committed,complete FROM files
		WHERE root=? AND committed>0 AND (SELECT user_version FROM pragma_user_version)=?
		AND EXISTS(SELECT 1 FROM content_scope WHERE desired=built)`, root, searchProjectionVersion)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var path, hash string
		var size, mtime, committed int64
		var complete bool
		if err := rows.Scan(&path, &size, &mtime, &hash, &committed, &complete); err != nil {
			_ = rows.Close()
			return result, err
		}
		file, ok := current[path]
		if !ok {
			continue
		}
		if complete && indexFileCurrent(file, size, mtime, hash) {
			result.IndexedFiles++
			result.CoveredBytes += size
		} else if committed < file.SizeBytes || (!complete && committed == file.SizeBytes) {
			// 状态查询不重新读大文件。真正能否复用，在搜索时校验字节指纹。
			result.PartialFiles++
			result.PartialBytes += committed
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := s.index.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM documents JOIN files ON files.id=documents.file_id WHERE files.root=? AND v.user_version=? AND c.desired=c.built),
		(v.user_version<>? OR c.desired<>c.built) AND EXISTS(SELECT 1 FROM files WHERE committed>0)
		FROM pragma_user_version v,content_scope c WHERE c.id=1`, root, searchProjectionVersion, searchProjectionVersion).Scan(&result.Documents, &result.RebuildRequired); err != nil {
		return result, err
	}
	var hasLegacy bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM search_locations) OR EXISTS(SELECT 1 FROM search_documents)`).Scan(&hasLegacy); err != nil {
		return result, err
	}
	// 旧库的重复正文只有在用户清理搜索数据时移除；升级不会自动丢弃已有恢复快照。
	if hasLegacy {
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sum(pgsize),0) FROM dbstat WHERE name='search_locations' OR name LIKE 'search_documents%' OR name LIKE '%search_locations%'`).Scan(&result.LegacyBytes); err != nil {
			return result, err
		}
	}
	return result, nil
}

// BuildSearchIndex 按 128 行短事务提交；达到空间上限时回滚当前批次，保留之前已完成的游标。
func (s *Store) BuildSearchIndex(ctx context.Context, report domain.InventoryReport, progress func(SearchIndexProgress)) (SearchIndexProgress, error) {
	if !report.ReadOnly {
		return SearchIndexProgress{}, fmt.Errorf("搜索构建只接受只读扫描结果")
	}
	s.index.mu.Lock()
	defer s.index.mu.Unlock()
	settings, err := s.SearchIndexSettings(ctx)
	if err != nil {
		return SearchIndexProgress{}, err
	}
	result := SearchIndexProgress{}
	if err := s.index.prepareProjection(ctx); err != nil {
		if indexFull(err) {
			result.BudgetReached = true
			return result, nil
		}
		return result, err
	}
	var scope searchtext.ContentScope
	if settings.Content != nil {
		scope = searchtext.ContentScopeFor(*settings.Content)
	}
	recentContext := settings.RecentContext != nil && *settings.RecentContext &&
		(scope == nil || scope["user"] || scope["final"] || scope["progress"] || scope["unmarked"])
	var pages, pageSize int64
	if err := s.index.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return SearchIndexProgress{}, err
	}
	if err := s.index.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return SearchIndexProgress{}, err
	}
	files := make([]domain.FileInventory, 0)
	for _, file := range report.Files {
		if file.Kind == domain.AssetKindJSONL && file.Rollout != nil {
			files = append(files, file)
		}
	}
	// 先处理较新的会话，同一批次和重启后使用确定的顺序。
	sort.Slice(files, func(a, b int) bool {
		if files[a].ModTime.Equal(files[b].ModTime) {
			return files[a].RelativePath < files[b].RelativePath
		}
		return files[a].ModTime.After(files[b].ModTime)
	})
	// 在最近的候选中优先完成稳定的小文件，避免单个大文件先用完预算。
	recent := files[:min(len(files), 64)]
	smallLimit := settings.BudgetBytes / 8
	isStableSmall := func(file domain.FileInventory) bool {
		return file.Status == domain.StatusStable && file.SizeBytes <= smallLimit
	}
	sort.SliceStable(recent, func(a, b int) bool {
		return isStableSmall(recent[a]) && !isStableSmall(recent[b])
	})
	result.FilesTotal = len(files)
	if pages*pageSize > settings.BudgetBytes {
		result.BudgetReached = true
		return result, nil
	}
	if _, err := s.index.db.ExecContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", settings.BudgetBytes/pageSize)); err != nil {
		return result, err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		added, covered, err := s.index.buildFile(ctx, report.Root, file, scope, recentContext, func(added, covered int64) {
			if progress != nil {
				pending := result
				pending.DocumentsAdded += added
				pending.BytesCovered += covered
				progress(pending)
			}
		})
		result.DocumentsAdded += added
		result.BytesCovered += covered
		if indexFull(err) {
			result.BudgetReached = true
			if progress != nil {
				progress(result)
			}
			return result, nil
		}
		if err != nil {
			return result, err
		}
		result.FilesDone++
		if progress != nil {
			progress(result)
		}
	}
	return result, nil
}

func indexFull(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&255 == 13
}

func (index *searchIndex) buildFile(ctx context.Context, root string, file domain.FileInventory, scope searchtext.ContentScope, recentContext bool, progress func(int64, int64)) (int64, int64, error) {
	path, err := application.ResolveReadOnlyPath(root, file.RelativePath)
	if err != nil {
		return 0, 0, err
	}
	input, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil {
		return 0, 0, err
	}
	var id, committed, lines, size, mtime, previousStart int64
	var prefix string
	var complete bool
	err = index.db.QueryRowContext(ctx, "SELECT id,committed,lines,prefix_hash,size,mtime,complete,window_start FROM files WHERE root=? AND path=?", root, file.RelativePath).Scan(&id, &committed, &lines, &prefix, &size, &mtime, &complete, &previousStart)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, err
	}
	// 只读扫描已经提供完整指纹时，未变文件不再重复读取全部正文。
	if complete && committed == before.Size() && size == before.Size() && mtime == before.ModTime().UnixNano() &&
		file.JSONL != nil && file.JSONL.ContentSHA256 != "" && file.JSONL.ContentSHA256 == prefix &&
		file.SizeBytes == before.Size() && file.ModTime.Equal(before.ModTime()) {
		return 0, 0, nil
	}
	var windowStart int64
	if recentContext {
		windowStart, err = searchtext.RecentContextStart(searchContextReader{ctx, io.NewSectionReader(input, 0, before.Size())})
		if err != nil {
			return 0, 0, err
		}
	}
	digest := sha256.New()
	projector := searchtext.NewContentProjector(scope, recentContext)
	if committed > before.Size() || windowStart < previousStart {
		// 模型窗口变大时，须从旧边界之前补回正文，按原文顺序重建这个文件。
		committed = 0
	}
	if committed > 0 {
		source := io.LimitReader(searchContextReader{ctx, input}, committed)
		if scope == nil {
			_, err = io.Copy(digest, source)
		} else {
			err = projector.ReadCommitted(io.TeeReader(source, digest))
		}
		if err != nil {
			return 0, 0, err
		}
		if hex.EncodeToString(digest.Sum(nil)) != prefix {
			committed = 0
		}
	}
	if committed == 0 {
		lines = 0
		digest.Reset()
		projector = searchtext.NewContentProjector(scope, recentContext)
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return 0, 0, err
		}
	}
	if complete && committed == before.Size() && size == before.Size() && mtime == before.ModTime().UnixNano() {
		return 0, 0, nil
	}
	var tx *sql.Tx
	begin := func() error { var err error; tx, err = index.db.BeginTx(ctx, nil); return err }
	if err := begin(); err != nil {
		return 0, 0, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if id == 0 {
		r, err := tx.ExecContext(ctx, "INSERT INTO files(root,path,conversation_id) VALUES(?,?,?)", root, file.RelativePath, file.Rollout.ConversationID)
		if err != nil {
			return 0, 0, err
		}
		id, err = r.LastInsertId()
		if err != nil {
			return 0, 0, err
		}
	} else if committed == 0 {
		if err := deleteIndexFileDocuments(ctx, tx, id); err != nil {
			return 0, 0, err
		}
	} else if windowStart > previousStart {
		if err := deleteIndexDocuments(ctx, tx, "file_id=? AND start_offset<? AND window_limited=1", id, windowStart); err != nil {
			return 0, 0, err
		}
	}
	initial := committed
	saved := committed
	var added, pendingAdded int64
	checkpoint := func(done bool) error {
		_, err := tx.ExecContext(ctx, "UPDATE files SET committed=?,lines=?,prefix_hash=?,size=?,mtime=?,complete=?,conversation_id=?,window_start=? WHERE id=?", committed, lines, hex.EncodeToString(digest.Sum(nil)), before.Size(), before.ModTime().UnixNano(), done, file.Rollout.ConversationID, windowStart, id)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		tx = nil
		added += pendingAdded
		pendingAdded = 0
		saved = committed
		if progress != nil {
			progress(added, saved-initial)
		}
		return nil
	}
	reader := bufio.NewReader(io.LimitReader(searchContextReader{ctx, input}, before.Size()-committed))
	batch := 0
	var compressed []byte
	for {
		if err := ctx.Err(); err != nil {
			return added, saved - initial, err
		}
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return added, saved - initial, readErr
		}
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			break
		}
		var frame domain.JSONLFrame
		var contentCopy []byte
		_, err := catalog.ScanReaderContent(bytes.NewReader(raw), func(f domain.JSONLFrame, content []byte) error { frame = f; contentCopy = content; return nil })
		if err != nil {
			return added, saved - initial, err
		}
		inWindow := committed >= windowStart
		var normalized string
		var windowLimited bool
		if frame.ValidJSON && !frame.Pending {
			normalized, windowLimited = projector.IndexRecord(frame, contentCopy, inWindow)
		}
		if frame.ValidJSON && !frame.Pending && (inWindow || !windowLimited) && (scope == nil || normalized != "") {
			// 保留易压缩的长记录，避免短关键词搜索反复解码 JSON。
			// 缓存与解码都有上限，未缓存的正文仍可按位置回读。
			var cached []byte
			if len(normalized) <= maxDecodedSearchText {
				compressed = index.encoder.EncodeAll([]byte(normalized), compressed[:0])
				if len(compressed) <= maxCompressedSearchText {
					cached = compressed
				}
			}
			r, err := tx.ExecContext(ctx, "INSERT INTO documents(file_id,line,start_offset,end_offset,top_type,text,window_limited) VALUES(?,?,?,?,?,COALESCE(?,X''),?)", id, lines+1, committed, committed+int64(len(raw)), frame.TopLevelType, cached, windowLimited)
			if err != nil {
				return added, saved - initial, err
			}
			docID, err := r.LastInsertId()
			if err != nil {
				return added, saved - initial, err
			}
			// 含 NUL 的关键词直接走逐条复核。替换分隔符后，FTS 仍可索引其后的词。
			if _, err := tx.ExecContext(ctx, "INSERT INTO terms(rowid,content) VALUES(?,?)", docID, strings.ReplaceAll(normalized, "\x00", " ")); err != nil {
				return added, saved - initial, err
			}
			if pairs := hanSearchTerms(normalized); pairs != "" {
				if _, err := tx.ExecContext(ctx, "INSERT INTO han_terms(rowid,content) VALUES(?,?)", docID, pairs); err != nil {
					return added, saved - initial, err
				}
			}
			pendingAdded++
		}
		_, _ = digest.Write(raw)
		committed += int64(len(raw))
		lines++
		batch++
		if batch == 128 {
			if err := checkpoint(false); err != nil {
				return added, saved - initial, err
			}
			if err := begin(); err != nil {
				return added, saved - initial, err
			}
			batch = 0
		}
		if readErr == io.EOF {
			break
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		return added, saved - initial, err
	}
	if !os.SameFile(before, after) || after.Size() < before.Size() || (after.Size() == before.Size() && !after.ModTime().Equal(before.ModTime())) {
		return added, saved - initial, fmt.Errorf("源文件在索引期间被替换或重写，请重新扫描：%s", file.RelativePath)
	}
	done := committed == before.Size() && after.Size() == before.Size()
	if done && file.JSONL != nil && file.SizeBytes == before.Size() && file.JSONL.ContentSHA256 != "" && hex.EncodeToString(digest.Sum(nil)) != file.JSONL.ContentSHA256 {
		return added, saved - initial, fmt.Errorf("源文件指纹变化，请重新扫描：%s", file.RelativePath)
	}
	if err := checkpoint(done); err != nil {
		return added, saved - initial, err
	}
	return added, saved - initial, nil
}

type searchContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r searchContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func deleteIndexFileDocuments(ctx context.Context, tx *sql.Tx, id int64) error {
	return deleteIndexDocuments(ctx, tx, "file_id=?", id)
}

func deleteIndexDocuments(ctx context.Context, tx *sql.Tx, condition string, args ...any) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM terms WHERE rowid IN (SELECT id FROM documents WHERE "+condition+")", args...); err != nil {
		return err
	}
	var hasHanTerms bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='han_terms')").Scan(&hasHanTerms); err != nil {
		return err
	}
	if hasHanTerms {
		if _, err := tx.ExecContext(ctx, "DELETE FROM han_terms WHERE rowid IN (SELECT id FROM documents WHERE "+condition+")", args...); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM documents WHERE "+condition, args...)
	return err
}

// ClearSearchIndex 仅清理可重建搜索数据并暂停自动构建；保留会话目录、阅读定位和设置。
func (s *Store) ClearSearchIndex(ctx context.Context) error {
	s.index.mu.Lock()
	defer s.index.mu.Unlock()
	_, err := s.index.db.ExecContext(ctx, `BEGIN IMMEDIATE; UPDATE settings SET automatic=0; `+clearSearchProjectionSQL+` COMMIT;`)
	if err != nil {
		_, _ = s.index.db.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if _, err := s.index.db.ExecContext(ctx, "VACUUM"); err != nil {
		return err
	}
	// 清理旧版在目录库中保存的两份正文，再通过 SQLite 收缩空闲页；不删除数据库文件。
	if _, err := s.db.ExecContext(ctx, `BEGIN IMMEDIATE; DELETE FROM search_documents; DELETE FROM search_locations; COMMIT;`); err != nil {
		_, _ = s.db.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// CompactSearchIndex 回收已删除记录的空闲页，保留现有搜索内容。
func (s *Store) CompactSearchIndex(ctx context.Context) error {
	s.index.mu.Lock()
	defer s.index.mu.Unlock()
	_, err := s.index.db.ExecContext(ctx, "VACUUM")
	return err
}

func indexedSourceText(ctx context.Context, path string, expected os.FileInfo, hit domain.SearchHit, scope searchtext.ContentScope) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if hit.StartOffset < 0 || hit.EndOffset <= hit.StartOffset || hit.EndOffset > expected.Size() {
		return "", fmt.Errorf("搜索索引的原文位置无效，请重新构建")
	}
	source, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || !sameSearchSource(expected, before) {
		return "", fmt.Errorf("搜索时源文件发生变化，请重试")
	}
	raw := make([]byte, hit.EndOffset-hit.StartOffset)
	if _, err := source.ReadAt(raw, hit.StartOffset); err != nil {
		return "", err
	}
	if !searchSourceUnchanged(path, before) {
		return "", fmt.Errorf("搜索时源文件发生变化，请重试")
	}
	frame := domain.JSONLFrame{ValidJSON: true, LineNumber: hit.Line, TopLevelType: hit.TopLevelType, StartOffset: hit.StartOffset, EndOffset: hit.EndOffset}
	return scope.CheckRecord(frame, bytes.TrimRight(raw, "\r\n")), nil
}

// searchTrigrams 选择至多八个必要的三字片段求交集，实际命中仍由正文复核。
func searchTrigrams(query string) string {
	runes := []rune(query)
	if len(runes) < 3 || strings.ContainsRune(query, 0) {
		return ""
	}
	count := min(8, len(runes)-2)
	terms := make([]string, 0, count)
	for i := 0; i < count; i++ {
		position := 0
		if count > 1 {
			position = i * (len(runes) - 3) / (count - 1)
		}
		term := string(runes[position : position+3])
		terms = append(terms, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " AND ")
}

// hanSearchTerms 每条记录只保留不同的相邻汉字对。匹配后仍回到正文检查原词。
func hanSearchTerms(text string) string {
	var tokens strings.Builder
	var previous rune
	seen := make(map[uint64]struct{})
	for _, character := range text {
		if !unicode.Is(unicode.Han, character) {
			previous = 0
			continue
		}
		if previous != 0 {
			key := hanPairKey(previous, character)
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				tokens.WriteString(strconv.FormatUint(key, 16))
				tokens.WriteByte(' ')
			}
		}
		previous = character
	}
	return tokens.String()
}

func hanPairKey(first, second rune) uint64 {
	return uint64(first)<<21 | uint64(second)
}

func indexFileCurrent(file domain.FileInventory, size, mtime int64, hash string) bool {
	return file.SizeBytes == size && file.ModTime.UnixNano() == mtime &&
		(file.JSONL == nil || file.JSONL.ContentSHA256 == "" || file.JSONL.ContentSHA256 == hash)
}

// SearchWithFiles 使用当前只读快照限定范围；未覆盖或变化的文件回退源扫描。
func (s *Store) SearchWithFiles(ctx context.Context, root, query string, limit int, files []domain.FileInventory) ([]domain.SearchHit, error) {
	return s.SearchWithFilesFiltered(ctx, root, query, limit, files, nil)
}

// SearchWithFilesFiltered 在返回上限之前复核候选，避免失效或重复记录用尽名额。
func (s *Store) SearchWithFilesFiltered(ctx context.Context, root, query string, limit int, files []domain.FileInventory, filter application.SearchHitFilter) ([]domain.SearchHit, error) {
	return s.SearchWithFilesObserved(ctx, root, query, limit, files, filter, nil)
}

// RemoveSearchFiles 只移除指定派生文件的搜索记录，供删除对话和类型纠正后失效使用。
func (s *Store) RemoveSearchFiles(ctx context.Context, root string, paths []string) error {
	s.index.mu.Lock()
	defer s.index.mu.Unlock()
	tx, err := s.index.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, path := range paths {
		var id int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM files WHERE root=? AND path=?", root, path).Scan(&id)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		if err := deleteIndexFileDocuments(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM files WHERE id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// IndexFilesFromCatalog 仅读取本项目目录库，不遍历源目录。
func (s *Store) IndexFilesFromCatalog(ctx context.Context, root string) ([]domain.FileInventory, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT relative_path,size_bytes,mod_time_ns,status,content_sha256 FROM assets WHERE workspace_id=? AND kind=? ORDER BY relative_path", stableID("workspace", root), domain.AssetKindJSONL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []domain.FileInventory
	for rows.Next() {
		var file domain.FileInventory
		var mtime int64
		file.JSONL = &domain.JSONLScanSummary{}
		if err := rows.Scan(&file.RelativePath, &file.SizeBytes, &mtime, &file.Status, &file.JSONL.ContentSHA256); err != nil {
			return nil, err
		}
		file.Kind = domain.AssetKindJSONL
		file.ModTime = time.Unix(0, mtime)
		files = append(files, file)
	}
	return files, rows.Err()
}

// indexSourcePaths 用于保持索引与扫描结果同步，识别删除和类型变更后的残留。
func (s *Store) indexSourcePaths(ctx context.Context, root string) ([]string, error) {
	rows, err := s.index.db.QueryContext(ctx, "SELECT path FROM files WHERE root=?", root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, filepath.ToSlash(p))
	}
	return paths, rows.Err()
}
