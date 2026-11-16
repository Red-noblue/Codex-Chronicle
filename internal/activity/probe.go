// 中文说明：提供跨平台只读活动探测，比较文件窗口、进程占用、writer lock 和 SQLite busy 状态。
package activity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/sqliteguard"
	_ "modernc.org/sqlite"
)

var uuidPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// Target 是一次活动探测的物理资产目标。
type Target struct {
	RelativePath string           `json:"relative_path"`
	Kind         domain.AssetKind `json:"kind"`
}

// AssetResult 是单个目标的活动探测结果；它不包含 JSONL 或 SQLite 正文。
type AssetResult struct {
	RelativePath string             `json:"relative_path"`
	Kind         domain.AssetKind   `json:"kind"`
	Status       domain.AssetStatus `json:"status"`
	// ActivityStatus 是细粒度活动状态；Status 保留旧版粗粒度状态，兼容已有写入门禁和客户端。
	ActivityStatus  domain.AssetStatus      `json:"activity_status,omitempty"`
	MutationBlocked bool                    `json:"mutation_blocked"`
	Evidence        domain.ActivityEvidence `json:"evidence"`
	Error           string                  `json:"error,omitempty"`
}

// Report 是一次探测窗口的可审阅结果。
type Report struct {
	SchemaVersion         int             `json:"schema_version"`
	Root                  string          `json:"root"`
	GeneratedAt           time.Time       `json:"generated_at"`
	IntervalMS            int             `json:"interval_ms"`
	WindowCount           int             `json:"window_count"`
	WindowIntervalMS      int             `json:"window_interval_ms,omitempty"`
	Windows               []WindowSummary `json:"windows,omitempty"`
	ProcessProbeAvailable bool            `json:"process_probe_available"`
	ProcessProbeReason    string          `json:"process_probe_reason,omitempty"`
	LockProbeAvailable    bool            `json:"lock_probe_available"`
	LockProbeReason       string          `json:"lock_probe_reason,omitempty"`
	LockDirectory         string          `json:"lock_directory,omitempty"`
	LockFileCount         int             `json:"lock_file_count"`
	LockIDs               []string        `json:"lock_ids,omitempty"`
	Results               []AssetResult   `json:"results"`
}

// WindowSummary 是连续探测中的一个窗口统计，不包含正文内容。
type WindowSummary struct {
	Index        int       `json:"index"`
	ProbedAt     time.Time `json:"probed_at"`
	StableCount  int       `json:"stable_count"`
	ActiveCount  int       `json:"active_count"`
	BlockedCount int       `json:"blocked_count"`
	ChangedCount int       `json:"changed_count"`
}

// ProcessSnapshot 是进程探测器的统一输出。
type ProcessSnapshot struct {
	Available bool
	Reason    string
	Files     map[string][]domain.ProcessHandle
}

// ProcessDetector 允许平台实现或测试替换打开文件探测器。
type ProcessDetector interface {
	Detect(context.Context, string) ProcessSnapshot
}

// TargetedProcessDetector 是可选的高效进程探测接口；实现可以只检查本次目标文件。
// 未实现该接口的旧 adapter 仍通过 ProcessDetector.Detect(root) 工作。
type TargetedProcessDetector interface {
	DetectTargets(context.Context, string, []string) ProcessSnapshot
}

// Options 控制探测窗口和安全策略。探测器始终只调用只读系统接口。
type Options struct {
	Interval            time.Duration
	HashBytes           int64
	LockDirectory       string
	DisableProcessProbe bool
	Conservative        bool
	ProcessDetector     ProcessDetector
	windowIndex         int
	windowCount         int
}

// DefaultOptions 返回适合 UI 手动刷新的一次短窗口配置。
func DefaultOptions() Options {
	return Options{Interval: 250 * time.Millisecond, HashBytes: 64 << 10, Conservative: true}
}

