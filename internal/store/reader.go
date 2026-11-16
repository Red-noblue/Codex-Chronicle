// 中文说明：复用派生 frame 索引定位阅读窗口；不在数据库中新增正文副本。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/searchtext"
)

// LoadReaderProjection 只读取本项目的有界导航元数据，不含消息正文和工具输出。
func (s *Store) LoadReaderProjection(ctx context.Context, scope, stamp string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM reader_projections WHERE scope=? AND stamp=? AND length(payload)<=?", scope, stamp, 16<<20).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}
func (s *Store) SaveReaderProjection(ctx context.Context, scope, stamp string, b []byte) error {
	if len(b) > 16<<20 {
		return fmt.Errorf("导航缓存超过上限")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO reader_projections(scope,stamp,payload,updated_at) VALUES(?,?,?,?) ON CONFLICT(scope) DO UPDATE SET stamp=excluded.stamp,payload=excluded.payload,updated_at=excluded.updated_at`, scope, stamp, b, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	// 每个来源只保留最新快照，总缓存最多 32 项；删除后的派生清理另行失效。
	if _, err = tx.ExecContext(ctx, "DELETE FROM reader_projections WHERE scope NOT IN (SELECT scope FROM reader_projections ORDER BY updated_at DESC LIMIT 32)"); err != nil {
		return err
	}
	for {
		var size int64
		if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(length(payload)),0) FROM reader_projections").Scan(&size); err != nil {
			return err
		}
		if size <= 64<<20 {
			break
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM reader_projections WHERE scope=(SELECT scope FROM reader_projections ORDER BY updated_at LIMIT 1)"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReaderFrame 返回目标附近完整记录的起点和此前行数；previous 用于向前翻页。
func (s *Store) ReaderFrame(ctx context.Context, root, relative string, offset int64, previous int) (int64, uint64, error) {
	var start int64
	var line uint64
	err := s.db.QueryRowContext(ctx, `SELECT f.start_offset,f.line FROM jsonl_frames f
		JOIN assets a ON a.id=f.asset_id WHERE a.workspace_id=? AND a.relative_path=? AND f.start_offset<=? AND f.pending=0
		ORDER BY f.start_offset DESC LIMIT 1 OFFSET ?`, stableID("workspace", root), relative, offset, previous).Scan(&start, &line)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("读取 frame 定位失败: %w", err)
	}
	return start, line - 1, nil
}

// ReaderSearchCandidates 只查当前历史文件的派生索引，不回退扫描整个源目录。
func (s *Store) ReaderSearchCandidates(ctx context.Context, root string, paths []string, query string) ([]domain.SearchHit, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := []any{stableID("workspace", root)}
	marks := make([]string, len(paths))
	for i, path := range paths {
		marks[i] = "?"
		args = append(args, path)
	}
	selectSQL := `SELECT loc.conversation_id,loc.source_path,loc.line,loc.start_offset,loc.end_offset,loc.snippet,loc.top_type,asset.status
		FROM search_documents JOIN search_locations loc ON loc.doc_id=search_documents.asset_id
		JOIN assets asset ON asset.id=loc.asset_id WHERE asset.workspace_id=? AND loc.source_path IN (` + strings.Join(marks, ",") + `)`
	match := `"` + strings.ReplaceAll(searchtext.Normalize(query), `"`, `""`) + `"`
	rows, err := s.db.QueryContext(ctx, selectSQL+` AND search_documents MATCH ? ORDER BY loc.source_path,loc.line LIMIT 100`, append(args, match)...)
	if err != nil {
		return nil, err
	}
	hits, err := scanSearchRows(rows, 100)
	if err != nil || len(hits) > 0 {
		return hits, err
	}
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(query))
	rows, err = s.db.QueryContext(ctx, selectSQL+` AND loc.snippet LIKE '%' || ? || '%' ESCAPE '\' ORDER BY loc.source_path,loc.line LIMIT 100`, append(args, like)...)
	if err != nil {
		return nil, err
	}
	return scanSearchRows(rows, 100)
}
