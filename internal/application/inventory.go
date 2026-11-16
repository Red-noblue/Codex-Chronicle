// 中文说明：实现只读文件 inventory，递归发现资产并解析 JSONL framing。
package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

// Scanner 只读扫描指定根目录，不创建 catalog、索引或临时文件。
// 同一 daemon 内会按路径和文件身份复用未变化资产的解析投影，避免 UI 请求反复读取大文件。
type Scanner struct {
	state *scannerState
}

// ScanOptions 控制扫描是否构建完整 conversation asset graph；文件和关系解析仍保持只读。
type ScanOptions struct {
	IncludeAssetGraphs bool
}

type scannerState struct {
	mu    sync.RWMutex
	cache map[string]cachedFile
}

type cachedFile struct {
	info      os.FileInfo
	file      domain.FileInventory
	auxiliary [2]cachedAuxiliary
}

type cachedAuxiliary struct {
	exists      bool
	unavailable bool
	info        os.FileInfo
}

const maxBackupPrefixProbeBytes int64 = 256 << 20

type rolloutOverlapEvidence struct {
	Suppress   bool
	Diagnostic domain.Diagnostic
}

// ScanProgress 是扫描过程中的轻量进度事件，不包含正文。
type ScanProgress struct {
	RelativePath string
	FilesSeen    int
	BytesSeen    int64
	CacheHits    int
	FilesParsed  int
}

// ProgressFunc 接收扫描进度；实现应快速返回，避免阻塞文件扫描。
type ProgressFunc func(ScanProgress)

// NewScanner 创建一个带进程内解析缓存的只读扫描器。
func NewScanner() Scanner {
	return Scanner{state: &scannerState{cache: make(map[string]cachedFile)}}
}

// Scan 递归扫描 root。root 必须是目录，符号链接不会被跟随。
func (s Scanner) Scan(ctx context.Context, root string) (domain.InventoryReport, error) {
	return s.ScanWithOptions(ctx, root, ScanOptions{IncludeAssetGraphs: true}, nil)
}

// ScanWithProgress 递归扫描 root，并在每个普通文件完成只读解析后报告进度。
func (s Scanner) ScanWithProgress(ctx context.Context, root string, progress ProgressFunc) (domain.InventoryReport, error) {
	return s.ScanWithOptions(ctx, root, ScanOptions{IncludeAssetGraphs: true}, progress)
}

// ScanWithoutGraphs 扫描列表、统计和维护所需的只读数据，但跳过完整资产图构建。
func (s Scanner) ScanWithoutGraphs(ctx context.Context, root string) (domain.InventoryReport, error) {
	return s.ScanWithOptions(ctx, root, ScanOptions{}, nil)
}

// ScanWithOptions 递归扫描 root，并按选项构建派生资产图。
func (s Scanner) ScanWithOptions(ctx context.Context, root string, options ScanOptions, progress ProgressFunc) (domain.InventoryReport, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return domain.InventoryReport{}, fmt.Errorf("解析扫描根失败: %w", err)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return domain.InventoryReport{}, fmt.Errorf("读取扫描根失败: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return domain.InventoryReport{}, fmt.Errorf("扫描根不能是符号链接: %s", absoluteRoot)
	}
	if !rootInfo.IsDir() {
		return domain.InventoryReport{}, fmt.Errorf("扫描根不是目录: %s", absoluteRoot)
	}

	report := domain.InventoryReport{
		SchemaVersion: 1,
		Root:          absoluteRoot,
		ReadOnly:      true,
		GeneratedAt:   time.Now().UTC(),
		Files:         make([]domain.FileInventory, 0),
	}
	var scannedBytes int64
	cacheHits := 0
	filesParsed := 0

	walkErr := filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if walkErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", relativePath(path, absoluteRoot), walkErr))
			return nil
		}
		if path == absoluteRoot {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		file, cacheHit, fileErr := s.inspectCached(ctx, path, absoluteRoot, entry)
		if fileErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", relativePath(path, absoluteRoot), fileErr))
			return nil
		}
		if cacheHit {
			cacheHits++
		} else {
			filesParsed++
		}
		report.Files = append(report.Files, file)
		scannedBytes += file.SizeBytes
		if progress != nil {
			progress(ScanProgress{
				RelativePath: relativePath(path, absoluteRoot), FilesSeen: len(report.Files), BytesSeen: scannedBytes,
				CacheHits: cacheHits, FilesParsed: filesParsed,
			})
		}
		return nil
	})
	if walkErr != nil {
		return report, walkErr
	}
	sort.Slice(report.Files, func(i, j int) bool {
		return report.Files[i].RelativePath < report.Files[j].RelativePath
	})
	report.Conversations = aggregateConversations(report.Root, report.Files, options)
	applyEffectiveMetadata(ctx, &report)
	return report, nil
}

