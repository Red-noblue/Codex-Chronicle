// 中文说明：在固定只读根目录上按需搜索 JSONL 行，返回可回溯的字节范围和摘要。
package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/searchtext"
)

// SearchJSONL 对已发现的 JSONL 资产执行受限按需搜索，不建立或修改源索引。
func SearchJSONL(ctx context.Context, root, query string, limit int) ([]domain.SearchHit, error) {
	query = normalizeSearchText(query)
	if query == "" {
		return nil, fmt.Errorf("搜索关键词不能为空")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析搜索根失败: %w", err)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("读取搜索根失败: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, fmt.Errorf("搜索根必须是普通目录")
	}
	relativePaths := make([]string, 0)
	walkErr := filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if walkErr != nil {
			return nil
		}
		if path == absoluteRoot || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() || strings.ToLower(filepath.Ext(path)) != ".jsonl" {
			return nil
		}
		relativePaths = append(relativePaths, relativePath(path, absoluteRoot))
		return nil
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if walkErr != nil && walkErr != context.Canceled {
		return nil, walkErr
	}
	return SearchJSONLFiles(ctx, absoluteRoot, relativePaths, query, limit)
}

// SearchJSONLFiles 在已发现的 JSONL 相对路径上执行搜索，避免每个关键词都重新遍历整个工作区。
// 路径仍会逐个通过只读边界校验，文件在扫描后消失时会跳过并交由上层 stale 状态提示用户。
func SearchJSONLFiles(ctx context.Context, root string, relativePaths []string, query string, limit int) ([]domain.SearchHit, error) {
	query = normalizeSearchText(query)
	if query == "" {
		return nil, fmt.Errorf("搜索关键词不能为空")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析搜索根失败: %w", err)
	}
	hits := make([]domain.SearchHit, 0, limit)
	paths := append([]string(nil), relativePaths...)
	sort.Strings(paths)
	for _, relative := range paths {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if strings.ToLower(filepath.Ext(relative)) != ".jsonl" {
			continue
		}
		kind := compat.JSONLFileKind(relative, domain.JSONLScanSummary{})
		if compat.IsCodexSupportPath(relative) || kind == domain.AssetKindInputHistory || kind == domain.AssetKindSessionIndex {
			continue
		}
		isRollout := kind == domain.AssetKindJSONL
		path, resolveErr := ResolveReadOnlyPath(absoluteRoot, relative)
		if resolveErr != nil {
			if os.IsNotExist(resolveErr) {
				continue
			}
			return nil, resolveErr
		}
		conversationID := ""
		pathHitIndexes := make([]int, 0)
		snapshot, scanErr := catalog.ScanLiveJSONLContent(ctx, path, func(frame domain.JSONLFrame, content []byte) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if len(hits) >= limit {
				return catalog.ErrLimitReached
			}
			// 半截或非法尾行只能作为诊断证据，不能被搜索结果当成完整事件。
			if !frame.ValidJSON || frame.Pending {
				return nil
			}
			if frame.TopLevelType == "session_meta" && conversationID == "" {
				conversationID = rolloutConversationID(content)
				isRollout = true
			}
			if !isRollout {
				return nil
			}
			searchable := searchableLine(content)
			if !strings.Contains(normalizeSearchText(searchable), query) {
				return nil
			}
			if conversationID == "" {
				conversationID = compat.StableConversationID(relative)
			}
			hits = append(hits, domain.SearchHit{
				ConversationID: conversationID,
				RelativePath:   relative,
				Line:           frame.LineNumber, StartOffset: frame.StartOffset, EndOffset: frame.EndOffset,
				Snippet: snippet(searchable, query), TopLevelType: frame.TopLevelType,
				SourceStatus: domain.StatusStable, ReadOnly: true,
			})
			pathHitIndexes = append(pathHitIndexes, len(hits)-1)
			return nil
		})
		if scanErr != nil && !errors.Is(scanErr, catalog.ErrLimitReached) {
			return nil, scanErr
		}
		for _, hitIndex := range pathHitIndexes {
			if hitIndex < 0 || hitIndex >= len(hits) {
				continue
			}
			hits[hitIndex].LiveConsistency = string(snapshot.State.Consistency)
			if snapshot.State.Active || snapshot.State.PendingLine {
				hits[hitIndex].SourceStatus = domain.StatusActiveWriting
			}
			if snapshot.State.PendingLine {
				hits[hitIndex].ActivityStatus = domain.StatusPendingJSONLLine
			} else if snapshot.State.Active {
				hits[hitIndex].ActivityStatus = domain.StatusSourceChangedDuringProbe
			} else {
				hits[hitIndex].ActivityStatus = domain.StatusStable
			}
		}
		if conversationID != "" {
			for index := range hits {
				if hits[index].RelativePath == relative {
					hits[index].ConversationID = conversationID
				}
			}
		}
		if len(hits) >= limit {
			break
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return hits, nil
}

func rolloutConversationID(content []byte) string {
	var record map[string]json.RawMessage
	if json.Unmarshal(content, &record) != nil {
		return ""
	}
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(record["payload"], &payload)
	for _, raw := range []json.RawMessage{payload["id"], record["id"], payload["thread_id"], record["thread_id"]} {
		var value string
		if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
			return compat.StableConversationID(strings.TrimSpace(value))
		}
	}
	return ""
}

func searchableLine(content []byte) string {
	// 原始 JSON 保留字段名和转义形式；再追加字符串值的解码结果，
	// 使用户输入真实换行时也能命中 JSON 中的 "\\n"。
	text := string(content)
	var value any
	if decodeErr := json.Unmarshal(content, &value); decodeErr == nil {
		text += " " + collectStrings(value, 0)
	}
	return text
}

func normalizeSearchText(value string) string {
	return searchtext.Normalize(value)
}

func snippet(value, query string) string {
	normalized := normalizeSearchText(value)
	position := strings.Index(normalized, query)
	if position < 0 {
		return limitRunes(normalized, 240)
	}
	start := position - 80
	if start < 0 {
		start = 0
	}
	end := position + len(query) + 160
	if end > len(normalized) {
		end = len(normalized)
	}
	return limitRunes(normalized[start:end], 280)
}

func limitRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func collectStrings(value any, depth int) string {
	if depth > 8 {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return limitRunes(typed, 4096)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := collectStrings(item, depth+1); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		parts := make([]string, 0, len(typed))
		for key, item := range typed {
			if key == "encrypted_content" || key == "base_instructions" {
				continue
			}
			if text := collectStrings(item, depth+1); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}