// Probe 对 targets 做一次短时间窗口采样。路径必须相对于 root，且不能经过符号链接。
func Probe(ctx context.Context, root string, targets []Target, options Options) (Report, error) {
	return ProbeWindows(ctx, root, targets, options, 1)
}

// ProbeWindows 连续执行多个只读探测窗口，并在每个资产证据中保留有限历史。
// 窗口数量用于区分短暂采样和持续活动；它不会创建锁、临时源文件或修改源目录。
func ProbeWindows(ctx context.Context, root string, targets []Target, options Options, windows int) (Report, error) {
	if windows <= 0 {
		windows = 1
	}
	if windows > 16 {
		return Report{}, fmt.Errorf("连续探测窗口数量不能超过 16")
	}
	options = normalizeOptions(options)
	merged := Report{}
	for index := 1; index <= windows; index++ {
		windowOptions := options
		windowOptions.windowIndex = index
		windowOptions.windowCount = windows
		report, err := probeOnce(ctx, root, targets, windowOptions)
		if err != nil {
			return merged, err
		}
		if index == 1 {
			merged = report
			merged.WindowCount = windows
			merged.WindowIntervalMS = report.IntervalMS
			merged.Windows = []WindowSummary{summarizeWindow(report, 1)}
			continue
		}
		merged.Windows = append(merged.Windows, summarizeWindow(report, index))
		mergeProbeWindow(&merged, report, index, windows)
	}
	if merged.WindowCount == 0 {
		merged.WindowCount = 1
	}
	return merged, nil
}