// CheckFreshness 只比较目录中的文件元数据，不解析正文，用于判断已缓存报告是否可能过期。
// 它不会创建、修改或锁定源 .codex 文件；任何新增、消失或大小/修改时间变化都会标记为 stale。
func (s Scanner) CheckFreshness(ctx context.Context, root string, report domain.InventoryReport) (bool, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false, fmt.Errorf("解析新鲜度扫描根失败: %w", err)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return true, fmt.Errorf("读取新鲜度扫描根失败: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return true, fmt.Errorf("新鲜度扫描根不是普通目录: %s", absoluteRoot)
	}
	type fileStamp struct {
		size    int64
		modTime time.Time
	}
	expected := make(map[string]fileStamp, len(report.Files))
	for _, file := range report.Files {
		expected[file.RelativePath] = fileStamp{size: file.SizeBytes, modTime: file.ModTime}
	}
	seen := make(map[string]struct{}, len(expected))
	stale := false
	walkErr := filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if walkErr != nil {
			stale = true
			return nil
		}
		if path == absoluteRoot || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		relative := relativePath(path, absoluteRoot)
		info, statErr := entry.Info()
		if statErr != nil {
			stale = true
			return nil
		}
		seen[relative] = struct{}{}
		stamp, ok := expected[relative]
		if !ok || stamp.size != info.Size() || !stamp.modTime.Equal(info.ModTime().UTC()) {
			stale = true
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return false, walkErr
		}
		return true, walkErr
	}
	if len(seen) != len(expected) {
		stale = true
	}
	return stale, nil
}

func (s Scanner) inspectCached(ctx context.Context, path, root string, entry fs.DirEntry) (domain.FileInventory, bool, error) {
	info, err := entry.Info()
	if err != nil {
		return domain.FileInventory{}, false, err
	}
	state := s.state
	if state == nil {
		file, inspectErr := inspectFile(ctx, path, root)
		return file, false, inspectErr
	}
	state.mu.RLock()
	cached, ok := state.cache[path]
	state.mu.RUnlock()
	auxiliary := sqliteAuxiliaryState(path)
	if ok && os.SameFile(cached.info, info) && cached.info.Size() == info.Size() && cached.info.ModTime().Equal(info.ModTime()) && sameAuxiliaryState(cached.auxiliary, auxiliary) {
		file := cached.file
		file.SizeBytes = info.Size()
		file.ModTime = info.ModTime().UTC()
		return file, true, nil
	}
	file, inspectErr := inspectFile(ctx, path, root)
	if inspectErr != nil {
		return file, false, inspectErr
	}
	afterAuxiliary := sqliteAuxiliaryState(path)
	if !sameAuxiliaryState(auxiliary, afterAuxiliary) {
		// 不把旧事务读到的线程缓存为较新 WAL 的投影；下次扫描须重新解析。
		file.ChangedDuringScan = true
		file.Status = domain.StatusActiveWriting
		state.mu.Lock()
		delete(state.cache, path)
		state.mu.Unlock()
		return file, false, nil
	}
	state.mu.Lock()
	state.cache[path] = cachedFile{info: info, file: file, auxiliary: afterAuxiliary}
	state.mu.Unlock()
	return file, false, nil
}

func sqliteAuxiliaryState(path string) [2]cachedAuxiliary {
	if kindForPath(path) != domain.AssetKindSQLite {
		return [2]cachedAuxiliary{}
	}
	var state [2]cachedAuxiliary
	for index, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if err == nil {
			state[index] = cachedAuxiliary{exists: true, info: info}
			continue
		}
		state[index] = cachedAuxiliary{unavailable: !os.IsNotExist(err)}
	}
	return state
}

