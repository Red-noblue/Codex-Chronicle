// 中文说明：从含 threads 表的 Codex SQLite 中提取线程元数据和关系证据，始终使用只读连接。
package compat

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/sqliteguard"
)

// AnalyzeThreadDatabasePath 读取 threads 和引用线程 ID 的表；没有 threads 表时返回空摘要。
func AnalyzeThreadDatabasePath(ctx context.Context, path string) ([]domain.ThreadRecord, []domain.SQLiteRelationship, error) {
	database, err := openReadOnlySQLite(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	defer database.Close()
	// 线程与行级关系必须取自同一个已提交视图，包含仍在 WAL 中的删除、增加和更名。
	snapshot, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("开始 SQLite 只读事务失败: %w", err)
	}
	defer snapshot.Rollback()
	rows, err := snapshot.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, nil, fmt.Errorf("读取 SQLite 表清单失败: %w", err)
	}
	tableNames := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("读取 SQLite 表名失败: %w", err)
		}
		tableNames = append(tableNames, name)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("关闭 SQLite 表清单失败: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("遍历 SQLite 表清单失败: %w", err)
	}
	hasThreads := false
	for _, name := range tableNames {
		if name == "threads" {
			hasThreads = true
			break
		}
	}
	if !hasThreads {
		relations, err := readUnboundRelationships(ctx, snapshot, tableNames)
		if err != nil {
			return nil, nil, err
		}
		return nil, relations, nil
	}
	threads, err := readThreads(ctx, snapshot)
	if err != nil {
		return nil, nil, err
	}
	relations, err := readRelationships(ctx, snapshot, tableNames, threads)
	if err != nil {
		return nil, nil, err
	}
	return threads, relations, nil
}

type sqliteQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readUnboundRelationships(ctx context.Context, database sqliteQueryer, tableNames []string) ([]domain.SQLiteRelationship, error) {
	relationships := make([]domain.SQLiteRelationship, 0)
	for _, table := range tableNames {
		columnInfo, err := pragmaColumnInfo(ctx, database, table)
		if err != nil {
			return nil, err
		}
		for _, columnDefinition := range columnInfo {
			column := columnDefinition.Name
			relation := relationshipName(table, column)
			if relation == "" {
				continue
			}
			query := `SELECT "` + quoteIdentifier(column) + `", COUNT(*) FROM "` + quoteIdentifier(table) + `" WHERE "` + quoteIdentifier(column) + `" IS NOT NULL GROUP BY "` + quoteIdentifier(column) + `" LIMIT 4096`
			rows, err := database.QueryContext(ctx, query)
			if err != nil {
				return nil, fmt.Errorf("读取未绑定关系 %s.%s 失败: %w", table, column, err)
			}
			for rows.Next() {
				var value any
				var count int64
				if err := rows.Scan(&value, &count); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("读取未绑定关系 %s.%s 行失败: %w", table, column, err)
				}
				threadID := databaseString(value)
				if threadID != "" && count > 0 {
					rowKeys, rowsComplete := readRelationshipRowKeys(ctx, database, table, column, threadID, count, columnInfo)
					relationships = appendUniqueSQLiteRelationship(relationships, domain.SQLiteRelationship{Table: table, Column: column, ThreadID: threadID, Relation: relation, RowCount: count, RowKeys: rowKeys, RowsComplete: rowsComplete})
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("遍历未绑定关系 %s.%s 失败: %w", table, column, err)
			}
			if err := rows.Close(); err != nil {
				return nil, fmt.Errorf("关闭未绑定关系 %s.%s 失败: %w", table, column, err)
			}
		}
	}
	if len(relationships) == 0 {
		return nil, nil
	}
	return relationships, nil
}

func openReadOnlySQLite(ctx context.Context, path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析 SQLite 路径失败: %w", err)
	}
	dsn, auxiliary, err := sqliteguard.ReadOnlyDSN(abs, 1000)
	if err != nil {
		return nil, err
	}
	if len(auxiliary) > 0 {
		// 已有旁路文件时不能用旧主库重建线程；缺少或异常的旁路文件应报错，避免由读取重建它们。
		for _, suffix := range []string{"-wal", "-shm"} {
			info, statErr := os.Lstat(abs + suffix)
			if statErr != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("SQLite 活动视图需要已存在的普通 WAL/SHM 文件: %s", filepath.Base(abs+suffix))
			}
		}
		// 非 WAL 主库旁的遗留文件不能交给 SQLite 打开，避免驱动把它们当成无效日志清理。
		source, openErr := os.Open(abs)
		if openErr != nil {
			return nil, fmt.Errorf("读取 SQLite 日志模式失败: %w", openErr)
		}
		var header [20]byte
		_, readErr := io.ReadFull(source, header[:])
		_ = source.Close()
		if readErr != nil || header[18] != 2 || header[19] != 2 {
			return nil, fmt.Errorf("SQLite 旁路文件与主库 WAL 模式不一致: %s", filepath.Base(abs))
		}
		dsn, _, err = sqliteguard.LiveReadOnlyDSN(abs, 1000)
		if err != nil {
			return nil, err
		}
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 只读连接失败: %w", err)
	}
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(2)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("读取 SQLite 失败: %w", err)
	}
	return database, nil
}