func probeOnce(ctx context.Context, root string, targets []Target, options Options) (Report, error) {
	absRoot, err := canonicalRoot(root)
	if err != nil {
		return Report{}, err
	}
	options = normalizeOptions(options)
	started := time.Now().UTC()
	report := Report{
		SchemaVersion: 1,
		Root:          absRoot,
		GeneratedAt:   started,
		IntervalMS:    int(options.Interval / time.Millisecond),
		Results:       make([]AssetResult, 0, len(targets)),
	}

	before := make(map[string]fileState, len(targets))
	resolved := make(map[string]string, len(targets))
	for _, target := range targets {
		if _, exists := before[target.RelativePath]; exists {
			continue
		}
		path, resolveErr := resolvePath(absRoot, target.RelativePath)
		if resolveErr != nil {
			before[target.RelativePath] = fileState{err: resolveErr}
			continue
		}
		resolved[target.RelativePath] = path
		before[target.RelativePath] = statFile(path, options.HashBytes)
	}

	if err := wait(ctx, options.Interval); err != nil {
		return report, err
	}

	processSnapshot := ProcessSnapshot{Files: map[string][]domain.ProcessHandle{}, Reason: "进程探测已禁用"}
	if !options.DisableProcessProbe {
		paths := make([]string, 0, len(resolved))
		for _, path := range resolved {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		if targeted, ok := options.ProcessDetector.(TargetedProcessDetector); ok {
			processSnapshot = targeted.DetectTargets(ctx, absRoot, paths)
		} else {
			processSnapshot = options.ProcessDetector.Detect(ctx, absRoot)
		}
	}
	report.ProcessProbeAvailable = processSnapshot.Available
	report.ProcessProbeReason = processSnapshot.Reason

	locks, lockErr := readLocks(absRoot, options.LockDirectory)
	report.LockProbeAvailable = lockErr == nil
	if lockErr != nil {
		report.LockProbeReason = lockErr.Error()
	}
	if lockErr == nil {
		report.LockDirectory = lockDirectory(absRoot, options.LockDirectory)
		report.LockFileCount = len(locks)
		report.LockIDs = make([]string, 0, len(locks))
		for id := range locks {
			report.LockIDs = append(report.LockIDs, id)
		}
		sort.Strings(report.LockIDs)
	}

	after := make(map[string]fileState, len(targets))
	for _, target := range targets {
		path := resolved[target.RelativePath]
		if path == "" {
			after[target.RelativePath] = fileState{err: before[target.RelativePath].err}
			continue
		}
		after[target.RelativePath] = statFile(path, options.HashBytes)
	}

	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if _, exists := seen[target.RelativePath]; exists {
			continue
		}
		seen[target.RelativePath] = struct{}{}
		lockReason := ""
		if lockErr != nil {
			lockReason = lockErr.Error()
		}
		result := evaluate(target, before[target.RelativePath], after[target.RelativePath], processSnapshot, locks, lockErr == nil, lockReason, absRoot, started, report.IntervalMS, options)
		if target.Kind == domain.AssetKindSQLite && result.Error == "" && result.Status != domain.StatusConflict && result.Status != domain.StatusUnknown {
			path := resolved[target.RelativePath]
			busy, reason := probeSQLiteBusy(ctx, path)
			result.Evidence.SQLiteBusy = busy
			result.Evidence.SQLiteProbeReason = reason
			if busy {
				result.Status = domain.StatusActiveWriting
				result.ActivityStatus = domain.StatusBlockedForMutation
				result.MutationBlocked = true
				result.Evidence.ObservedStates = append(result.Evidence.ObservedStates, domain.StatusBlockedForMutation)
				result.Evidence.MutationBlocked = true
				result.Evidence.LifecycleBlocked = true
				result.Evidence.Reason = "SQLite 当前返回 busy/locked，暂缓任何生命周期写入"
			}
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func summarizeWindow(report Report, index int) WindowSummary {
	summary := WindowSummary{Index: index, ProbedAt: report.GeneratedAt}
	for _, result := range report.Results {
		if result.Status == domain.StatusStable {
			summary.StableCount++
		} else {
			summary.ActiveCount++
		}
		if result.MutationBlocked {
			summary.BlockedCount++
		}
		if result.Evidence.ChangedDuringProbe || result.Evidence.SourceChangedDuringProbe {
			summary.ChangedCount++
		}
	}
	return summary
}

func mergeProbeWindow(merged *Report, window Report, index, total int) {
	byPath := make(map[string]AssetResult, len(window.Results))
	for _, result := range window.Results {
		byPath[result.RelativePath] = result
	}
	for resultIndex := range merged.Results {
		current := &merged.Results[resultIndex]
		next, ok := byPath[current.RelativePath]
		if !ok {
			continue
		}
		history := append([]domain.ActivityObservation(nil), current.Evidence.History...)
		if len(history) == 0 {
			history = appendActivityHistory(history, activityObservation(current))
		}
		history = appendActivityHistory(history, activityObservation(&next))
		current.ActivityStatus = strongerActivityStatus(current.ActivityStatus, next.ActivityStatus)
		current.Status = strongerCoarseStatus(current.Status, next.Status)
		current.MutationBlocked = current.MutationBlocked || next.MutationBlocked
		nextEvidence := next.Evidence
		nextEvidence.History = history
		nextEvidence.WindowCount = total
		nextEvidence.WindowIndex = index
		nextEvidence.MutationBlocked = current.MutationBlocked
		nextEvidence.LifecycleBlocked = current.MutationBlocked
		current.Evidence = nextEvidence
	}
}

func activityObservation(result *AssetResult) domain.ActivityObservation {
	if result == nil {
		return domain.ActivityObservation{}
	}
	return domain.ActivityObservation{
		ProbedAt: result.Evidence.ProbedAt, Status: result.ActivityStatus,
		ObservedStates:     append([]domain.AssetStatus(nil), result.Evidence.ObservedStates...),
		Changed:            result.Evidence.ChangedDuringProbe || result.Evidence.SourceChangedDuringProbe,
		SourceHashCompared: result.Evidence.SourceHashCompared,
		Reason:             result.Evidence.Reason,
	}
}

func appendActivityHistory(history []domain.ActivityObservation, observation domain.ActivityObservation) []domain.ActivityObservation {
	if observation.ProbedAt.IsZero() {
		return history
	}
	if len(history) >= 8 {
		history = history[len(history)-7:]
	}
	return append(history, observation)
}

func strongerCoarseStatus(left, right domain.AssetStatus) domain.AssetStatus {
	rank := map[domain.AssetStatus]int{
		domain.StatusStable: 0, domain.StatusSQLiteWALPresent: 1, domain.StatusWriterLockObserved: 2,
		domain.StatusStaleLock: 2, domain.StatusPendingJSONLLine: 4,
		domain.StatusUnknown: 3, domain.StatusConflict: 4, domain.StatusActiveWriting: 5,
		domain.StatusGrowing: 5, domain.StatusOpenByProcess: 5, domain.StatusSourceChangedDuringProbe: 5,
		domain.StatusBlockedForMutation: 6,
	}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func strongerActivityStatus(left, right domain.AssetStatus) domain.AssetStatus {
	rank := map[domain.AssetStatus]int{
		domain.StatusStable: 0, domain.StatusSQLiteWALPresent: 1, domain.StatusWriterLockObserved: 2,
		domain.StatusStaleLock: 3, domain.StatusPendingJSONLLine: 4, domain.StatusGrowing: 5,
		domain.StatusOpenByProcess: 6, domain.StatusSourceChangedDuringProbe: 7,
		domain.StatusBlockedForMutation: 8, domain.StatusUnknown: 9, domain.StatusConflict: 10,
	}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

type fileState struct {
	info          os.FileInfo
	size          int64
	modTime       time.Time
	mode          os.FileMode
	identity      string
	hash          string
	hashAlgorithm string
	err           error
}

func evaluate(target Target, before, after fileState, processes ProcessSnapshot, locks map[string]lockInfo, lockAvailable bool, lockReason, root string, started time.Time, intervalMS int, options Options) AssetResult {
	evidence := domain.ActivityEvidence{
		ProbedAt:              started,
		IntervalMS:            intervalMS,
		WindowIndex:           options.windowIndex,
		WindowCount:           options.windowCount,
		BeforeSizeBytes:       before.size,
		AfterSizeBytes:        after.size,
		BeforeModTime:         before.modTime.UTC(),
		AfterModTime:          after.modTime.UTC(),
		SourceIdentityBefore:  before.identity,
		SourceIdentityAfter:   after.identity,
		SourceHashBefore:      before.hash,
		SourceHashAfter:       after.hash,
		SourceHashAlgorithm:   firstNonEmpty(before.hashAlgorithm, after.hashAlgorithm),
		SourceHashCompared:    before.hash != "" && after.hash != "" && before.hashAlgorithm == after.hashAlgorithm,
		ProcessProbeAvailable: processes.Available,
		ProcessProbeReason:    processes.Reason,
		LockProbeAvailable:    lockAvailable,
		LockProbeReason:       lockReason,
		OpenByProcess:         append([]domain.ProcessHandle(nil), processes.Files[absolutePath(root, target.RelativePath)]...),
		Readable:              true,
	}
	result := AssetResult{RelativePath: target.RelativePath, Kind: target.Kind, Status: domain.StatusStable, ActivityStatus: domain.StatusStable, Evidence: evidence}
	finish := func() AssetResult {
		if result.Status != domain.StatusStable {
			result.MutationBlocked = true
		}
		if result.MutationBlocked {
			evidence.ObservedStates = append(evidence.ObservedStates, domain.StatusBlockedForMutation)
		}
		if result.MutationBlocked {
			evidence.MutationBlocked = true
			evidence.LifecycleBlocked = true
		}
		if result.ActivityStatus == "" {
			result.ActivityStatus = result.Status
		}
		evidence.ObservedStates = uniqueStatuses(evidence.ObservedStates)
		result.Evidence = evidence
		result.Evidence.History = appendActivityHistory(result.Evidence.History, activityObservation(&result))
		return result
	}
	if before.err != nil || after.err != nil {
		result.Status = domain.StatusUnknown
		result.ActivityStatus = domain.StatusUnknown
		evidence.Readable = false
		if after.err != nil && before.err == nil && os.IsNotExist(after.err) {
			result.Status = domain.StatusConflict
			result.ActivityStatus = domain.StatusConflict
			evidence.Reason = "目标文件在探测窗口内消失，不能复用旧快照"
		} else if before.err != nil && after.err == nil && os.IsNotExist(before.err) {
			result.Status = domain.StatusConflict
			result.ActivityStatus = domain.StatusConflict
			evidence.Reason = "目标文件在探测窗口前不存在，不能建立稳定快照"
		} else {
			evidence.Reason = "探测窗口内无法稳定读取目标文件"
		}
		if after.err != nil {
			result.Error = after.err.Error()
		} else {
			result.Error = before.err.Error()
		}
		return finish()
	}
	if before.mode&os.ModeSymlink != 0 || after.mode&os.ModeSymlink != 0 || !after.mode.IsRegular() {
		result.Status = domain.StatusUnknown
		result.ActivityStatus = domain.StatusUnknown
		evidence.Readable = false
		result.Error = "目标文件类型在探测期间不安全"
		evidence.Reason = "路径包含符号链接或不再是普通文件"
		return finish()
	}
	changed := before.size != after.size || !before.modTime.Equal(after.modTime)
	if before.hash != "" && after.hash != "" && before.hashAlgorithm == after.hashAlgorithm && before.hash != after.hash {
		changed = true
	}
	evidence.ChangedDuringProbe = changed
	evidence.SourceChangedDuringProbe = changed || (before.identity != "" && after.identity != "" && before.identity != after.identity) || !os.SameFile(before.info, after.info)
	if changed {
		result.Status = domain.StatusActiveWriting
		result.ActivityStatus = domain.StatusSourceChangedDuringProbe
		evidence.ObservedStates = append(evidence.ObservedStates, domain.StatusSourceChangedDuringProbe)
		evidence.Reason = "文件大小、修改时间或内容指纹在探测窗口内发生变化"
		if after.size > before.size {
			result.ActivityStatus = domain.StatusGrowing
			evidence.ObservedStates = append(evidence.ObservedStates, domain.StatusGrowing)
		}
	}
	if target.Kind == domain.AssetKindJSONL || target.Kind == domain.AssetKindSessionIndex {
		pending, pendingErr := pendingJSONLLine(absolutePath(root, target.RelativePath), after.size)
		if pendingErr == nil && pending {
			evidence.JSONLPendingLine = true
			evidence.ObservedStates = append(evidence.ObservedStates, domain.StatusPendingJSONLLine)
			if result.Status == domain.StatusStable {
				result.Status = domain.StatusPendingJSONLLine
				result.ActivityStatus = domain.StatusPendingJSONLLine
				evidence.Reason = "JSONL 文件末尾存在尚未完成的记录"
			}
		}
	}
	if len(evidence.OpenByProcess) > 0 {
		result.Status = domain.StatusActiveWriting
		result.ActivityStatus = domain.StatusOpenByProcess
		evidence.ObservedStates = append(evidence.ObservedStates, domain.StatusOpenByProcess)
		evidence.Reason = "目标文件仍被进程打开，暂缓生命周期写入"
	}
	ids := idsInPath(target.RelativePath)
	for id, info := range locks {
		if _, ok := ids[id]; !ok {
			continue
		}
		evidence.MatchingLockIDs = append(evidence.MatchingLockIDs, id)
		age := int64(time.Since(info.modTime).Seconds())
		if age >= 0 && (evidence.LockAgeSeconds == 0 || age > evidence.LockAgeSeconds) {
			evidence.LockAgeSeconds = age
		}
	}
	sort.Strings(evidence.MatchingLockIDs)
	if len(evidence.MatchingLockIDs) > 0 {
		evidence.ObservedStates = append(evidence.ObservedStates, domain.StatusWriterLockObserved)
		if result.Status == domain.StatusStable {
			result.Status = domain.StatusStaleLock
			result.ActivityStatus = domain.StatusWriterLockObserved
			evidence.Reason = "存在匹配 writer lock，但当前窗口没有文件变化或打开进程证据"
		}
	}
	if target.Kind == domain.AssetKindSQLite {
		for _, auxiliary := range sqliteguard.ExistingAuxiliaryFiles(absolutePath(root, target.RelativePath)) {
			evidence.SQLiteAuxiliary = append(evidence.SQLiteAuxiliary, filepath.Base(auxiliary))
			switch {
			case strings.HasSuffix(strings.ToLower(auxiliary), "-wal"):
				evidence.SQLiteWALPresent = true
			case strings.HasSuffix(strings.ToLower(auxiliary), "-shm"):
				evidence.SQLiteSHMPresent = true
			}
		}
		if evidence.SQLiteWALPresent || evidence.SQLiteSHMPresent {
			evidence.ObservedStates = append(evidence.ObservedStates, domain.StatusSQLiteWALPresent)
			result.MutationBlocked = true
			if result.Status == domain.StatusStable {
				result.Status = domain.StatusSQLiteWALPresent
				result.ActivityStatus = domain.StatusSQLiteWALPresent
			}
		}
	}
	if !processes.Available && options.Conservative && result.Status == domain.StatusStable {
		result.Status = domain.StatusUnknown
		result.ActivityStatus = domain.StatusUnknown
		evidence.Reason = "当前平台无法确认打开文件进程，不能把稳定采样当作可写安全"
	}
	if !lockAvailable && options.Conservative && result.Status == domain.StatusStable {
		result.Status = domain.StatusUnknown
		result.ActivityStatus = domain.StatusUnknown
		evidence.Reason = "writer lock 目录无法读取，不能把稳定采样当作可写安全"
	}
	if evidence.SourceChangedDuringProbe {
		result.MutationBlocked = true
		evidence.ObservedStates = append(evidence.ObservedStates, domain.StatusBlockedForMutation)
	}
	return finish()
}

func uniqueStatuses(values []domain.AssetStatus) []domain.AssetStatus {
	seen := make(map[domain.AssetStatus]struct{}, len(values))
	result := make([]domain.AssetStatus, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

type lockInfo struct {
	modTime time.Time
}

func readLocks(root, configured string) (map[string]lockInfo, error) {
	directory := lockDirectory(root, configured)
	directoryInfo, statErr := os.Lstat(directory)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return map[string]lockInfo{}, nil
		}
		return nil, statErr
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return nil, fmt.Errorf("writer lock 目录不是普通目录: %s", directory)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	locks := make(map[string]lockInfo)
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".lock") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		for _, id := range uuidPattern.FindAllString(entry.Name(), -1) {
			locks[strings.ToLower(id)] = lockInfo{modTime: info.ModTime()}
		}
	}
	return locks, nil
}

func idsInPath(path string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, value := range uuidPattern.FindAllString(filepath.Base(path), -1) {
		ids[strings.ToLower(value)] = struct{}{}
	}
	return ids
}

func probeSQLiteBusy(ctx context.Context, path string) (bool, string) {
	if strings.TrimSpace(path) == "" {
		return false, "SQLite 路径为空"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err.Error()
	}
	dsn, auxiliary, err := sqliteguard.ReadOnlyDSN(abs, 0)
	if err != nil {
		return false, err.Error()
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false, err.Error()
	}
	defer database.Close()
	probeCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	var version int
	err = database.QueryRowContext(probeCtx, "PRAGMA schema_version").Scan(&version)
	if err == nil {
		if len(auxiliary) > 0 {
			return false, "SQLite immutable 只读探测成功；检测到 WAL/SHM，可能未包含 WAL 中尚未合并的最新提交"
		}
		return false, "SQLite schema_version 只读探测成功"
	}
	if isBusyError(err) {
		return true, err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true, "SQLite 只读探测超时，按 busy/locked 风险处理"
	}
	return false, err.Error()
}

func isBusyError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") || strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked")
}

func statFile(path string, hashBytes int64) fileState {
	if strings.TrimSpace(path) == "" {
		return fileState{err: fmt.Errorf("目标文件路径为空")}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fileState{err: err}
	}
	state := fileState{info: info, size: info.Size(), modTime: info.ModTime(), mode: info.Mode(), identity: fileIdentity(info)}
	if info.Mode().IsRegular() && hashBytes > 0 {
		state.hash, state.hashAlgorithm, state.err = hashFileSample(path, info.Size(), hashBytes)
	}
	return state
}

func hashFileSample(path string, size, maxBytes int64) (string, string, error) {
	if maxBytes <= 0 {
		return "", "", nil
	}
	if maxBytes > 8<<20 {
		maxBytes = 8 << 20
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	hash := sha256.New()
	if size <= maxBytes {
		if _, err := io.CopyN(hash, file, size); err != nil && !(errors.Is(err, io.EOF) && size == 0) {
			return "", "", err
		}
		return hex.EncodeToString(hash.Sum(nil)), "sha256-full", nil
	}
	half := maxBytes / 2
	if half == 0 {
		half = 1
	}
	prefix := make([]byte, half)
	if _, err := io.ReadFull(file, prefix); err != nil {
		return "", "", err
	}
	if _, err := hash.Write(prefix); err != nil {
		return "", "", err
	}
	if _, err := hash.Write([]byte("\x00codex-chronicle-sample\x00")); err != nil {
		return "", "", err
	}
	if _, err := file.Seek(-half, io.SeekEnd); err != nil {
		return "", "", err
	}
	suffix := make([]byte, half)
	if _, err := io.ReadFull(file, suffix); err != nil {
		return "", "", err
	}
	if _, err := hash.Write(suffix); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), "sha256-prefix-suffix", nil
}

func pendingJSONLLine(path string, size int64) (bool, error) {
	if size <= 0 {
		return false, nil
	}
	const tailBytes int64 = 1 << 20
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	start := size - tailBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, size-start))
	if err != nil {
		return false, err
	}
	if len(data) == 0 || data[len(data)-1] == '\n' || data[len(data)-1] == '\r' {
		return false, nil
	}
	line := data
	if index := bytes.LastIndexByte(data, '\n'); index >= 0 {
		line = data[index+1:]
	}
	line = bytes.TrimSuffix(line, []byte{'\r'})
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return false, nil
	}
	if json.Valid(line) {
		return false, nil
	}
	var value any
	decodeErr := json.Unmarshal(line, &value)
	return decodeErr != nil && strings.Contains(strings.ToLower(decodeErr.Error()), "unexpected end"), nil
}

