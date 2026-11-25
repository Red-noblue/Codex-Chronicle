// 中文说明：复用派生 frame 索引定位阅读窗口；不在数据库中新增正文副本。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"codex-chronicle.local/internal/domain"
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
	files, err := s.IndexFilesFromCatalog(ctx, root)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(paths))
	for _, path := range paths {
		allowed[path] = true
	}
	scoped := make([]domain.FileInventory, 0, len(paths))
	for _, file := range files {
		if allowed[file.RelativePath] {
			scoped = append(scoped, file)
		}
	}
	return s.SearchWithFiles(ctx, root, query, 100, scoped)
}