func sameAuxiliaryState(left, right [2]cachedAuxiliary) bool {
	for index := range left {
		if left[index].exists != right[index].exists || left[index].unavailable != right[index].unavailable {
			return false
		}
		if left[index].exists {
			if !os.SameFile(left[index].info, right[index].info) || left[index].info.Size() != right[index].info.Size() || !left[index].info.ModTime().Equal(right[index].info.ModTime()) {
				return false
			}
		}
	}
	return true
}

func inspectFile(ctx context.Context, path, root string) (domain.FileInventory, error) {
	before, err := os.Stat(path)
	if err != nil {
		return domain.FileInventory{}, err
	}
	file := domain.FileInventory{
		Asset: domain.Asset{
			RelativePath: relativePath(path, root),
			Kind:         kindForPath(relativePath(path, root)),
			SizeBytes:    before.Size(),
			ModTime:      before.ModTime().UTC(),
			Status:       domain.StatusStable,
		},
	}
	supportFile := compat.IsCodexSupportPath(file.RelativePath)
	if !supportFile && (file.Kind == domain.AssetKindJSONL || file.Kind == domain.AssetKindGenericJSONL) {
		var rollout domain.RolloutSummary
		rollout, summary, analyzeErr := compat.AnalyzeRolloutPath(path)
		file.JSONL = &summary
		file.Kind = compat.JSONLFileKind(file.RelativePath, summary)
		if file.Kind == domain.AssetKindJSONL {
			file.Rollout = &rollout
		}
		if file.Rollout != nil && (summary.PendingLines > 0 || summary.InvalidJSONLines > 0 || summary.InvalidUTF8Lines > 0) && rollout.Status != domain.StatusStable {
			// rollout 投影已经发现半截/非法记录时，物理文件也必须进入同一保守状态，
			// 避免 UI 把存在待写尾部的 JSONL 误显示为稳定资产。
			file.Status = rollout.Status
		}
		err = analyzeErr
		if err != nil {
			file.Status = domain.StatusUnknown
			file.Error = err.Error()
			file.Rollout = nil
		}
	}
	if file.Kind == domain.AssetKindSessionIndex && !supportFile {
		index, indexErr := compat.AnalyzeSessionIndexPath(path)
		file.SessionIndex = &index
		if indexErr != nil {
			file.Status = domain.StatusUnknown
			file.Error = indexErr.Error()
		}
		if index.InvalidJSONLines > 0 && file.Status == domain.StatusStable {
			file.Status = domain.StatusUnknown
		}
	}
	if file.Kind == domain.AssetKindSQLite && !supportFile {
		summary, analyzeErr := compat.AnalyzeSQLitePath(ctx, path)
		if analyzeErr != nil {
			file.Error = analyzeErr.Error()
			file.Status = domain.StatusUnknown
			file.SQLite = &domain.SQLiteSummary{
				ReadOnly: false,
				Diagnostic: &domain.Diagnostic{
					Code: "SQLITE_UNREADABLE", Severity: domain.SeverityBlock,
					Message:  "SQLite 文件无法以只读方式解析，不能执行修复或写操作",
					Evidence: analyzeErr.Error(), ReadOnly: true,
				},
			}
		} else {
			threads, relationships, threadErr := compat.AnalyzeThreadDatabasePath(ctx, path)
			if threadErr != nil {
				file.Status = domain.StatusUnknown
				file.Error = threadErr.Error()
				if summary.Diagnostic == nil {
					summary.Diagnostic = &domain.Diagnostic{Code: "SQLITE_THREADS_UNREADABLE", Severity: domain.SeverityBlock, Message: "SQLite 线程关系无法以只读方式读取，不能建立完整对话资产图", Evidence: threadErr.Error(), ReadOnly: true}
				}
			} else {
				summary.Threads = threads
				summary.Relationships = relationships
			}
			file.SQLite = &summary
			if summary.Diagnostic != nil && summary.Diagnostic.Code == "SQLITE_IMMUTABLE_WAL_VIEW" {
				// immutable 视图不会合并 WAL；结构虽然可读，但不能宣称已稳定覆盖最新提交。
				file.Status = domain.StatusUnknown
			}
		}
	}
	after, statErr := os.Stat(path)
	if statErr != nil {
		file.Status = domain.StatusConflict
		file.Error = statErr.Error()
		return file, nil
	}
	file.ChangedDuringScan = before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime())
	if file.ChangedDuringScan {
		file.Status = domain.StatusActiveWriting
		if file.Rollout != nil {
			file.Rollout.Status = domain.StatusActiveWriting
			file.Rollout.Diagnostics = append(file.Rollout.Diagnostics, domain.Diagnostic{
				Code: "FILE_CHANGED_DURING_SCAN", Severity: domain.SeverityBlock,
				Message: "文件在只读扫描期间发生变化，只能阅读和诊断", ReadOnly: true,
			})
		}
	}
	return file, nil
}

