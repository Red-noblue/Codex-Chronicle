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

// SearchHitFilter 复核候选并可调整其定位。结果上限只计入复核通过的命中。
type SearchHitFilter func(domain.SearchHit) (domain.SearchHit, bool, error)

// SearchProgress 在搜索推进时报告新命中和已检查的正文记录数。
type SearchProgress struct {
	Stage           domain.SearchMatchOrigin
	FilesDone       int
	FilesTotal      int
	RecordsDone     int64
	SourceBytesRead int64 // 本文件的候选读取量，含前缀校验，不含目录扫描及阅读视图复核。
	PrefixBytesRead int64
	Hits            []domain.SearchHit
}

type SearchObserver func(SearchProgress) error

// SearchFileCursor 指向已检查的最后一条完整记录之后。
type SearchFileCursor struct {
	Offset         int64
	Line           uint64
	ConversationID string
}

// SearchJSONL 对已发现的 JSONL 资产执行受限按需搜索，不建立或修改源索引。
func SearchJSONL(ctx context.Context, root, query string, limit int) ([]domain.SearchHit, error) {
	return SearchJSONLFiltered(ctx, root, query, limit, false, nil)
}

func SearchJSONLFiltered(ctx context.Context, root, query string, limit int, technical bool, filter SearchHitFilter) ([]domain.SearchHit, error) {
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
	return SearchJSONLFilesFiltered(ctx, absoluteRoot, relativePaths, query, limit, technical, filter)
}

// SearchJSONLFiles 在已发现的 JSONL 相对路径上执行搜索，避免每个关键词都重新遍历整个工作区。
// 路径仍会逐个通过只读边界校验，文件在扫描后消失时会跳过并交由上层 stale 状态提示用户。
func SearchJSONLFiles(ctx context.Context, root string, relativePaths []string, query string, limit int) ([]domain.SearchHit, error) {
	return SearchJSONLFilesFiltered(ctx, root, relativePaths, query, limit, false, nil)
}

func SearchJSONLFilesFiltered(ctx context.Context, root string, relativePaths []string, query string, limit int, technical bool, filter SearchHitFilter) ([]domain.SearchHit, error) {
	return SearchJSONLFilesObserved(ctx, root, relativePaths, query, limit, technical, filter, nil)
}

func SearchJSONLFilesObserved(ctx context.Context, root string, relativePaths []string, query string, limit int, technical bool, filter SearchHitFilter, observe SearchObserver) ([]domain.SearchHit, error) {
	query = searchtext.Normalize(query)
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
	for index, relative := range paths {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var checkProgress func(int64) error
		if observe != nil {
			checkProgress = func(records int64) error {
				return observe(SearchProgress{Stage: domain.SearchMatchSource, FilesDone: index, FilesTotal: len(paths), RecordsDone: records})
			}
		}
		found, sourceBytes, err := searchJSONLFile(ctx, absoluteRoot, relative, query, limit-len(hits), technical, filter, checkProgress, nil, SearchFileCursor{})
		if err != nil {
			return nil, err
		}
		hits = append(hits, found...)
		if observe != nil {
			if err := observe(SearchProgress{Stage: domain.SearchMatchSource, FilesDone: index + 1, FilesTotal: len(paths), SourceBytesRead: sourceBytes, Hits: found}); err != nil {
				return nil, err
			}
		}
		if len(hits) >= limit {
			break
		}
	}
	return hits, ctx.Err()
}

// SearchJSONLFileSuffix 复用直接扫描的文字提取规则，从指定的文件版本和位置继续。
func SearchJSONLFileSuffix(ctx context.Context, root, relative, query string, limit int, expected os.FileInfo, cursor SearchFileCursor, filter SearchHitFilter, checkProgress func(int64) error) ([]domain.SearchHit, int64, error) {
	return searchJSONLFile(ctx, root, relative, query, limit, false, filter, checkProgress, expected, cursor)
}

