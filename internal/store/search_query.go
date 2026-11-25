// 中文说明：按来源顺序分批读取检索候选，释放数据库连接后再解压或复核原文。
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"unicode"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/searchtext"
)

const searchCandidateBatch = 64
const sparseSearchThreshold = 128

var (
	ErrSearchIndexNotReady = errors.New("当前工作区没有可用索引，可改用全部搜索")
	ErrSearchIndexChanged  = errors.New("搜索期间索引发生变化，请重试")
)

type indexedSearchFile struct {
	id, size, mtime int64
	committed       int64
	windowStart     int64
	lines           uint64
	hash            string
	conversationID  string
	complete        bool
	policy          string
}

const searchFileColumns = "id,size,mtime,committed,window_start,lines,prefix_hash,conversation_id,complete,(SELECT built FROM content_scope WHERE id=1)"

func (file *indexedSearchFile) scanTargets() []any {
	return []any{&file.id, &file.size, &file.mtime, &file.committed, &file.windowStart, &file.lines, &file.hash, &file.conversationID, &file.complete, &file.policy}
}

type searchCandidate struct {
	id int64
	domain.SearchHit
	compressed []byte
}

type searchTermMatch struct {
	table string // 只取代码中定义的 SQLite 表名，关键词另行绑定。
	value string
}

func searchTermsFor(query string) searchTermMatch {
	if match := searchTrigrams(query); match != "" {
		return searchTermMatch{table: "terms", value: match}
	}
	runes := []rune(query)
	if len(runes) == 2 && unicode.Is(unicode.Han, runes[0]) && unicode.Is(unicode.Han, runes[1]) {
		return searchTermMatch{table: "han_terms", value: strconv.FormatUint(hanPairKey(runes[0], runes[1]), 16)}
	}
	return searchTermMatch{}
}

// SearchWithFilesObserved 在每个来源完成后报告已复核的命中，未覆盖的部分仍搜索原文。
func (s *Store) SearchWithFilesObserved(ctx context.Context, root, query string, limit int, files []domain.FileInventory, filter application.SearchHitFilter, observe application.SearchObserver) ([]domain.SearchHit, error) {
	return s.searchWithFiles(ctx, root, query, limit, files, filter, observe, false)
}

// SearchIndexOnlyObserved 查找实际构建且仍可复核的内容。
func (s *Store) SearchIndexOnlyObserved(ctx context.Context, root, query string, limit int, files []domain.FileInventory, filter application.SearchHitFilter, observe application.SearchObserver) ([]domain.SearchHit, error) {
	return s.searchWithFiles(ctx, root, query, limit, files, filter, observe, true)
}

func (s *Store) searchWithFiles(ctx context.Context, root, query string, limit int, files []domain.FileInventory, filter application.SearchHitFilter, observe application.SearchObserver, indexedOnly bool) ([]domain.SearchHit, error) {
	normalized := searchtext.Normalize(query)
	if normalized == "" {
		return nil, fmt.Errorf("搜索关键词不能为空")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	match := searchTermsFor(normalized)
	coverage, sparse, scope, err := s.index.searchCoverage(ctx, root, match, indexedOnly)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]domain.FileInventory, len(files))
	for _, file := range files {
		if file.Kind == domain.AssetKindJSONL {
			allowed[file.RelativePath] = file
		}
	}
	paths := make([]string, 0, len(allowed))
	for path := range allowed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	hits := make([]domain.SearchHit, 0, limit)
	for i, relative := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file := allowed[relative]
		record, covered := coverage[relative]
		stage := domain.SearchMatchIndex
		var checkProgress func(domain.SearchMatchOrigin, int64) error
		if observe != nil {
			checkProgress = func(origin domain.SearchMatchOrigin, records int64) error {
				stage = origin
				return observe(application.SearchProgress{Stage: origin, FilesDone: i, FilesTotal: len(paths), RecordsDone: records})
			}
		}
		// coverage 只记录候选来自哪个已提交版本；实际读取前另外复核源文件。
		found, sourceBytes, prefixBytes, err := s.index.searchFile(ctx, root, file, record, covered && (sparse == nil || sparse[record.id]), normalized, match, limit-len(hits), filter, checkProgress, scope, indexedOnly)
		if err != nil {
			if errors.Is(err, ErrSearchIndexChanged) {
				// 仅保留已完成来源的命中，自动搜索会回读其余原文。
				return hits, err
			}
			return nil, err
		}
		hits = append(hits, found...)
		if observe != nil {
			if err := observe(application.SearchProgress{
				Stage: stage, FilesDone: i + 1, FilesTotal: len(paths), Hits: found,
				SourceBytesRead: sourceBytes, PrefixBytesRead: prefixBytes,
			}); err != nil {
				return nil, err
			}
		}
		if len(hits) == limit {
			break
		}
	}
	return hits, ctx.Err()
}