func fileIdentity(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return fmt.Sprintf("%d:%d:%o", info.ModTime().UnixNano(), info.Size(), info.Mode())
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("解析活动探测根失败: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("读取活动探测根失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("活动探测根必须是普通目录: %s", abs)
	}
	return filepath.Clean(abs), nil
}

func resolvePath(root, relative string) (string, error) {
	if strings.TrimSpace(relative) == "" {
		return "", fmt.Errorf("目标相对路径为空")
	}
	candidateRelative := filepath.FromSlash(relative)
	if filepath.IsAbs(candidateRelative) {
		return "", fmt.Errorf("目标路径必须是相对路径")
	}
	clean := filepath.Clean(candidateRelative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("目标路径越出活动探测根")
	}
	candidate := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		candidate = filepath.Join(candidate, part)
		info, err := os.Lstat(candidate)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("目标路径包含符号链接")
		}
	}
	return candidate, nil
}

func absolutePath(root, relative string) string {
	path, err := resolvePath(root, relative)
	if err != nil {
		return filepath.Join(root, filepath.FromSlash(relative))
	}
	return path
}

func lockDirectory(root, configured string) string {
	if strings.TrimSpace(configured) != "" {
		if filepath.IsAbs(configured) {
			return filepath.Clean(configured)
		}
		return filepath.Join(root, filepath.FromSlash(configured))
	}
	return filepath.Join(root, "thread-writer-locks")
}