func searchJSONLFile(ctx context.Context, absoluteRoot, relative, query string, limit int, technical bool, filter SearchHitFilter, checkProgress func(int64) error, expected os.FileInfo, cursor SearchFileCursor) ([]domain.SearchHit, int64, error) {
	if strings.ToLower(filepath.Ext(relative)) != ".jsonl" {
		return nil, 0, nil
	}
	kind := compat.JSONLFileKind(relative, domain.JSONLScanSummary{})
	if compat.IsCodexSupportPath(relative) || kind == domain.AssetKindInputHistory || kind == domain.AssetKindSessionIndex {
		return nil, 0, nil
	}
	isRollout := kind == domain.AssetKindJSONL || cursor.Offset > 0
	path, resolveErr := ResolveReadOnlyPath(absoluteRoot, relative)
	if resolveErr != nil {
		if expected == nil && os.IsNotExist(resolveErr) {
			return nil, 0, nil
		}
		return nil, 0, resolveErr
	}
	conversationID := cursor.ConversationID
	var hits []domain.SearchHit
	visit := func(frame domain.JSONLFrame, content []byte) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if len(hits) >= limit {
			return catalog.ErrLimitReached
		}
		frame.LineNumber += cursor.Line
		frame.StartOffset += cursor.Offset
		frame.EndOffset += cursor.Offset
		frame.ContentEndOffset += cursor.Offset
		if checkProgress != nil && frame.LineNumber%256 == 0 {
			if err := checkProgress(int64(frame.LineNumber)); err != nil {
				return err
			}
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
		match := ""
		if technical {
			match = searchtext.Snippet(searchtext.Normalize(string(content)), query)
		}
		if match == "" {
			match = searchtext.Snippet(searchtext.Record(frame, content), query)
		}
		if match == "" {
			return nil
		}
		if conversationID == "" {
			conversationID = compat.StableConversationID(relative)
		}
		hit := domain.SearchHit{
			ConversationID: conversationID,
			RelativePath:   relative,
			Line:           frame.LineNumber, StartOffset: frame.StartOffset, EndOffset: frame.EndOffset,
			Snippet: match, TopLevelType: frame.TopLevelType,
			MatchOrigin:  domain.SearchMatchSource,
			SourceStatus: domain.StatusStable, ReadOnly: true,
		}
		if filter != nil {
			var ok bool
			var err error
			hit, ok, err = filter(hit)
			if err != nil || !ok {
				return err
			}
		}
		hits = append(hits, hit)
		return nil
	}
	var snapshot catalog.LiveJSONLSnapshot
	var scanErr error
	if expected == nil {
		snapshot, scanErr = catalog.ScanLiveJSONLContent(ctx, path, visit)
	} else {
		snapshot, scanErr = catalog.ScanLiveJSONLSuffix(ctx, path, expected, cursor.Offset, visit)
	}
	if scanErr != nil && !errors.Is(scanErr, catalog.ErrLimitReached) {
		return nil, 0, scanErr
	}
	if snapshot.State.Consistency == catalog.LiveRewriteConflict || snapshot.State.Consistency == catalog.LiveTruncated {
		return nil, 0, fmt.Errorf("搜索期间文件被替换或重写，请重试：%s", relative)
	}
	for hitIndex := range hits {
		setLiveSearchStatus(&hits[hitIndex], snapshot.State)
	}
	if filter == nil && conversationID != "" {
		for index := range hits {
			hits[index].ConversationID = conversationID
		}
	}
	return hits, snapshot.State.CapturedBytes, nil
}

func setLiveSearchStatus(hit *domain.SearchHit, state catalog.LiveFileState) {
	hit.LiveConsistency = string(state.Consistency)
	if state.Active || state.PendingLine {
		hit.SourceStatus = domain.StatusActiveWriting
	}
	if state.PendingLine {
		hit.ActivityStatus = domain.StatusPendingJSONLLine
	} else if state.Active {
		hit.ActivityStatus = domain.StatusSourceChangedDuringProbe
	} else {
		hit.ActivityStatus = domain.StatusStable
	}
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

// limitRunes 用于限制摘要输出中的单段正文。
func limitRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