func (index *searchIndex) searchFile(ctx context.Context, root string, file domain.FileInventory, record indexedSearchFile, hasCandidates bool, query string, match searchTermMatch, limit int, filter application.SearchHitFilter, progress func(domain.SearchMatchOrigin, int64) error, scope searchtext.ContentScope, indexedOnly bool) ([]domain.SearchHit, int64, int64, error) {
	if indexedOnly && !hasCandidates {
		// 此次仅检索索引，没有候选的文件无需回读源文件。
		return nil, 0, 0, nil
	}
	var hits []domain.SearchHit
	var start application.SearchFileCursor
	var before os.FileInfo
	var sourceBytes, prefixBytes int64
	if record.id != 0 {
		path, err := application.ResolveReadOnlyPath(root, file.RelativePath)
		if err == nil {
			info, err := os.Lstat(path)
			if err == nil && info.Mode().IsRegular() {
				valid := record.complete && indexFileCurrent(file, record.size, record.mtime, record.hash) &&
					info.Size() == record.size && info.ModTime().UnixNano() == record.mtime
				if !valid && (!record.complete || record.committed < info.Size()) {
					valid, prefixBytes = verifyIndexedPrefix(ctx, path, info, record)
					sourceBytes += prefixBytes
				}
				if valid {
					before = info
					start = application.SearchFileCursor{Offset: record.committed, Line: record.lines, ConversationID: record.conversationID}
					if hasCandidates {
						var last searchCandidate
						var readBytes int64
						var exhausted bool
						hits, last, readBytes, exhausted, err = index.searchIndexedFile(ctx, root, path, file, record, info, query, match, limit, filter, progress, scope)
						if err != nil {
							return nil, 0, 0, err
						}
						sourceBytes += readBytes
						if !exhausted {
							if indexedOnly {
								return nil, 0, 0, ErrSearchIndexChanged
							}
							// 续建或清理改变了索引。只续用实际检查过的最后一行。
							start = application.SearchFileCursor{Offset: last.EndOffset, Line: last.Line, ConversationID: record.conversationID}
						}
					}
					if indexedOnly || start.Offset == info.Size() || len(hits) == limit {
						if !searchSourceUnchanged(path, info) {
							return nil, 0, 0, fmt.Errorf("搜索期间来源发生变化，请重试：%s", file.RelativePath)
						}
						return hits, sourceBytes, prefixBytes, nil
					}
				}
			}
		}
	}
	if indexedOnly {
		return nil, sourceBytes, prefixBytes, nil
	}
	var sourceProgress func(int64) error
	if progress != nil {
		if err := progress(domain.SearchMatchSource, 0); err != nil {
			return nil, 0, 0, err
		}
		sourceProgress = func(records int64) error { return progress(domain.SearchMatchSource, records) }
	}
	found, readBytes, err := application.SearchJSONLFileSuffix(ctx, root, file.RelativePath, query, limit-len(hits), before, start, filter, sourceProgress)
	if err != nil {
		return nil, 0, 0, err
	}
	return append(hits, found...), sourceBytes + readBytes, prefixBytes, nil
}

