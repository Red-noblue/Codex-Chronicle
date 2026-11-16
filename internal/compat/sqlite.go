// 中文说明：以只读连接提取 SQLite 的版本、表列和 schema 指纹，不执行迁移或 checkpoint。
package compat

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/sqliteguard"
	_ "modernc.org/sqlite"
)

// AnalyzeSQLitePath 通过 SQLite immutable 只读视图打开数据库，仅读取结构元数据。
func AnalyzeSQLitePath(ctx context.Context, path string) (domain.SQLiteSummary, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return domain.SQLiteSummary{}, fmt.Errorf("解析 SQLite 路径失败: %w", err)
	}
	dsn, auxiliary, err := sqliteguard.ReadOnlyDSN(abs, 1000)
	if err != nil {
		return domain.SQLiteSummary{}, err
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return domain.SQLiteSummary{}, fmt.Errorf("打开 SQLite 只读连接失败: %w", err)
	}
	defer database.Close()
	// 结构查询会先读取表清单，再逐表读取列定义；允许少量只读连接避免
	// 某些 SQLite 驱动在游标关闭时把下一条 PRAGMA 查询排队等待自身。
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(2)
	if err := database.PingContext(ctx); err != nil {
		return domain.SQLiteSummary{}, fmt.Errorf("读取 SQLite 失败: %w", err)
	}
	var userVersion int
	if err := database.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return domain.SQLiteSummary{}, fmt.Errorf("读取 SQLite user_version 失败: %w", err)
	}
	rows, err := database.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return domain.SQLiteSummary{}, fmt.Errorf("读取 SQLite 表清单失败: %w", err)
	}
	tableNames := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return domain.SQLiteSummary{}, fmt.Errorf("读取 SQLite 表名失败: %w", err)
		}
		tableNames = append(tableNames, name)
	}
	if err := rows.Close(); err != nil {
		return domain.SQLiteSummary{}, fmt.Errorf("关闭 SQLite 表清单失败: %w", err)
	}
	if err := rows.Err(); err != nil {
		return domain.SQLiteSummary{}, fmt.Errorf("遍历 SQLite 表清单失败: %w", err)
	}
	tables := make([]domain.SQLiteTableSummary, 0, len(tableNames))
	for _, name := range tableNames {
		columns, err := tableColumns(ctx, database, name)
		if err != nil {
			return domain.SQLiteSummary{}, err
		}
		tables = append(tables, domain.SQLiteTableSummary{Name: name, Columns: columns})
	}
	objects, err := schemaObjects(ctx, database)
	if err != nil {
		return domain.SQLiteSummary{}, err
	}
	summary := domain.SQLiteSummary{
		UserVersion: userVersion, Tables: tables, SchemaObjects: objects,
		SchemaFingerprint: schemaFingerprint(userVersion, tables, objects), ReadOnly: true,
	}
	if len(auxiliary) > 0 {
		summary.Diagnostic = &domain.Diagnostic{
			Code:     "SQLITE_IMMUTABLE_WAL_VIEW",
			Severity: domain.SeverityWarning,
			Message:  "检测到 SQLite WAL/SHM，已使用 immutable 只读视图；结构摘要可能未包含 WAL 中尚未合并的最新提交",
			Evidence: strings.Join(auxiliary, ", "),
			ReadOnly: true,
		}
	}
	return summary, nil
}

func schemaObjects(ctx context.Context, database *sql.DB) ([]domain.SQLiteSchemaObjectSummary, error) {
	rows, err := database.QueryContext(ctx, `SELECT type, name, tbl_name, sql
		FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' AND type IN ('index', 'trigger', 'view')
		ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("读取 SQLite schema 对象失败: %w", err)
	}
	defer rows.Close()
	objects := make([]domain.SQLiteSchemaObjectSummary, 0)
	for rows.Next() {
		var kind, name, table string
		var definition sql.NullString
		if err := rows.Scan(&kind, &name, &table, &definition); err != nil {
			return nil, fmt.Errorf("读取 SQLite schema 对象失败: %w", err)
		}
		object := domain.SQLiteSchemaObjectSummary{Kind: kind, Name: name, Table: table}
		if definition.Valid {
			hash := sha256.Sum256([]byte(definition.String))
			object.SQLSHA256 = hex.EncodeToString(hash[:])
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 SQLite schema 对象失败: %w", err)
	}
	return objects, nil
}

func tableColumns(ctx context.Context, database *sql.DB, table string) ([]string, error) {
	quoted := strings.ReplaceAll(table, `"`, `""`)
	rows, err := database.QueryContext(ctx, `PRAGMA table_info("`+quoted+`")`)
	if err != nil {
		return nil, fmt.Errorf("读取 SQLite 表 %s 列失败: %w", table, err)
	}
	defer rows.Close()
	columns := make([]string, 0)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("读取 SQLite 表 %s 列定义失败: %w", table, err)
		}
		columns = append(columns, fmt.Sprintf("%s:%s:%d:%d", name, columnType, notNull, primaryKey))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 SQLite 表 %s 列失败: %w", table, err)
	}
	return columns, nil
}

func schemaFingerprint(userVersion int, tables []domain.SQLiteTableSummary, objects []domain.SQLiteSchemaObjectSummary) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "user_version=%d\n", userVersion)
	for _, table := range tables {
		_, _ = fmt.Fprintf(hash, "table=%s\n", table.Name)
		for _, column := range table.Columns {
			_, _ = fmt.Fprintf(hash, "column=%s\n", column)
		}
	}
	for _, object := range objects {
		_, _ = fmt.Fprintf(hash, "object=%s:%s:%s:%s\n", object.Kind, object.Name, object.Table, object.SQLSHA256)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