func readThreads(ctx context.Context, database sqliteQueryer) ([]domain.ThreadRecord, error) {
	rows, err := database.QueryContext(ctx, `SELECT * FROM "threads"`)
	if err != nil {
		return nil, fmt.Errorf("读取 threads 表失败: %w", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("读取 threads 列失败: %w", err)
	}
	threads := make([]domain.ThreadRecord, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("读取 threads 行失败: %w", err)
		}
		thread := domain.ThreadRecord{}
		metadata := make(map[string]json.RawMessage)
		for index, column := range columns {
			value := values[index]
			switch column {
			case "source", "parent_thread_id", "thread_source", "agent_path", "agent_nickname", "forked_from_id":
				metadata[column], _ = json.Marshal(databaseString(value))
			}
			switch column {
			case "id":
				thread.ID = databaseString(value)
			case "rollout_path":
				thread.RolloutPath = databaseString(value)
			case "title":
				thread.Title = databaseString(value)
			case "first_user_message":
				thread.FirstUserMessage = databaseString(value)
			case "cwd":
				thread.CWD = databaseString(value)
			case "model":
				thread.Model = databaseString(value)
			case "model_provider":
				thread.ModelProvider = databaseString(value)
			case "cli_version":
				thread.CLIVersion = databaseString(value)
			case "created_at":
				thread.CreatedAt = databaseTime(value)
			case "updated_at":
				thread.UpdatedAt = databaseTime(value)
			case "tokens_used":
				thread.TokensUsed = databaseInt64(value)
			case "archived":
				thread.Archived = databaseInt64(value) != 0
			}
		}
		thread.ThreadOrigin = threadOrigin(metadata)
		if strings.TrimSpace(thread.ID) != "" {
			threads = append(threads, thread)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 threads 表失败: %w", err)
	}
	return threads, nil
}

func readRelationships(ctx context.Context, database sqliteQueryer, tableNames []string, threads []domain.ThreadRecord) ([]domain.SQLiteRelationship, error) {
	threadIDs := make(map[string]struct{}, len(threads))
	for _, thread := range threads {
		threadIDs[thread.ID] = struct{}{}
	}
	relationships := make([]domain.SQLiteRelationship, 0)
	for _, table := range tableNames {
		columnInfo, err := pragmaColumnInfo(ctx, database, table)
		if err != nil {
			return nil, err
		}
		for _, columnDefinition := range columnInfo {
			column := columnDefinition.Name
			relation := relationshipName(table, column)
			if relation == "" {
				continue
			}
			for threadID := range threadIDs {
				query := `SELECT COUNT(*) FROM "` + quoteIdentifier(table) + `" WHERE "` + quoteIdentifier(column) + `" = ?`
				var count int64
				if err := database.QueryRowContext(ctx, query, threadID).Scan(&count); err != nil {
					return nil, fmt.Errorf("读取关系 %s.%s 失败: %w", table, column, err)
				}
				if count > 0 {
					rowKeys, rowsComplete := readRelationshipRowKeys(ctx, database, table, column, threadID, count, columnInfo)
					relatedIDs := readRelatedThreadIDs(ctx, database, table, column, threadID, columnInfo)
					relatedID := ""
					if len(relatedIDs) > 0 {
						relatedID = relatedIDs[0]
					}
					relationships = appendUniqueSQLiteRelationship(relationships, domain.SQLiteRelationship{
						Table: table, Column: column, ThreadID: threadID,
						RelatedThreadID: relatedID, RelatedThreadIDs: relatedIDs,
						Relation: relation, RowCount: count, RowKeys: rowKeys, RowsComplete: rowsComplete,
					})
				}
			}
		}
	}
	return relationships, nil
}

type tableColumnInfo struct {
	Name       string
	PrimaryKey int
}

func pragmaColumnInfo(ctx context.Context, database sqliteQueryer, table string) ([]tableColumnInfo, error) {
	rows, err := database.QueryContext(ctx, `PRAGMA table_info("`+quoteIdentifier(table)+`")`)
	if err != nil {
		return nil, fmt.Errorf("读取关系表 %s 列失败: %w", table, err)
	}
	defer rows.Close()
	columns := make([]tableColumnInfo, 0)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("读取关系表 %s 列定义失败: %w", table, err)
		}
		columns = append(columns, tableColumnInfo{Name: name, PrimaryKey: primaryKey})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历关系表 %s 列失败: %w", table, err)
	}
	return columns, nil
}

