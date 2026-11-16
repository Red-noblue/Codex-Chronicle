// 中文说明：管理稳定副本制作和验证的后台任务，避免 80GB 级只读哈希阻塞 HTTP 请求。
package copy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-chronicle.local/internal/domain"
)

// PlanSummary 是 UI/API 可展示的计划摘要，不展开数十万条文件清单。
type PlanSummary struct {
	SourceRoot           string          `json:"source_root"`
	DestinationRoot      string          `json:"destination_root"`
	FileCount            int64           `json:"file_count"`
	TotalBytes           int64           `json:"total_bytes"`
	SourceManifestSHA256 string          `json:"source_manifest_sha256"`
	Marker               MarkerSpec      `json:"marker"`
	ExcludedPaths        []string        `json:"excluded_paths,omitempty"`
	Activity             ActivitySummary `json:"activity,omitempty"`
	Warnings             []string        `json:"warnings,omitempty"`
}

// TaskSnapshot 是副本后台任务的轻量状态；正文和完整文件清单不会通过任务接口返回。
type TaskSnapshot struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	Status          domain.TaskStatus `json:"status"`
	SourceRoot      string            `json:"source_root,omitempty"`
	DestinationRoot string            `json:"destination_root,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	StartedAt       time.Time         `json:"started_at,omitempty"`
	CompletedAt     time.Time         `json:"completed_at,omitempty"`
	Phase           string            `json:"phase,omitempty"`
	FilesDone       int64             `json:"files_done"`
	FilesTotal      int64             `json:"files_total"`
	BytesDone       int64             `json:"bytes_done"`
	BytesTotal      int64             `json:"bytes_total"`
	Plan            *PlanSummary      `json:"plan,omitempty"`
	Result          *CopyResult       `json:"result,omitempty"`
	Error           string            `json:"error,omitempty"`
}

type taskState struct {
	snapshot  TaskSnapshot
	ctx       context.Context
	cancel    context.CancelFunc
	planID    string
	planRoot  string
	planPath  string
	plan      CopyPlan
	hasPlan   bool
	executing bool
	published bool
}

// Manager 管理当前进程内的副本制作任务；源文件和已发布副本的持久生命周期不依赖它。
type Manager struct {
	mu       sync.RWMutex
	tasks    map[string]*taskState
	planRoot string
	sequence uint64
}

// ActiveCount 供安装更新等待副本发布完成，未执行的已完成计划不阻止退出。
func (m *Manager) ActiveCount() int {
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

// NewManager 创建副本任务管理器。
func NewManager() *Manager {
	return NewManagerWithPlanRoot("")
}

// NewManagerWithPlanRoot 创建可将完整 CopyPlan 落盘的副本任务管理器。
// planRoot 为空时保持进程内存模式，供嵌入调用和测试使用。
func NewManagerWithPlanRoot(planRoot string) *Manager {
	planRoot = normalizePlanRoot(planRoot)
	return &Manager{tasks: make(map[string]*taskState), planRoot: planRoot}
}

// SetPlanRoot 切换后续副本计划的持久目录；已创建任务继续使用其原目录。
func (m *Manager) SetPlanRoot(planRoot string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.planRoot = normalizePlanRoot(planRoot)
	m.mu.Unlock()
}

// StartPrepare 后台生成稳定副本计划；该阶段只读取源目录，不创建目标目录。
func (m *Manager) StartPrepare(request CopyRequest) TaskSnapshot {
	state := m.newTask("copy_prepare", request.SourceRoot, request.DestinationRoot)
	state.planRoot = m.planRootSnapshot()
	initial := state.snapshot
	go m.runPrepare(state, request)
	return initial
}

// StartExecute 根据已完成的 prepare 任务启动显式发布；调用方必须先取得 prepared 任务状态。
func (m *Manager) StartExecute(planTaskID string, options ExecuteOptions) (TaskSnapshot, error) {
	if m == nil {
		return TaskSnapshot{}, errors.New("副本任务管理器未初始化")
	}
	var planState *taskState
	m.mu.RLock()
	_, ok := m.tasks[planTaskID]
	m.mu.RUnlock()
	if !ok {
		if _, restoreErr := m.restorePersistedPlan(planTaskID); restoreErr != nil {
			return TaskSnapshot{}, restoreErr
		}
	}

	// 在同一把锁下重新读取并占用计划，避免两个请求同时执行同一份副本计划。
	m.mu.Lock()
	planState, ok = m.tasks[planTaskID]
	if !ok {
		m.mu.Unlock()
		return TaskSnapshot{}, fmt.Errorf("副本计划任务不存在：%s", planTaskID)
	}
	if planState.published {
		m.mu.Unlock()
		return TaskSnapshot{}, fmt.Errorf("副本计划已经发布：%s", planTaskID)
	}
	if planState.executing {
		m.mu.Unlock()
		return TaskSnapshot{}, fmt.Errorf("副本计划正在执行：%s", planTaskID)
	}
	if planState.planRoot != m.planRoot {
		m.mu.Unlock()
		return TaskSnapshot{}, fmt.Errorf("副本计划属于其他工作区：%s", planTaskID)
	}
	if planState.snapshot.Kind != "copy_prepare" || planState.snapshot.Status != domain.TaskCompleted || !planState.hasPlan {
		status := planState.snapshot.Status
		m.mu.Unlock()
		return TaskSnapshot{}, fmt.Errorf("副本计划尚未完成，当前状态：%s", status)
	}
	plan := planState.plan
	planPath := planState.planPath
	planState.executing = true
	m.mu.Unlock()
	if planPath != "" {
		var err error
		plan, err = loadPlan(planPath)
		if err != nil {
			m.releasePlan(planTaskID)
			return TaskSnapshot{}, fmt.Errorf("读取副本计划失败：%w", err)
		}
	}
	state := m.newTask("copy_execute", plan.SourceRoot, plan.DestinationRoot)
	state.planID = planTaskID
	initial := state.snapshot
	go m.runExecute(state, plan, options)
	return initial, nil
}

// StartVerify 后台验证已发布副本的 manifest 和所有文件哈希。
func (m *Manager) StartVerify(root string) TaskSnapshot {
	state := m.newTask("copy_verify", "", root)
	initial := state.snapshot
	go m.runVerify(state, root)
	return initial
}

// Get 返回任务快照；不存在时返回 false。
func (m *Manager) Get(id string) (TaskSnapshot, bool) {
	if m == nil {
		return TaskSnapshot{}, false
	}
	m.mu.RLock()
	state, ok := m.tasks[id]
	if !ok {
		m.mu.RUnlock()
		if _, restoreErr := m.restorePersistedPlan(id); restoreErr != nil {
			return TaskSnapshot{}, false
		}
		m.mu.RLock()
		state, ok = m.tasks[id]
		if !ok {
			m.mu.RUnlock()
			return TaskSnapshot{}, false
		}
	}
	snapshot := state.snapshot
	m.mu.RUnlock()
	return snapshot, true
}

// ListPrepared 返回内存和落盘的 prepare 计划摘要，供 daemon 重启后的 UI 重新发现待确认任务。
// 返回值不包含完整文件清单，也不会自动读取源目录或启动执行。
func (m *Manager) ListPrepared() []TaskSnapshot {
	if m == nil {
		return []TaskSnapshot{}
	}
	seen := make(map[string]struct{})
	result := make([]TaskSnapshot, 0)
	planRoot := m.planRootSnapshot()
	m.mu.RLock()
	for id, state := range m.tasks {
		if state.planRoot == planRoot && state.snapshot.Kind == "copy_prepare" && state.snapshot.Status == domain.TaskCompleted && !state.executing && !state.published {
			seen[id] = struct{}{}
			result = append(result, state.snapshot)
		}
	}
	m.mu.RUnlock()
	if strings.TrimSpace(planRoot) != "" {
		if entries, err := os.ReadDir(planRoot); err == nil {
			const prefix = "copy-plan-"
			const suffix = ".json"
			for _, entry := range entries {
				if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), suffix) {
					continue
				}
				id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), prefix), suffix)
				if id == "" {
					continue
				}
				if _, exists := seen[id]; exists {
					continue
				}
				published, statusErr := isPlanPublished(planRoot, id)
				if statusErr != nil || published {
					continue
				}
				snapshot, err := m.restorePersistedPlan(id)
				if err != nil || snapshot.Kind != "copy_prepare" || snapshot.Status != domain.TaskCompleted {
					continue
				}
				seen[id] = struct{}{}
				result = append(result, snapshot)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// Cancel 请求取消任务；任务在当前文件边界响应取消。
func (m *Manager) Cancel(id string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	state, ok := m.tasks[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	state.cancel()
	return true
}

func (m *Manager) newTask(kind, source, destination string) *taskState {
	sequence := atomic.AddUint64(&m.sequence, 1)
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	state := &taskState{snapshot: TaskSnapshot{ID: fmt.Sprintf("copy-%s-%d-%d", kind, now.UnixNano(), sequence), Kind: kind, Status: domain.TaskQueued, SourceRoot: source, DestinationRoot: destination, CreatedAt: now}, ctx: ctx, cancel: cancel}
	m.mu.Lock()
	if m.tasks == nil {
		m.tasks = make(map[string]*taskState)
	}
	state.snapshot.Phase = "queued"
	m.tasks[state.snapshot.ID] = state
	m.mu.Unlock()
	return state
}

func (m *Manager) runPrepare(state *taskState, request CopyRequest) {
	m.update(state.snapshot.ID, func(snapshot *TaskSnapshot) {
		snapshot.Status = domain.TaskRunning
		snapshot.StartedAt = time.Now().UTC()
		snapshot.Phase = "prepare"
	})
	request.Progress = m.progress(state.snapshot.ID)
	plan, err := Prepare(state.ctx, request)
	if err != nil {
		// 失败时仅保留摘要，便于 UI 显示活动锁和阻断原因，不把完整文件清单放入任务快照。
		if plan.SourceRoot != "" {
			m.update(state.snapshot.ID, func(snapshot *TaskSnapshot) { snapshot.Plan = planSummary(plan) })
		}
		m.finishError(state.snapshot.ID, err)
		return
	}
	planPath := ""
	if state.planRoot != "" {
		planPath, err = persistPlan(state.planRoot, state.snapshot.ID, plan)
		if err != nil {
			m.finishError(state.snapshot.ID, err)
			return
		}
	}
	m.mu.Lock()
	if current, ok := m.tasks[state.snapshot.ID]; ok {
		current.planPath = planPath
		if planPath == "" {
			current.plan = plan
		}
		current.hasPlan = true
		current.snapshot.Plan = planSummary(plan)
	}
	m.mu.Unlock()
	m.finish(state.snapshot.ID, domain.TaskCompleted, "prepared", nil)
}

func (m *Manager) runExecute(state *taskState, plan CopyPlan, options ExecuteOptions) {
	m.update(state.snapshot.ID, func(snapshot *TaskSnapshot) {
		snapshot.Status = domain.TaskRunning
		snapshot.StartedAt = time.Now().UTC()
		snapshot.Phase = "execute"
		snapshot.FilesTotal = plan.FileCount
		snapshot.BytesTotal = plan.TotalBytes
	})
	options.Progress = m.progress(state.snapshot.ID)
	result, err := Execute(state.ctx, plan, options)
	if err != nil {
		m.releasePlan(state.planID)
		m.finishError(state.snapshot.ID, err)
		return
	}
	m.update(state.snapshot.ID, func(snapshot *TaskSnapshot) { snapshot.Result = &result })
	m.consumePlan(state.planID)
	m.finish(state.snapshot.ID, domain.TaskCompleted, "published", nil)
}

func (m *Manager) runVerify(state *taskState, root string) {
	m.update(state.snapshot.ID, func(snapshot *TaskSnapshot) {
		snapshot.Status = domain.TaskRunning
		snapshot.StartedAt = time.Now().UTC()
		snapshot.Phase = "verify"
	})
	result, err := VerifyDestination(state.ctx, root)
	if err != nil {
		m.finishError(state.snapshot.ID, err)
		return
	}
	copyResult := CopyResult{Published: result.Verified, DestinationRoot: result.Root, ManifestPath: result.Root + "/" + ManifestName, FileCount: result.FileCount, TotalBytes: result.TotalBytes, SourceManifestSHA256: result.SourceManifestSHA256, DestinationManifestSHA256: result.SourceManifestSHA256}
	m.update(state.snapshot.ID, func(snapshot *TaskSnapshot) { snapshot.Result = &copyResult })
	m.finish(state.snapshot.ID, domain.TaskCompleted, "verified", nil)
}

func (m *Manager) progress(id string) ProgressFunc {
	return func(progress Progress) {
		m.update(id, func(snapshot *TaskSnapshot) {
			snapshot.Phase = progress.Phase
			snapshot.FilesDone = progress.FilesDone
			snapshot.FilesTotal = progress.FilesTotal
			snapshot.BytesDone = progress.BytesDone
			snapshot.BytesTotal = progress.BytesTotal
		})
	}
}

func (m *Manager) finishError(id string, err error) {
	status := domain.TaskFailed
	if errors.Is(err, context.Canceled) {
		status = domain.TaskCancelled
	}
	m.finish(id, status, "failed", err)
}

func (m *Manager) finish(id string, status domain.TaskStatus, phase string, taskErr error) {
	m.update(id, func(snapshot *TaskSnapshot) {
		snapshot.Status = status
		snapshot.Phase = phase
		snapshot.CompletedAt = time.Now().UTC()
		if taskErr != nil {
			snapshot.Error = taskErr.Error()
		}
	})
}

func (m *Manager) update(id string, mutate func(*TaskSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.tasks[id]; ok {
		mutate(&state.snapshot)
	}
}

func (m *Manager) releasePlan(planTaskID string) {
	if strings.TrimSpace(planTaskID) == "" {
		return
	}
	m.mu.Lock()
	if state, ok := m.tasks[planTaskID]; ok {
		state.executing = false
	}
	m.mu.Unlock()
}

func (m *Manager) consumePlan(planTaskID string) {
	if strings.TrimSpace(planTaskID) == "" {
		return
	}
	m.mu.Lock()
	state, ok := m.tasks[planTaskID]
	if !ok {
		m.mu.Unlock()
		return
	}
	state.executing = false
	state.published = true
	planRoot, planPath := state.planRoot, state.planPath
	m.mu.Unlock()
	if planPath == "" {
		return
	}
	if err := markPlanPublished(planRoot, planTaskID); err != nil {
		// 副本已完成发布；保留任务为已发布并把状态写入失败暴露给 UI，避免重复执行。
		m.update(planTaskID, func(snapshot *TaskSnapshot) {
			snapshot.Error = "副本已发布，但待确认计划状态标记写入失败：" + err.Error()
		})
	}
}

func planSummary(plan CopyPlan) *PlanSummary {
	return &PlanSummary{SourceRoot: plan.SourceRoot, DestinationRoot: plan.DestinationRoot, FileCount: plan.FileCount, TotalBytes: plan.TotalBytes, SourceManifestSHA256: plan.SourceManifestSHA256, Marker: plan.Marker, ExcludedPaths: append([]string(nil), plan.ExcludedPaths...), Activity: plan.Activity, Warnings: append([]string(nil), plan.Warnings...)}
}

func persistPlan(planRoot, taskID string, plan CopyPlan) (string, error) {
	if strings.TrimSpace(planRoot) == "" {
		return "", nil
	}
	if _, err := ensureDirectoryPath(planRoot); err != nil {
		return "", fmt.Errorf("副本计划目录不安全：%w", err)
	}
	path := filepath.Join(planRoot, "copy-plan-"+taskID+".json")
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("创建副本计划文件失败：%w", err)
	}
	encoderErr := json.NewEncoder(file).Encode(plan)
	if encoderErr == nil {
		encoderErr = file.Sync()
	}
	closeErr := file.Close()
	if encoderErr != nil {
		_ = os.Remove(temporary)
		return "", fmt.Errorf("写入副本计划文件失败：%w", encoderErr)
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return "", fmt.Errorf("关闭副本计划文件失败：%w", closeErr)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return "", fmt.Errorf("发布副本计划文件失败：%w", err)
	}
	if err := syncDirectory(planRoot); err != nil {
		return "", fmt.Errorf("同步副本计划目录失败：%w", err)
	}
	return path, nil
}

// restorePersistedPlan 将进程重启前落盘的 prepare 计划恢复为可查询、可执行的任务。
// 它只读取计划和元数据，不读取源目录，也不会自动开始复制或写入目标。
func (m *Manager) restorePersistedPlan(taskID string) (TaskSnapshot, error) {
	planRoot := m.planRootSnapshot()
	if strings.TrimSpace(planRoot) == "" {
		return TaskSnapshot{}, fmt.Errorf("副本计划未配置持久目录")
	}
	if strings.TrimSpace(taskID) == "" || filepath.Base(taskID) != taskID || strings.ContainsAny(taskID, `/\\`) {
		return TaskSnapshot{}, fmt.Errorf("副本计划任务 ID 不安全：%q", taskID)
	}
	if err := ensureExistingAncestorsSafe(planRoot); err != nil {
		return TaskSnapshot{}, fmt.Errorf("读取副本计划目录失败：%w", err)
	}
	if published, err := isPlanPublished(planRoot, taskID); err != nil {
		return TaskSnapshot{}, fmt.Errorf("读取副本计划状态失败：%w", err)
	} else if published {
		return TaskSnapshot{}, fmt.Errorf("副本计划已经发布：%s", taskID)
	}
	path := filepath.Join(planRoot, "copy-plan-"+taskID+".json")
	plan, err := loadPlan(path)
	if err != nil {
		return TaskSnapshot{}, fmt.Errorf("读取持久化副本计划失败：%w", err)
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	state := &taskState{
		snapshot: TaskSnapshot{
			ID: taskID, Kind: "copy_prepare", Status: domain.TaskCompleted,
			SourceRoot: plan.SourceRoot, DestinationRoot: plan.DestinationRoot,
			CreatedAt: plan.CreatedAt, StartedAt: plan.CreatedAt, CompletedAt: now,
			Phase: "prepared", Plan: planSummary(plan),
		},
		ctx: ctx, cancel: cancel, planRoot: planRoot, planPath: path, hasPlan: true,
	}
	m.mu.Lock()
	if existing, exists := m.tasks[taskID]; exists {
		snapshot := existing.snapshot
		m.mu.Unlock()
		cancel()
		return snapshot, nil
	}
	m.tasks[taskID] = state
	m.mu.Unlock()
	return state.snapshot, nil
}

const publishedPlanSuffix = ".published"

func planPublishedPath(planRoot, taskID string) string {
	return filepath.Join(planRoot, "copy-plan-"+taskID+publishedPlanSuffix)
}

func isPlanPublished(planRoot, taskID string) (bool, error) {
	if strings.TrimSpace(planRoot) == "" {
		return false, nil
	}
	path := planPublishedPath(planRoot, taskID)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("副本计划状态文件不是普通文件：%s", path)
	}
	return true, nil
}

func markPlanPublished(planRoot, taskID string) error {
	if strings.TrimSpace(planRoot) == "" {
		return nil
	}
	if filepath.Base(taskID) != taskID || strings.ContainsAny(taskID, `/\\`) {
		return fmt.Errorf("副本计划任务 ID 不安全：%q", taskID)
	}
	path := planPublishedPath(planRoot, taskID)
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(time.Now().UTC().Format(time.RFC3339Nano) + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDirectory(planRoot)
}

func (m *Manager) planRootSnapshot() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.planRoot
}

func normalizePlanRoot(planRoot string) string {
	if strings.TrimSpace(planRoot) == "" {
		return ""
	}
	abs, err := filepath.Abs(planRoot)
	if err != nil {
		return ""
	}
	return filepath.Clean(abs)
}

func loadPlan(path string) (CopyPlan, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return CopyPlan{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return CopyPlan{}, fmt.Errorf("副本计划文件不是普通文件：%s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return CopyPlan{}, err
	}
	defer file.Close()
	var plan CopyPlan
	if err := json.NewDecoder(file).Decode(&plan); err != nil {
		return CopyPlan{}, err
	}
	if err := validatePlan(plan); err != nil {
		return CopyPlan{}, err
	}
	return plan, nil
}
