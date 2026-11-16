// 中文说明：管理后台只读 inventory 任务、取消和进度快照，供 REST/SSE 工作台使用。
package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"codex-chronicle.local/internal/domain"
)

// TaskSnapshot 是不会暴露正文的后台扫描状态。
type TaskSnapshot struct {
	ID          string                  `json:"id"`
	Kind        string                  `json:"kind"`
	Root        string                  `json:"root"`
	Status      domain.TaskStatus       `json:"status"`
	CreatedAt   time.Time               `json:"created_at"`
	StartedAt   time.Time               `json:"started_at,omitempty"`
	CompletedAt time.Time               `json:"completed_at,omitempty"`
	FilesSeen   int                     `json:"files_seen"`
	BytesSeen   int64                   `json:"bytes_seen"`
	CacheHits   int                     `json:"cache_hits"`
	FilesParsed int                     `json:"files_parsed"`
	Error       string                  `json:"error,omitempty"`
	Report      *domain.InventoryReport `json:"report,omitempty"`
	// Reused 表示本次启动请求复用了同一根目录的已有运行任务。
	Reused bool `json:"reused,omitempty"`
}

type taskState struct {
	snapshot TaskSnapshot
	cancel   context.CancelFunc
	done     chan struct{}
	root     string
	options  ScanOptions
}

// TaskManager 管理当前 daemon 进程内的后台扫描任务。
type TaskManager struct {
	scanner      Scanner
	mu           sync.RWMutex
	tasks        map[string]*taskState
	activeByRoot map[string]string
	latestByRoot map[string]latestInventory
	sequence     uint64
}

type latestInventory struct {
	report       domain.InventoryReport
	taskID       string
	stale        bool
	checkError   string
	rootModTime  time.Time
	checkedAt    time.Time
	successfulAt time.Time
}

// ActiveCount 供安全退出检查，包含尚未开始和仍在扫描的任务。
func (m *TaskManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, task := range m.tasks {
		if task.snapshot.Status == domain.TaskQueued || task.snapshot.Status == domain.TaskRunning {
			count++
		}
	}
	return count
}

const freshnessCheckInterval = time.Second

// SnapshotMetadata 描述当前工作区快照的新鲜度和刷新状态，不包含正文。
type SnapshotMetadata struct {
	Status               string    `json:"snapshot_status"`
	Stale                bool      `json:"stale"`
	ScanTaskID           string    `json:"scan_task_id,omitempty"`
	LastSuccessfulScanAt time.Time `json:"last_successful_scan_at,omitempty"`
	RefreshRequested     bool      `json:"refresh_requested"`
	FreshnessCheckError  string    `json:"freshness_check_error,omitempty"`
}

// InventorySnapshot 是报告和其控制面元数据的只读组合。
type InventorySnapshot struct {
	Report   domain.InventoryReport
	Metadata SnapshotMetadata
}

// NewTaskManager 创建任务管理器。
func NewTaskManager(scanner Scanner) *TaskManager {
	return &TaskManager{scanner: scanner, tasks: make(map[string]*taskState), activeByRoot: make(map[string]string), latestByRoot: make(map[string]latestInventory)}
}

// InvalidateInventory 在源操作完成后失效本项目快照，阻止进行中的旧扫描覆盖新状态；不修改源文件。
func (m *TaskManager) InvalidateInventory(root string) {
	if m == nil {
		return
	}
	root = canonicalScanRoot(root)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.latestByRoot, root)
	if active := m.tasks[m.activeByRoot[root]]; active != nil {
		active.cancel()
	}
	delete(m.activeByRoot, root)
}

// StartInventory 在后台启动只读扫描，立即返回 queued/running 快照。
func (m *TaskManager) StartInventory(root string) TaskSnapshot {
	return m.StartInventoryWithOptions(root, ScanOptions{IncludeAssetGraphs: true})
}

// StartInventoryWithOptions 在后台启动只读扫描；同一工作区同一时刻只保留一个运行任务。
// 如果已有任务正在扫描该根目录，调用方会复用它，避免多个 HTTP 请求重复遍历大型 .codex。
func (m *TaskManager) StartInventoryWithOptions(root string, options ScanOptions) TaskSnapshot {
	if m == nil {
		return TaskSnapshot{Status: domain.TaskFailed, Error: "任务管理器未初始化"}
	}
	canonicalRoot := canonicalScanRoot(root)
	m.mu.Lock()
	if activeID := m.activeByRoot[canonicalRoot]; activeID != "" {
		if active, ok := m.tasks[activeID]; ok && (active.snapshot.Status == domain.TaskQueued || active.snapshot.Status == domain.TaskRunning) {
			reused := cloneTaskSnapshot(active.snapshot)
			reused.Reused = true
			m.mu.Unlock()
			return reused
		}
		delete(m.activeByRoot, canonicalRoot)
	}
	sequence := atomic.AddUint64(&m.sequence, 1)
	id := fmt.Sprintf("scan-%d-%d", time.Now().UTC().UnixNano(), sequence)
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	state := &taskState{
		snapshot: TaskSnapshot{ID: id, Kind: "inventory", Root: canonicalRoot, Status: domain.TaskQueued, CreatedAt: now},
		cancel:   cancel, done: make(chan struct{}), root: canonicalRoot, options: options,
	}
	initial := state.snapshot
	m.tasks[id] = state
	m.activeByRoot[canonicalRoot] = id
	m.mu.Unlock()
	go m.runInventory(ctx, state)
	return initial
}