func scanJSONL(path string) (*domain.JSONLScanSummary, error) {
	summary, err := catalog.ScanPath(path, nil)
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

func aggregateConversations(root string, files []domain.FileInventory, options ScanOptions) []domain.ConversationSummary {
	byID := make(map[string]domain.ConversationSummary)
	threadDatabaseSeen := false
	threadIDs := make(map[string]struct{})
	threadPaths := make(map[string]struct{})
	countedRolloutContent := make(map[string]map[string]string)
	overlaps := detectBackupRolloutOverlaps(root, files)
	for _, file := range files {
		if file.Rollout == nil {
			continue
		}
		rollout := file.Rollout
		conversation, ok := byID[rollout.ConversationID]
		if !ok {
			conversation = domain.ConversationSummary{
				ID: rollout.ConversationID, SourceID: rollout.SourceID,
				Title: rollout.Title, FirstUserText: rollout.FirstUserText,
				CWD: rollout.CWD, Model: rollout.Model, ModelProvider: rollout.ModelProvider,
				CLIVersion: rollout.CLIVersion, StartedAt: rollout.StartedAt,
				UpdatedAt: rollout.UpdatedAt, Status: rollout.Status, Orphan: rollout.Orphan,
				RolloutPaths: make([]string, 0, 1), RelatedAssetPaths: make([]string, 0, 1), Diagnostics: make([]domain.Diagnostic, 0),
			}
		}
		conversation.RolloutPaths = appendUnique(conversation.RolloutPaths, file.RelativePath)
		duplicateRollout := false
		if overlap, exists := overlaps[file.RelativePath]; exists {
			duplicateRollout = overlap.Suppress
			conversation.Diagnostics = appendDiagnostic(conversation.Diagnostics, overlap.Diagnostic)
		}
		if file.JSONL != nil && strings.TrimSpace(file.JSONL.ContentSHA256) != "" {
			seen := countedRolloutContent[rollout.ConversationID]
			if seen == nil {
				seen = make(map[string]string)
				countedRolloutContent[rollout.ConversationID] = seen
			}
			if previousPath, exists := seen[file.JSONL.ContentSHA256]; exists {
				duplicateRollout = true
				conversation.Diagnostics = appendDiagnostic(conversation.Diagnostics, domain.Diagnostic{
					Code: "ROLLOUT_DUPLICATE_CONTENT", Severity: domain.SeverityInfo,
					Message:  "同一逻辑对话发现内容相同的 rollout 副本；物理路径保留，但统计只计一次",
					Evidence: fmt.Sprintf("%s 与 %s；sha256=%s", file.RelativePath, previousPath, file.JSONL.ContentSHA256), ReadOnly: true,
				})
			} else {
				seen[file.JSONL.ContentSHA256] = file.RelativePath
			}
		}
		if !duplicateRollout {
			conversation.EventCount += rollout.EventCount
			conversation.UserMessages += rollout.UserMessages
			conversation.AssistantMessages += rollout.AssistantMessages
			conversation.TurnCount += rollout.TurnCount
			conversation.TokenUsage = mergeTokenUsage(conversation.TokenUsage, rollout.TokenUsage)
		}
		conversation.Status = stricterStatus(conversation.Status, rollout.Status)
		conversation.Orphan = conversation.Orphan && rollout.Orphan
		conversation.Title = firstNonEmpty(conversation.Title, rollout.Title)
		conversation.FirstUserText = firstNonEmpty(conversation.FirstUserText, rollout.FirstUserText)
		conversation.CWD = firstNonEmpty(conversation.CWD, rollout.CWD)
		conversation.Model = firstNonEmpty(conversation.Model, rollout.Model)
		conversation.ModelProvider = firstNonEmpty(conversation.ModelProvider, rollout.ModelProvider)
		conversation.CLIVersion = firstNonEmpty(conversation.CLIVersion, rollout.CLIVersion)
		if conversation.StartedAt.IsZero() || (!rollout.StartedAt.IsZero() && rollout.StartedAt.Before(conversation.StartedAt)) {
			conversation.StartedAt = rollout.StartedAt
		}
		if rollout.UpdatedAt.After(conversation.UpdatedAt) {
			conversation.UpdatedAt = rollout.UpdatedAt
		}
		for _, diagnostic := range rollout.Diagnostics {
			if len(conversation.Diagnostics) >= 16 {
				break
			}
			conversation.Diagnostics = append(conversation.Diagnostics, diagnostic)
		}
		byID[conversation.ID] = conversation
		if file.SQLite != nil && len(file.SQLite.Threads) > 0 {
			threadDatabaseSeen = true
		}
	}
	for _, file := range files {
		// 备份控制文件保留在 inventory，但其过期线程不能覆盖或复活当前会话。
		if file.SQLite == nil || len(file.SQLite.Threads) == 0 || isBackupRolloutPath(file.RelativePath) {
			continue
		}
		threadDatabaseSeen = true
		for _, thread := range file.SQLite.Threads {
			conversationID := compat.StableConversationID(thread.ID)
			conversation, ok := byID[conversationID]
			if !ok {
				conversation = domain.ConversationSummary{ID: conversationID, SourceID: thread.ID, Status: domain.StatusStable, RolloutPaths: make([]string, 0), RelatedAssetPaths: make([]string, 0), Diagnostics: make([]domain.Diagnostic, 0), Relationships: make([]domain.SQLiteRelationship, 0)}
			}
			conversation.RelatedAssetPaths = appendUnique(conversation.RelatedAssetPaths, file.RelativePath)
			conversation.SourceID = firstNonEmpty(conversation.SourceID, thread.ID)
			if !compat.IsInjectedContext(thread.Title) {
				conversation.Title = firstNonEmpty(thread.Title, conversation.Title)
			}
			if !compat.IsInjectedContext(thread.FirstUserMessage) {
				conversation.FirstUserText = firstNonEmpty(thread.FirstUserMessage, conversation.FirstUserText)
			}
			conversation.CWD = firstNonEmpty(thread.CWD, conversation.CWD)
			conversation.Model = firstNonEmpty(thread.Model, conversation.Model)
			conversation.ModelProvider = firstNonEmpty(thread.ModelProvider, conversation.ModelProvider)
			conversation.CLIVersion = firstNonEmpty(thread.CLIVersion, conversation.CLIVersion)
			if conversation.StartedAt.IsZero() || (!thread.CreatedAt.IsZero() && thread.CreatedAt.Before(conversation.StartedAt)) {
				conversation.StartedAt = thread.CreatedAt
			}
			if thread.UpdatedAt.After(conversation.UpdatedAt) {
				conversation.UpdatedAt = thread.UpdatedAt
			}
			if thread.TokensUsed > conversation.TokenUsage.TotalTokens {
				conversation.TokenUsage.TotalTokens = thread.TokensUsed
			}
			if thread.RolloutPath != "" {
				relative, safe := normalizeRolloutPath(root, thread.RolloutPath)
				if !safe {
					conversation.Status = stricterStatus(conversation.Status, domain.StatusConflict)
					conversation.Diagnostics = appendDiagnostic(conversation.Diagnostics, domain.Diagnostic{Code: "ROLLOUT_PATH_UNSAFE", Severity: domain.SeverityBlock, Message: "SQLite rollout_path 越出扫描根，拒绝建立可写关系", Evidence: thread.RolloutPath, ReadOnly: true})
				} else {
					conversation.RolloutPaths = appendUnique(conversation.RolloutPaths, relative)
					threadIDs[thread.ID] = struct{}{}
					threadPaths[relative] = struct{}{}
					if !containsFile(files, relative) {
						conversation.Status = stricterStatus(conversation.Status, domain.StatusConflict)
						conversation.Diagnostics = appendDiagnostic(conversation.Diagnostics, domain.Diagnostic{Code: "ROLLOUT_MISSING", Severity: domain.SeverityBlock, Message: "SQLite 线程引用的 rollout 文件不存在", Evidence: relative, ReadOnly: true})
					}
				}
			} else {
				conversation.Status = stricterStatus(conversation.Status, domain.StatusUnknown)
				conversation.Diagnostics = appendDiagnostic(conversation.Diagnostics, domain.Diagnostic{Code: "ROLLOUT_PATH_MISSING", Severity: domain.SeverityBlock, Message: "SQLite 线程没有 rollout_path，关系不完整", ReadOnly: true})
			}
			byID[conversation.ID] = conversation
		}
		for _, relationship := range file.SQLite.Relationships {
			conversationID := compat.StableConversationID(relationship.ThreadID)
			conversation, ok := byID[conversationID]
			if !ok {
				conversation = domain.ConversationSummary{ID: conversationID, SourceID: relationship.ThreadID, Status: domain.StatusUnknown, RolloutPaths: make([]string, 0), RelatedAssetPaths: make([]string, 0), Diagnostics: make([]domain.Diagnostic, 0), Relationships: make([]domain.SQLiteRelationship, 0)}
				conversation.Diagnostics = appendDiagnostic(conversation.Diagnostics, domain.Diagnostic{Code: "RELATION_ONLY_THREAD", Severity: domain.SeverityWarning, Message: "SQLite 关系引用的线程没有 threads 主记录，暂按孤儿资产只读展示", Evidence: relationship.ThreadID, ReadOnly: true})
			}
			conversation.RelatedAssetPaths = appendUnique(conversation.RelatedAssetPaths, file.RelativePath)
			conversation.Relationships = appendUniqueRelationship(conversation.Relationships, relationship)
			byID[conversationID] = conversation
		}
	}
	for _, file := range files {
		if file.SQLite == nil || len(file.SQLite.Threads) > 0 || len(file.SQLite.Relationships) == 0 || isBackupRolloutPath(file.RelativePath) {
			continue
		}
		for _, relationship := range file.SQLite.Relationships {
			conversationID := compat.StableConversationID(relationship.ThreadID)
			conversation, ok := byID[conversationID]
			if !ok {
				continue
			}
			conversation.RelatedAssetPaths = appendUnique(conversation.RelatedAssetPaths, file.RelativePath)
			conversation.Relationships = appendUniqueRelationship(conversation.Relationships, relationship)
			byID[conversationID] = conversation
		}
	}
	for _, file := range files {
		if file.SessionIndex == nil || len(file.SessionIndex.Entries) == 0 || isBackupRolloutPath(file.RelativePath) {
			continue
		}
		for _, entry := range file.SessionIndex.Entries {
			conversationID := compat.StableConversationID(entry.ID)
			conversation, ok := byID[conversationID]
			if !ok {
				continue
			}
			conversation.RelatedAssetPaths = appendUnique(conversation.RelatedAssetPaths, file.RelativePath)
			byID[conversationID] = conversation
		}
	}
	if threadDatabaseSeen {
		for id, conversation := range byID {
			if conversation.SourceID != "" {
				if _, ok := threadIDs[conversation.SourceID]; !ok {
					conversation.Orphan = true
					conversation.Status = stricterStatus(conversation.Status, domain.StatusUnknown)
					conversation.Diagnostics = appendDiagnostic(conversation.Diagnostics, domain.Diagnostic{Code: "THREAD_NOT_IN_STATE", Severity: domain.SeverityWarning, Message: "rollout 存在但未在可观测 threads 表中关联，暂按孤儿资产只读展示", ReadOnly: true})
				}
			}
			for _, path := range conversation.RolloutPaths {
				if _, ok := threadPaths[path]; !ok && containsFile(files, path) {
					conversation.Orphan = true
				}
			}
			byID[id] = conversation
		}
	}
	attachConversationFamilies(byID, files)
	conversations := make([]domain.ConversationSummary, 0, len(byID))
	for _, conversation := range byID {
		sort.Strings(conversation.RolloutPaths)
		sort.Strings(conversation.RelatedAssetPaths)
		if options.IncludeAssetGraphs {
			conversation.AssetGraph = pointerToGraph(buildConversationAssetGraph(root, files, conversation))
			conversation.AssetSetHash = conversation.AssetGraph.Hash
		}
		conversations = append(conversations, conversation)
	}
	sort.Slice(conversations, func(i, j int) bool {
		if conversations[i].UpdatedAt.Equal(conversations[j].UpdatedAt) {
			return conversations[i].ID < conversations[j].ID
		}
		return conversations[i].UpdatedAt.After(conversations[j].UpdatedAt)
	})
	return conversations
}

// detectBackupRolloutOverlaps 只对备份目录中的 rollout 做有限前缀探测。
// 备份通常是当前 rollout 的旧快照；成功确认后保留备份路径，但不重复计算共同历史。
func detectBackupRolloutOverlaps(root string, files []domain.FileInventory) map[string]rolloutOverlapEvidence {
	result := make(map[string]rolloutOverlapEvidence)
	byConversation := make(map[string][]domain.FileInventory)
	for _, file := range files {
		if file.Rollout == nil || file.ChangedDuringScan || !isBackupRolloutPath(file.RelativePath) {
			continue
		}
		byConversation[file.Rollout.ConversationID] = append(byConversation[file.Rollout.ConversationID], file)
	}
	for conversationID, backups := range byConversation {
		candidates := make([]domain.FileInventory, 0)
		for _, file := range files {
			if file.Rollout == nil || file.ChangedDuringScan || file.Rollout.ConversationID != conversationID || isBackupRolloutPath(file.RelativePath) {
				continue
			}
			candidates = append(candidates, file)
		}
		for _, backup := range backups {
			for _, candidate := range candidates {
				if candidate.SizeBytes <= backup.SizeBytes || candidate.SizeBytes == 0 {
					continue
				}
				if backup.SizeBytes > maxBackupPrefixProbeBytes {
					result[backup.RelativePath] = rolloutOverlapEvidence{Diagnostic: domain.Diagnostic{
						Code: "ROLLOUT_BACKUP_OVERLAP_UNVERIFIED", Severity: domain.SeverityWarning,
						Message:  "备份 rollout 可能是当前文件的旧快照，但前缀超过本次探测预算，统计未自动去重",
						Evidence: fmt.Sprintf("%s 与 %s；共同前缀探测上限=%d 字节", backup.RelativePath, candidate.RelativePath, maxBackupPrefixProbeBytes), ReadOnly: true,
					}}
					break
				}
				match, err := filesSharePrefix(root, backup, candidate)
				if err != nil {
					continue
				}
				if match {
					result[backup.RelativePath] = rolloutOverlapEvidence{Suppress: true, Diagnostic: domain.Diagnostic{
						Code: "ROLLOUT_BACKUP_PREFIX", Severity: domain.SeverityInfo,
						Message:  "备份 rollout 是当前 rollout 的完整前缀；物理路径保留，但共同历史只计一次",
						Evidence: fmt.Sprintf("%s 是 %s 的 %d 字节前缀", backup.RelativePath, candidate.RelativePath, backup.SizeBytes), ReadOnly: true,
					}}
					break
				}
			}
		}
	}
	return result
}

func isBackupRolloutPath(relative string) bool {
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		lower := strings.ToLower(strings.TrimSpace(part))
		if lower == "backup" || lower == "backups" || strings.HasPrefix(lower, "backup-") {
			return true
		}
	}
	return false
}