func normalizeOptions(options Options) Options {
	if options.HashBytes == 0 {
		options.HashBytes = DefaultOptions().HashBytes
	}
	if options.HashBytes < 0 {
		options.HashBytes = 0
	}
	if options.HashBytes > 8<<20 {
		options.HashBytes = 8 << 20
	}
	if options.Interval <= 0 {
		options.Interval = DefaultOptions().Interval
	}
	if options.Interval < time.Millisecond {
		options.Interval = time.Millisecond
	}
	if options.Interval > 5*time.Second {
		options.Interval = 5 * time.Second
	}
	if options.ProcessDetector == nil && !options.DisableProcessProbe {
		options.ProcessDetector = systemProcessDetector{}
	}
	if options.windowIndex <= 0 {
		options.windowIndex = 1
	}
	if options.windowCount <= 0 {
		options.windowCount = 1
	}
	return options
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type systemProcessDetector struct{}

func (systemProcessDetector) Detect(ctx context.Context, root string) ProcessSnapshot {
	return systemProcessDetector{}.DetectTargets(ctx, root, nil)
}

func (systemProcessDetector) DetectTargets(ctx context.Context, root string, paths []string) ProcessSnapshot {
	if runtime.GOOS == "windows" {
		return ProcessSnapshot{Reason: "Windows 默认没有可安全调用的打开文件路径探测器"}
	}
	command, err := exec.LookPath("lsof")
	if err != nil {
		return ProcessSnapshot{Reason: "未找到 lsof，无法确认打开文件进程"}
	}
	arguments := []string{"-n", "-P", "-Fpcn"}
	if len(paths) > 0 && len(paths) <= 256 && commandLength(paths) <= 100000 {
		arguments = append(arguments, paths...)
	} else {
		arguments = append(arguments, "+D", root)
	}
	completed := exec.CommandContext(ctx, command, arguments...)
	output, err := completed.Output()
	snapshot := ProcessSnapshot{Available: true, Files: make(map[string][]domain.ProcessHandle)}
	if err != nil {
		if ctx.Err() != nil {
			return ProcessSnapshot{Reason: "lsof 探测超时或被取消: " + ctx.Err().Error()}
		}
		snapshot.Reason = "lsof 返回非零状态，已使用其可解析输出"
	}
	parseLsofOutput(root, output, snapshot.Files)
	if snapshot.Reason == "" {
		if len(paths) > 0 && len(paths) <= 256 && commandLength(paths) <= 100000 {
			snapshot.Reason = "lsof 按目标文件只读探测成功"
		} else {
			snapshot.Reason = "lsof 按扫描根只读探测成功"
		}
	}
	return snapshot
}

func commandLength(paths []string) int {
	length := 0
	for _, path := range paths {
		length += len(path) + 1
	}
	return length
}

func parseLsofOutput(root string, output []byte, files map[string][]domain.ProcessHandle) {
	pid := 0
	command := ""
	for _, line := range strings.Split(string(output), "\n") {
		if len(line) < 2 {
			continue
		}
		value := line[1:]
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(value)
		case 'c':
			command = value
		case 'n':
			path := strings.TrimSuffix(value, " (deleted)")
			if !within(root, path) {
				continue
			}
			absolute, err := filepath.Abs(path)
			if err != nil || pid <= 0 {
				continue
			}
			files[absolute] = appendUniqueProcess(files[absolute], domain.ProcessHandle{PID: pid, Command: command})
		}
	}
}

func appendUniqueProcess(values []domain.ProcessHandle, value domain.ProcessHandle) []domain.ProcessHandle {
	for _, existing := range values {
		if existing.PID == value.PID && existing.Command == value.Command {
			return values
		}
	}
	return append(values, value)
}

func within(root, path string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absRoot, absPath)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