// Get 返回任务快照；不存在时返回 false。
func (m *TaskManager) Get(id string) (TaskSnapshot, bool) {
	m.mu.RLock()
	state, ok := m.tasks[id]
	if !ok {
		m.mu.RUnlock()
		return TaskSnapshot{}, false
	}
	snapshot := cloneTaskSnapshot(state.snapshot)
	m.mu.RUnlock()
	return snapshot, true
}

// Cancel 请求取消任务；扫描器在下一个文件边界响应取消。
func (m *TaskManager) Cancel(id string) bool {
	m.mu.RLock()
	state, ok := m.tasks[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	state.cancel()
	return true
}

// Wait 等待任务进入终态；请求取消时只取消等待，不会取消后台扫描任务。
func (m *TaskManager) Wait(ctx context.Context, id string) (TaskSnapshot, error) {
	if m == nil {
		return TaskSnapshot{}, errors.New("任务管理器未初始化")
	}
	m.mu.RLock()
	state, ok := m.tasks[id]
	m.mu.RUnlock()
	if !ok {
		return TaskSnapshot{}, fmt.Errorf("任务不存在: %s", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-state.done:
		snapshot, ok := m.Get(id)
		if !ok {
			return TaskSnapshot{}, fmt.Errorf("任务不存在: %s", id)
		}
		return snapshot, nil
	case <-ctx.Done():
		return TaskSnapshot{}, ctx.Err()
	}
}

// ReadReport 返回可复用的最新只读报告；首次读取或需要更完整投影时只启动一个后台扫描。
// checkFreshness 为 true 时会在返回前对源文件做元数据校验，并在发现变化时标记 stale。
func (m *TaskManager) ReadReport(ctx context.Context, root string, options ScanOptions, checkFreshness bool) (InventorySnapshot, error) {
	if m == nil {
		return InventorySnapshot{}, errors.New("任务管理器未初始化")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	canonicalRoot := canonicalScanRoot(root)
	for {
		snapshot, found, err := m.Snapshot(ctx, canonicalRoot, checkFreshness)
		if err != nil {
			return InventorySnapshot{}, err
		}
		if found && reportSupports(snapshot.Report, options) {
			return snapshot, nil
		}
		activeID := m.activeTaskID(canonicalRoot)
		if activeID == "" {
			task := m.StartInventoryWithOptions(canonicalRoot, options)
			if task.Status == domain.TaskFailed {
				return InventorySnapshot{}, errors.New(task.Error)
			}
			activeID = task.ID
		}
		task, waitErr := m.Wait(ctx, activeID)
		if waitErr != nil {
			return InventorySnapshot{}, waitErr
		}
		if task.Status != domain.TaskCompleted {
			if task.Error != "" {
				return InventorySnapshot{}, errors.New(task.Error)
			}
			return InventorySnapshot{}, fmt.Errorf("inventory 扫描未完成: %s", task.Status)
		}
	}
}

// Snapshot 返回指定工作区的最新成功快照；没有快照时 found 为 false。
func (m *TaskManager) Snapshot(ctx context.Context, root string, checkFreshness bool) (InventorySnapshot, bool, error) {
	if m == nil {
		return InventorySnapshot{}, false, errors.New("任务管理器未初始化")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	canonicalRoot := canonicalScanRoot(root)
	m.mu.RLock()
	latest, found := m.latestByRoot[canonicalRoot]
	activeID := m.activeByRoot[canonicalRoot]
	m.mu.RUnlock()
	if !found {
		return InventorySnapshot{Metadata: SnapshotMetadata{Status: "missing", RefreshRequested: activeID != "", ScanTaskID: activeID}}, false, nil
	}
	if checkFreshness {
		rootInfo, statErr := os.Stat(canonicalRoot)
		if statErr != nil {
			latest.stale = true
			latest.checkError = statErr.Error()
		} else if !latest.rootModTime.IsZero() && !latest.rootModTime.Equal(rootInfo.ModTime()) {
			// 根目录时间变化可以廉价发现新增、删除和目录项变化；正文校验会在下一次低频检查中完成。
			latest.stale = true
			latest.checkError = ""
		} else if latest.checkedAt.IsZero() || time.Since(latest.checkedAt) >= freshnessCheckInterval {
			stale, checkErr := m.scanner.CheckFreshness(ctx, canonicalRoot, latest.report)
			if checkErr != nil && errors.Is(checkErr, context.Canceled) {
				return InventorySnapshot{}, false, checkErr
			}
			latest.stale = stale
			if checkErr != nil {
				latest.checkError = checkErr.Error()
			} else {
				latest.checkError = ""
			}
		}
		latest.checkedAt = time.Now().UTC()
		if rootInfo != nil {
			latest.rootModTime = rootInfo.ModTime()
		}
		m.mu.Lock()
		if current, ok := m.latestByRoot[canonicalRoot]; ok && current.taskID == latest.taskID {
			m.latestByRoot[canonicalRoot] = latest
		}
		m.mu.Unlock()
	}
	status := "fresh"
	if latest.stale {
		status = "stale"
	}
	successfulAt := latest.successfulAt
	if successfulAt.IsZero() {
		successfulAt = latest.report.GeneratedAt
	}
	return InventorySnapshot{
		Report: latest.report,
		Metadata: SnapshotMetadata{
			Status: status, Stale: latest.stale, ScanTaskID: firstNonEmpty(activeID, latest.taskID),
			LastSuccessfulScanAt: successfulAt, RefreshRequested: activeID != "", FreshnessCheckError: latest.checkError,
		},
	}, true, nil
}

func (m *TaskManager) activeTaskID(root string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeByRoot[canonicalScanRoot(root)]
}

func (m *TaskManager) runInventory(ctx context.Context, state *taskState) {
	id, root, options := state.snapshot.ID, state.root, state.options
	m.update(id, func(snapshot *TaskSnapshot) {
		snapshot.Status = domain.TaskRunning
		snapshot.StartedAt = time.Now().UTC()
	})
	report, err := m.scanner.ScanWithOptions(ctx, root, options, func(progress ScanProgress) {
		m.update(id, func(snapshot *TaskSnapshot) {
			snapshot.FilesSeen = progress.FilesSeen
			snapshot.BytesSeen = progress.BytesSeen
			snapshot.CacheHits = progress.CacheHits
			snapshot.FilesParsed = progress.FilesParsed
		})
	})
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		m.finish(id, func(snapshot *TaskSnapshot) {
			snapshot.CompletedAt = time.Now().UTC()
			if errors.Is(err, context.Canceled) {
				snapshot.Status = domain.TaskCancelled
			} else {
				snapshot.Status = domain.TaskFailed
				snapshot.Error = err.Error()
			}
		})
		return
	}
	m.mu.Lock()
	completedAt := time.Now().UTC()
	if m.activeByRoot[root] != id || ctx.Err() != nil {
		if current := m.tasks[id]; current != nil {
			current.snapshot.CompletedAt = completedAt
			current.snapshot.Status = domain.TaskCancelled
			close(current.done)
		}
		if m.activeByRoot[root] == id {
			delete(m.activeByRoot, root)
		}
		m.mu.Unlock()
		return
	}
	if current, ok := m.tasks[id]; ok {
		current.snapshot.CompletedAt = completedAt
		current.snapshot.Status = domain.TaskCompleted
		current.snapshot.FilesSeen = len(report.Files)
		current.snapshot.Report = &report
	}
	rootModTime := time.Time{}
	if rootInfo, rootErr := os.Stat(root); rootErr == nil {
		rootModTime = rootInfo.ModTime()
	}
	m.latestByRoot[root] = latestInventory{report: report, taskID: id, stale: reportChangedDuringScan(report), rootModTime: rootModTime, checkedAt: completedAt, successfulAt: completedAt}
	if m.activeByRoot[root] == id {
		delete(m.activeByRoot, root)
	}
	if current, ok := m.tasks[id]; ok {
		close(current.done)
	}
	m.mu.Unlock()
}

func (m *TaskManager) update(id string, mutate func(*TaskSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.tasks[id]
	if ok {
		mutate(&state.snapshot)
	}
}

func (m *TaskManager) finish(id string, mutate func(*TaskSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.tasks[id]
	if !ok {
		return
	}
	mutate(&state.snapshot)
	if m.activeByRoot[state.root] == id {
		delete(m.activeByRoot, state.root)
	}
	close(state.done)
}

func canonicalScanRoot(root string) string {
	absolute, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return filepath.Clean(root)
	}
	return absolute
}

func reportSupports(report domain.InventoryReport, options ScanOptions) bool {
	if !options.IncludeAssetGraphs {
		return true
	}
	if len(report.Conversations) == 0 {
		return true
	}
	for _, conversation := range report.Conversations {
		if conversation.AssetGraph == nil {
			return false
		}
	}
	return true
}

func reportChangedDuringScan(report domain.InventoryReport) bool {
	for _, file := range report.Files {
		if file.ChangedDuringScan {
			return true
		}
	}
	return false
}

func cloneTaskSnapshot(snapshot TaskSnapshot) TaskSnapshot {
	cloned := snapshot
	if snapshot.Report != nil {
		report := *snapshot.Report
		report.Files = append([]domain.FileInventory(nil), snapshot.Report.Files...)
		report.Conversations = append([]domain.ConversationSummary(nil), snapshot.Report.Conversations...)
		report.Errors = append([]string(nil), snapshot.Report.Errors...)
		cloned.Report = &report
	}
	return cloned
}
