// 中文说明：为稳定副本捕获 SQLite schema、行身份和 session index 的只读快照，不保存对话正文。
package copy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/sqliteguard"
	_ "modernc.org/sqlite"
)

// captureControlPlaneSnapshot 只读取副本候选控制面；缺失或不完整输入会被标记，而不会被猜测为完整。
func captureControlPlaneSnapshot(ctx context.Context, root string, files []FileEntry) ControlPlaneSnapshot {
	snapshot := ControlPlaneSnapshot{SQLite: make([]domain.SQLiteSummary, 0), Rows: make([]SQLiteRowSnapshot, 0), SessionIndex: make([]SessionIndexSnapshot, 0), Complete: true, CompletenessState: "complete", Diagnostics: make([]string, 0)}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			snapshot.Complete = false
			snapshot.Diagnostics = append(snapshot.Diagnostics, "控制面快照因任务取消而不完整")
			break
		}
		path := filepath.Join(root, filepath.FromSlash(file.RelativePath))
		lower := strings.ToLower(file.RelativePath)
		switch {
		case strings.HasSuffix(lower, ".sqlite"), strings.HasSuffix(lower, ".sqlite3"), strings.HasSuffix(lower, ".db"):
			summary, err := compat.AnalyzeSQLitePath(ctx, path)
			if err != nil {
				snapshot.Complete = false
				snapshot.Diagnostics = append(snapshot.Diagnostics, fmt.Sprintf("SQLite %s schema 读取失败：%v", file.RelativePath, err))
				continue
			}
			snapshot.SQLite = append(snapshot.SQLite, summary)
			rows, rowErr := captureSQLiteRowSnapshot(ctx, path, file.RelativePath)
			if rowErr != nil {
				snapshot.Complete = false
				snapshot.Diagnostics = append(snapshot.Diagnostics, fmt.Sprintf("SQLite %s 行快照失败：%v", file.RelativePath, rowErr))
			} else {
				snapshot.Rows = append(snapshot.Rows, rows)
			}
		case filepath.Base(filepath.FromSlash(file.RelativePath)) == "session_index.jsonl":
			index, err := compat.AnalyzeSessionIndexPath(path)
			if err != nil {
				snapshot.Complete = false
				snapshot.Diagnostics = append(snapshot.Diagnostics, fmt.Sprintf("session index %s 读取失败：%v", file.RelativePath, err))
				continue
			}
			digest, _, hashErr := hashFile(ctx, path, DefaultChunkBytes)
			if hashErr != nil {
				snapshot.Complete = false
				snapshot.Diagnostics = append(snapshot.Diagnostics, fmt.Sprintf("session index %s 摘要失败：%v", file.RelativePath, hashErr))
				continue
			}
			snapshot.SessionIndex = append(snapshot.SessionIndex, SessionIndexSnapshot{RelativePath: file.RelativePath, FileSHA256: digest, LineCount: index.LineCount, InvalidJSONLines: index.InvalidJSONLines, Entries: append([]domain.SessionIndexEntry(nil), index.Entries...), Complete: index.InvalidJSONLines == 0})
			if index.InvalidJSONLines > 0 {
				snapshot.Complete = false
				snapshot.Diagnostics = append(snapshot.Diagnostics, fmt.Sprintf("session index %s 包含 %d 条无效行", file.RelativePath, index.InvalidJSONLines))
			}
		}
	}
	sort.Slice(snapshot.SQLite, func(i, j int) bool {
		return snapshot.SQLite[i].SchemaFingerprint < snapshot.SQLite[j].SchemaFingerprint
	})
	sort.Slice(snapshot.Rows, func(i, j int) bool { return snapshot.Rows[i].Database < snapshot.Rows[j].Database })
	sort.Slice(snapshot.SessionIndex, func(i, j int) bool {
		return snapshot.SessionIndex[i].RelativePath < snapshot.SessionIndex[j].RelativePath
	})
	snapshot.SchemaSemanticSHA256 = controlPlaneSchemaDigest(snapshot.SQLite)
	if !snapshot.Complete {
		snapshot.CompletenessState = "incomplete"
	}
	return snapshot
}

func controlPlaneSchemaDigest(values []domain.SQLiteSummary) string {
	data, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func captureSQLiteRowSnapshot(ctx context.Context, path, relative string) (SQLiteRowSnapshot, error) {
	dsn, _, err := sqliteguard.ReadOnlyDSN(path, 1000)
	if err != nil {
		return SQLiteRowSnapshot{}, err
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return SQLiteRowSnapshot{}, err
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return SQLiteRowSnapshot{}, err
	}
	tables, err := metadataTableNames(ctx, database)
	if err != nil {
		return SQLiteRowSnapshot{}, err
	}
	hash := sha256.New()
	var rowCount int64
	for _, table := range tables {
		columns, err := metadataPrimaryColumns(ctx, database, table)
		if err != nil {
			return SQLiteRowSnapshot{}, err
		}
		var count int64
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+quoteMetadataIdentifier(table)+`"`).Scan(&count); err != nil {
			return SQLiteRowSnapshot{}, err
		}
		rowCount += count
		if len(columns) == 0 {
			_, _ = fmt.Fprintf(hash, "table=%s\x00count=%d\n", table, count)
			continue
		}
		selectColumns := make([]string, len(columns))
		for index, column := range columns {
			selectColumns[index] = `"` + quoteMetadataIdentifier(column) + `"`
		}
		query := `SELECT ` + strings.Join(selectColumns, ",") + ` FROM "` + quoteMetadataIdentifier(table) + `" ORDER BY ` + strings.Join(selectColumns, ",")
		rows, err := database.QueryContext(ctx, query)
		if err != nil {
			return SQLiteRowSnapshot{}, err
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(values))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				return SQLiteRowSnapshot{}, err
			}
			_, _ = fmt.Fprintf(hash, "table=%s\x00", table)
			for index, value := range values {
				_, _ = fmt.Fprintf(hash, "%s=%s\x00", columns[index], stableMetadataValue(value))
			}
			_, _ = hash.Write([]byte{'\n'})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return SQLiteRowSnapshot{}, err
		}
		if err := rows.Close(); err != nil {
			return SQLiteRowSnapshot{}, err
		}
	}
	return SQLiteRowSnapshot{Database: filepath.ToSlash(relative), RowCount: rowCount, RowIdentitySHA256: hex.EncodeToString(hash.Sum(nil)), Complete: true}, nil
}

func metadataTableNames(ctx context.Context, database *sql.DB) ([]string, error) {
	rows, err := database.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func metadataPrimaryColumns(ctx context.Context, database *sql.DB, table string) ([]string, error) {
	rows, err := database.QueryContext(ctx, `PRAGMA table_info("`+quoteMetadataIdentifier(table)+`")`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type column struct {
		name string
		pk   int
	}
	columns := make([]column, 0)
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		if pk > 0 {
			columns = append(columns, column{name: name, pk: pk})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(columns, func(i, j int) bool { return columns[i].pk < columns[j].pk })
	result := make([]string, len(columns))
	for index, column := range columns {
		result[index] = column.name
	}
	return result, nil
}

func stableMetadataValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case []byte:
		return "bytes:" + hex.EncodeToString(typed)
	case int64:
		return "int:" + strconv.FormatInt(typed, 10)
	case float64:
		return "float:" + strconv.FormatFloat(typed, 'g', -1, 64)
	case string:
		return "text:" + typed
	default:
		return fmt.Sprintf("%T:%v", value, value)
	}
}

func quoteMetadataIdentifier(value string) string {
	return strings.ReplaceAll(value, `"`, `""`)
}
