// 中文说明：以只读方式解析 session index JSONL，提取可回溯的线程标识和行号。
package compat

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
)

// AnalyzeSessionIndexPath 读取 session_index.jsonl 的结构，不保存索引正文。
func AnalyzeSessionIndexPath(path string) (domain.SessionIndexSummary, error) {
	input, err := os.Open(path)
	if err != nil {
		return domain.SessionIndexSummary{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return domain.SessionIndexSummary{}, err
	}
	summary := domain.SessionIndexSummary{Entries: make([]domain.SessionIndexEntry, 0)}
	_, err = catalog.ScanReaderContent(io.LimitReader(input, info.Size()), func(frame domain.JSONLFrame, content []byte) error {
		summary.LineCount++
		if !frame.ValidJSON || frame.Pending {
			summary.InvalidJSONLines++
			return nil
		}
		var record map[string]json.RawMessage
		if err := json.Unmarshal(content, &record); err != nil {
			summary.InvalidJSONLines++
			return nil
		}
		id := firstNonEmpty(rawString(record["id"]), rawString(record["thread_id"]), rawString(record["session_id"]))
		if strings.TrimSpace(id) != "" {
			summary.Entries = append(summary.Entries, domain.SessionIndexEntry{ID: id, Line: frame.LineNumber})
		}
		return nil
	})
	if err != nil {
		return summary, fmt.Errorf("读取 session index 失败: %w", err)
	}
	return summary, nil
}