func pragmaColumns(ctx context.Context, database sqliteQueryer, table string) ([]string, error) {
	info, err := pragmaColumnInfo(ctx, database, table)
	if err != nil {
		return nil, err
	}
	columns := make([]string, 0, len(info))
	for _, item := range info {
		columns = append(columns, item.Name)
	}
	return columns, nil
}

func readRelatedThreadIDs(ctx context.Context, database sqliteQueryer, table, column, threadID string, columns []tableColumnInfo) []string {
	relatedColumn := ""
	if column == "parent_thread_id" {
		relatedColumn = "child_thread_id"
	} else if column == "child_thread_id" {
		relatedColumn = "parent_thread_id"
	}
	if relatedColumn == "" || !hasColumn(columns, relatedColumn) {
		return nil
	}
	query := `SELECT DISTINCT "` + quoteIdentifier(relatedColumn) + `" FROM "` + quoteIdentifier(table) + `" WHERE "` + quoteIdentifier(column) + `" = ? ORDER BY 1`
	rows, err := database.QueryContext(ctx, query, threadID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var value any
		if rows.Scan(&value) != nil {
			return nil
		}
		if id := databaseString(value); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func readRelationshipRowKeys(ctx context.Context, database sqliteQueryer, table, column, threadID string, rowCount int64, columns []tableColumnInfo) ([]domain.SQLiteRowReference, bool) {
	keyColumns := make([]string, 0)
	for _, item := range columns {
		if item.PrimaryKey > 0 {
			keyColumns = append(keyColumns, item.Name)
		}
	}
	keyKind := "primary_key"
	selectColumns := keyColumns
	if len(selectColumns) == 0 {
		keyKind = "rowid"
		selectColumns = []string{"rowid"}
	}
	quoted := make([]string, 0, len(selectColumns))
	for _, name := range selectColumns {
		if name == "rowid" {
			quoted = append(quoted, "rowid")
		} else {
			quoted = append(quoted, `"`+quoteIdentifier(name)+`"`)
		}
	}
	query := `SELECT ` + strings.Join(quoted, ", ") + ` FROM "` + quoteIdentifier(table) + `" WHERE "` + quoteIdentifier(column) + `" = ? LIMIT 4096`
	rows, err := database.QueryContext(ctx, query, threadID)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	keys := make([]domain.SQLiteRowReference, 0)
	for rows.Next() {
		values := make([]any, len(selectColumns))
		pointers := make([]any, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return keys, false
		}
		key := encodeRowKey(values)
		if key != "" {
			keys = append(keys, domain.SQLiteRowReference{Key: key, KeyKind: keyKind})
		}
	}
	if rows.Err() != nil {
		return keys, false
	}
	return keys, rowCount <= int64(len(keys))
}

func encodeRowKey(values []any) string {
	if len(values) == 1 {
		return databaseString(values[0])
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func hasColumn(columns []tableColumnInfo, wanted string) bool {
	for _, column := range columns {
		if column.Name == wanted {
			return true
		}
	}
	return false
}

func relationshipName(table, column string) string {
	lowerTable := strings.ToLower(table)
	if column == "thread_id" && strings.Contains(lowerTable, "attachment") {
		return "attachment"
	}
	switch column {
	case "thread_id":
		return "references_thread"
	case "parent_thread_id":
		return "parent_thread"
	case "child_thread_id":
		return "child_thread"
	default:
		return ""
	}
}

func appendUniqueSQLiteRelationship(values []domain.SQLiteRelationship, value domain.SQLiteRelationship) []domain.SQLiteRelationship {
	for _, existing := range values {
		if existing.Table == value.Table && existing.Column == value.Column && existing.ThreadID == value.ThreadID {
			return values
		}
	}
	return append(values, value)
}

func quoteIdentifier(value string) string {
	return strings.ReplaceAll(value, `"`, `""`)
}

func databaseString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case []byte:
		return strings.TrimSpace(string(typed))
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func databaseInt64(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case []byte:
		parsed, _ := strconv.ParseInt(string(typed), 10, 64)
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(typed, 10, 64)
		return parsed
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func databaseTime(value any) time.Time {
	text := databaseString(value)
	if text != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UTC()
		}
		if seconds, err := strconv.ParseInt(text, 10, 64); err == nil {
			if seconds > 1e12 {
				return time.UnixMilli(seconds).UTC()
			}
			return time.Unix(seconds, 0).UTC()
		}
	}
	return time.Time{}
}