func verifyIndexedPrefix(ctx context.Context, path string, before os.FileInfo, record indexedSearchFile) (bool, int64) {
	if record.committed <= 0 || record.committed > before.Size() || record.lines == 0 || len(record.hash) != sha256.Size*2 {
		return false, 0
	}
	source, err := os.Open(path)
	if err != nil {
		return false, 0
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !sameSearchSource(before, opened) {
		return false, 0
	}
	var boundary [1]byte
	read, err := source.ReadAt(boundary[:], record.committed-1)
	if err != nil || boundary[0] != '\n' {
		return false, int64(read)
	}
	digest := sha256.New()
	n, err := io.CopyN(digest, searchContextReader{ctx, source}, record.committed)
	return err == nil && hex.EncodeToString(digest.Sum(nil)) == record.hash && searchSourceUnchanged(path, before), n + int64(read)
}

func searchSourceUnchanged(path string, before os.FileInfo) bool {
	after, err := os.Lstat(path)
	return err == nil && sameSearchSource(before, after)
}

func sameSearchSource(before, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func (index *searchIndex) searchCoverage(ctx context.Context, root string, match searchTermMatch, indexedOnly bool) (map[string]indexedSearchFile, map[int64]bool, searchtext.ContentScope, error) {
	tx, err := index.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback()
	var desired, built string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT desired,built,user_version FROM content_scope,pragma_user_version WHERE id=1`).Scan(&desired, &built, &version); err != nil {
		return nil, nil, nil, err
	}
	if !indexedOnly && (built != "" || desired != built || version != searchProjectionVersion) {
		// 自定义索引只覆盖所选类别，完整搜索须读原文。
		return nil, nil, nil, nil
	}
	if indexedOnly && (desired != built || version != searchProjectionVersion) {
		return nil, nil, nil, ErrSearchIndexNotReady
	}
	policy, err := decodeIndexContentPolicy(built)
	if err != nil {
		return nil, nil, nil, err
	}
	scope := searchtext.ContentScopeFor(policy.Content)
	rows, err := tx.QueryContext(ctx, `SELECT path,`+searchFileColumns+` FROM files
		WHERE root=? AND committed>0 AND (SELECT user_version FROM pragma_user_version)=?`, root, searchProjectionVersion)
	if err != nil {
		return nil, nil, nil, err
	}
	coverage := make(map[string]indexedSearchFile)
	for rows.Next() {
		var relative string
		var file indexedSearchFile
		if err := rows.Scan(append([]any{&relative}, file.scanTargets()...)...); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		coverage[relative] = file
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, err
	}
	if indexedOnly && len(coverage) == 0 {
		return nil, nil, nil, ErrSearchIndexNotReady
	}
	if match.value == "" || len(coverage) == 0 {
		return coverage, nil, scope, nil
	}
	// 稀有词只需访问少量来源。探测超过固定上限时停止，不先统计全库命中数。
	statement := fmt.Sprintf(`SELECT d.file_id FROM %[1]s
		CROSS JOIN documents d ON d.id=%[1]s.rowid CROSS JOIN files f ON f.id=d.file_id
		WHERE %[1]s MATCH ? AND f.root=? AND f.committed>0 LIMIT ?`, match.table)
	rows, err = tx.QueryContext(ctx, statement, match.value, root, sparseSearchThreshold+1)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	sparse := make(map[int64]bool)
	for count := 1; rows.Next(); count++ {
		if count > sparseSearchThreshold {
			return coverage, nil, scope, nil
		}
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, nil, nil, err
		}
		sparse[id] = true
	}
	return coverage, sparse, scope, rows.Err()
}

func (index *searchIndex) searchIndexedFile(ctx context.Context, root, path string, file domain.FileInventory, record indexedSearchFile, before os.FileInfo, query string, match searchTermMatch, limit int, filter application.SearchHitFilter, checkProgress func(domain.SearchMatchOrigin, int64) error, scope searchtext.ContentScope) ([]domain.SearchHit, searchCandidate, int64, bool, error) {
	var hits []domain.SearchHit
	var cursor searchCandidate
	var decoded []byte
	needle := []byte(query)
	var candidatesDone, sourceBytes int64
	for len(hits) < limit {
		batch, current, err := index.readSearchBatch(ctx, root, file.RelativePath, record, match, cursor)
		if err != nil {
			return nil, searchCandidate{}, 0, false, err
		}
		if !current {
			return hits, cursor, sourceBytes, false, nil
		}
		for _, candidate := range batch {
			if err := ctx.Err(); err != nil {
				return nil, searchCandidate{}, 0, false, err
			}
			cursor = candidate
			candidatesDone++
			hit := candidate.SearchHit
			hit.RelativePath, hit.ReadOnly, hit.SourceStatus = file.RelativePath, true, file.Status
			hit.MatchOrigin = domain.SearchMatchIndex
			if len(candidate.compressed) == 0 {
				text, err := indexedSourceText(ctx, path, before, hit, scope)
				if err != nil {
					return nil, searchCandidate{}, 0, false, err
				}
				sourceBytes += hit.EndOffset - hit.StartOffset
				hit.Snippet = searchtext.Snippet(text, query)
			} else {
				decoded, err = index.decoder.DecodeAll(candidate.compressed, decoded[:0])
				if err != nil {
					return nil, searchCandidate{}, 0, false, err
				}
				if bytes.Contains(decoded, needle) {
					hit.Snippet = searchtext.Snippet(string(decoded), query)
				}
				if cap(decoded) > 64<<10 {
					decoded = nil
				}
			}
			if hit.Snippet == "" {
				continue
			}
			if filter != nil {
				var accept bool
				hit, accept, err = filter(hit)
				if err != nil {
					return nil, searchCandidate{}, 0, false, err
				}
				if !accept {
					continue
				}
			}
			hits = append(hits, hit)
			if len(hits) == limit {
				break
			}
		}
		if checkProgress != nil && len(batch) > 0 {
			if err := checkProgress(domain.SearchMatchIndex, candidatesDone); err != nil {
				return nil, searchCandidate{}, 0, false, err
			}
		}
		if len(batch) < searchCandidateBatch {
			break
		}
	}
	return hits, cursor, sourceBytes, true, nil
}

const shortCandidateSQL = `SELECT id,line,start_offset,end_offset,top_type,text FROM documents
	WHERE file_id=? AND id>? ORDER BY id LIMIT ?`
const termCandidateSQL = `SELECT d.id,d.line,d.start_offset,d.end_offset,d.top_type,d.text
	FROM %[1]s CROSS JOIN documents d ON d.id=%[1]s.rowid
	WHERE %[1]s MATCH ? AND %[1]s.rowid>? AND %[1]s.rowid<=? AND d.file_id=?
	ORDER BY %[1]s.rowid LIMIT ?`

func (index *searchIndex) readSearchBatch(ctx context.Context, root, path string, record indexedSearchFile, match searchTermMatch, cursor searchCandidate) ([]searchCandidate, bool, error) {
	tx, err := index.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var current indexedSearchFile
	err = tx.QueryRowContext(ctx, `SELECT `+searchFileColumns+` FROM files
		WHERE root=? AND path=? AND (SELECT user_version FROM pragma_user_version)=?
		AND EXISTS(SELECT 1 FROM content_scope WHERE desired=built)`, root, path, searchProjectionVersion).
		Scan(current.scanTargets()...)
	if errors.Is(err, sql.ErrNoRows) || err == nil && current != record {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if cursor.id > 0 {
		// 重建可能复用文件 ID 与指纹，但给事件分配不同的 rowid。验证逻辑位置后才能续读。
		var line uint64
		err := tx.QueryRowContext(ctx, "SELECT line FROM documents WHERE id=? AND file_id=?", cursor.id, record.id).Scan(&line)
		if errors.Is(err, sql.ErrNoRows) || err == nil && line != cursor.Line {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
	var rows *sql.Rows
	if match.value == "" {
		rows, err = tx.QueryContext(ctx, shortCandidateSQL, record.id, cursor.id, searchCandidateBatch)
	} else {
		// 日志在单个来源内按行顺序分配 rowid，利用 FTS 自带的 rowid 范围和顺序。
		var first, last int64
		err = tx.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT id FROM documents WHERE file_id=? ORDER BY id LIMIT 1),0),
			COALESCE((SELECT id FROM documents WHERE file_id=? ORDER BY id DESC LIMIT 1),0)`, record.id, record.id).Scan(&first, &last)
		if err != nil || last == 0 {
			return nil, err == nil, err
		}
		rows, err = tx.QueryContext(ctx, fmt.Sprintf(termCandidateSQL, match.table), match.value, max(first-1, cursor.id), last, record.id, searchCandidateBatch)
	}
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	batch := make([]searchCandidate, 0, searchCandidateBatch)
	for rows.Next() {
		var row searchCandidate
		row.ConversationID = record.conversationID
		if err := rows.Scan(&row.id, &row.Line, &row.StartOffset, &row.EndOffset, &row.TopLevelType, &row.compressed); err != nil {
			return nil, false, err
		}
		batch = append(batch, row)
	}
	return batch, true, rows.Err()
}