func filesSharePrefix(root string, shorter, longer domain.FileInventory) (bool, error) {
	shortPath, err := ResolveReadOnlyPath(root, shorter.RelativePath)
	if err != nil {
		return false, err
	}
	longPath, err := ResolveReadOnlyPath(root, longer.RelativePath)
	if err != nil {
		return false, err
	}
	shortInfo, err := os.Lstat(shortPath)
	if err != nil || shortInfo.Size() != shorter.SizeBytes {
		return false, err
	}
	longInfo, err := os.Lstat(longPath)
	if err != nil || longInfo.Size() != longer.SizeBytes {
		return false, err
	}
	shortFile, err := os.Open(shortPath)
	if err != nil {
		return false, err
	}
	defer shortFile.Close()
	longFile, err := os.Open(longPath)
	if err != nil {
		return false, err
	}
	defer longFile.Close()
	left := make([]byte, 128*1024)
	right := make([]byte, len(left))
	remaining := shortInfo.Size()
	for remaining > 0 {
		chunk := int64(len(left))
		if remaining < chunk {
			chunk = remaining
		}
		if _, err := io.ReadFull(shortFile, left[:chunk]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(longFile, right[:chunk]); err != nil {
			return false, err
		}
		for index := int64(0); index < chunk; index++ {
			if left[index] != right[index] {
				return false, nil
			}
		}
		remaining -= chunk
	}
	shortAfter, shortErr := os.Lstat(shortPath)
	longAfter, longErr := os.Lstat(longPath)
	if shortErr != nil || longErr != nil || !sameFileVersion(shortInfo, shortAfter) || !sameFileVersion(longInfo, longAfter) {
		return false, fmt.Errorf("备份前缀探测期间文件发生变化")
	}
	return true, nil
}

func sameFileVersion(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func pointerToGraph(graph domain.ConversationAssetGraph) *domain.ConversationAssetGraph {
	return &graph
}

func hasTable(summary domain.SQLiteSummary, name string) bool {
	for _, table := range summary.Tables {
		if table.Name == name {
			return true
		}
	}
	return false
}

func containsFile(files []domain.FileInventory, relative string) bool {
	for _, file := range files {
		if file.RelativePath == relative {
			return true
		}
	}
	return false
}

func normalizeRolloutPath(root, value string) (string, bool) {
	return compat.ResolveRolloutPath(root, value)
}

func appendUniqueRelationship(values []domain.SQLiteRelationship, value domain.SQLiteRelationship) []domain.SQLiteRelationship {
	for _, existing := range values {
		if existing.Table == value.Table && existing.Column == value.Column && existing.ThreadID == value.ThreadID {
			return values
		}
	}
	return append(values, value)
}

func appendDiagnostic(values []domain.Diagnostic, value domain.Diagnostic) []domain.Diagnostic {
	if len(values) >= 16 {
		return values
	}
	return append(values, value)
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func mergeTokenUsage(left, right domain.TokenUsage) domain.TokenUsage {
	if right.TotalTokens >= left.TotalTokens {
		return right
	}
	return left
}

func stricterStatus(left, right domain.AssetStatus) domain.AssetStatus {
	rank := map[domain.AssetStatus]int{
		domain.StatusStable: 0, domain.StatusUnsupported: 1, domain.StatusSQLiteWALPresent: 2,
		domain.StatusWriterLockObserved: 2, domain.StatusStaleLock: 2,
		domain.StatusPendingJSONLLine: 4, domain.StatusUnknown: 3, domain.StatusConflict: 4,
		domain.StatusActiveWriting: 5, domain.StatusGrowing: 5, domain.StatusOpenByProcess: 5,
		domain.StatusSourceChangedDuringProbe: 5, domain.StatusBlockedForMutation: 6,
	}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func assetSetHash(paths []string) string {
	hash := sha256.New()
	for _, path := range paths {
		_, _ = hash.Write([]byte(path))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func kindForPath(path string) domain.AssetKind {
	extension := strings.ToLower(filepath.Ext(path))
	switch extension {
	case ".jsonl":
		return compat.JSONLFileKind(path, domain.JSONLScanSummary{})
	case ".sqlite", ".db":
		return domain.AssetKindSQLite
	default:
		return domain.AssetKindUnknown
	}
}

func relativePath(path, root string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}
