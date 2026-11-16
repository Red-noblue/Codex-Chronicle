// 中文说明：管理 LiveBestEffort 活动副本任务，允许后台扫描和部分发布而不阻塞 API 请求。
package copy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"codex-chronicle.local/internal/domain"
)

// LivePlanSummary 是活动副本任务在 API/UI 中展示的有限摘要。
type LivePlanSummary struct {
	SourceRoot      string          `json:"source_root"`
	DestinationRoot string          `json:"destination_root"`
	AssetCount      int             `json:"asset_count"`
	StableCount     int             `json:"stable_count"`
	ActiveCount     int             `json:"active_count"`
	QueuedCount     int             `json:"queued_count"`
	ExcludedCount   int             `json:"excluded_count"`
	Activity        ActivitySummary `json:"activity,omitempty"`
	Warnings        []string        `json:"warnings,omitempty"`
}

// LiveTaskSnapshot 是活动副本任务状态；完整资产证据只在任务完成后随结果返回。
type LiveTaskSnapshot struct {
	ID              string              `json:"id"`
	Kind            string              `json:"kind"`
	Status          domain.TaskStatus   `json:"status"`
	SourceRoot      string              `json:"source_root,omitempty"`
	DestinationRoot string              `json:"destination_root,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	StartedAt       time.Time           `json:"started_at,omitempty"`
	CompletedAt     time.Time           `json:"completed_at,omitempty"`
	Phase           string              `json:"phase,omitempty"`
	Plan            *LivePlanSummary    `json:"plan,omitempty"`
	Result          *LiveSnapshotResult `json:"result,omitempty"`
	Error           string              `json:"error,omitempty"`
}

type liveTaskState struct {
	snapshot  LiveTaskSnapshot
	ctx       context.Context
	cancel    context.CancelFunc
	plan      LiveSnapshotPlan
	hasPlan   bool
	executing bool
}

// LiveManager 管理活动副本任务；源目录和活动副本目标均不依赖进程内状态恢复。
type LiveManager struct {
	mu       sync.RWMutex
	tasks    map[string]*liveTaskState
	sequence uint64
}

// ActiveCount 供安装更新等待活动副本任务完成。
func (m *LiveManager) ActiveCount() int {
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

// NewLiveManager 创建活动副本任务管理器。
func NewLiveManager() *LiveManager {
	return &LiveManager{tasks: make(map[string]*liveTaskState)}
}

// StartPrepare 异步创建 LiveBestEffort 计划。
func (m *LiveManager) StartPrepare(request LiveSnapshotRequest) LiveTaskSnapshot {
	state := m.newTask("live_prepare", request.SourceRoot, request.DestinationRoot)
	initial := state.snapshot
	go m.runPrepare(state, request)
	return initial
}

// StartExecute 显式发布已完成的活动副本计划。
func (m *LiveManager) StartExecute(id string, options ExecuteOptions) (LiveTaskSnapshot, error) {
	if m == nil {
		return LiveTaskSnapshot{}, errors.New("活动副本任务管理器未初始化")
	}
	m.mu.Lock()
	state, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return LiveTaskSnapshot{}, fmt.Errorf("活动副本任务不存在：%s", id)
	}
	if state.executing {
		m.mu.Unlock()
		return LiveTaskSnapshot{}, fmt.Errorf("活动副本任务正在执行：%s", id)
	}
	if state.snapshot.Kind != "live_prepare" || state.snapshot.Status != domain.TaskCompleted || !state.hasPlan {
		status := state.snapshot.Status
		m.mu.Unlock()
		return LiveTaskSnapshot{}, fmt.Errorf("活动副本计划尚未完成，当前状态：%s", status)
	}
	state.executing = true
	plan := state.plan
	m.mu.Unlock()
	execution := m.newTask("live_execute", plan.SourceRoot, plan.DestinationRoot)
	// 在启动后台 goroutine 前保存初始快照，避免异步状态更新与返回值读取发生竞态。
	initial := execution.snapshot
	go m.runExecute(execution, id, plan, options)
	return initial, nil
}

// Get 返回活动副本任务状态。
func (m *LiveManager) Get(id string) (LiveTaskSnapshot, bool) {
	if m == nil {
		return LiveTaskSnapshot{}, false
	}
	m.mu.RLock()
	state, ok := m.tasks[id]
	if !ok {
		m.mu.RUnlock()
		return LiveTaskSnapshot{}, false
	}
	snapshot := state.snapshot
	m.mu.RUnlock()
	return snapshot, true
}

// Cancel 请求取消活动副本任务。
func (m *LiveManager) Cancel(id string) bool {
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

func (m *LiveManager) newTask(kind, source, destination string) *liveTaskState {
	sequence := atomic.AddUint64(&m.sequence, 1)
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	state := &liveTaskState{snapshot: LiveTaskSnapshot{ID: fmt.Sprintf("copy-%s-%d-%d", kind, now.UnixNano(), sequence), Kind: kind, Status: domain.TaskQueued, SourceRoot: source, DestinationRoot: destination, CreatedAt: now, Phase: "queued"}, ctx: ctx, cancel: cancel}
	m.mu.Lock()
	if m.tasks == nil {
		m.tasks = make(map[string]*liveTaskState)
	}
	m.tasks[state.snapshot.ID] = state
	m.mu.Unlock()
	return state
}

func (m *LiveManager) runPrepare(state *liveTaskState, request LiveSnapshotRequest) {
	m.update(state.snapshot.ID, func(snapshot *LiveTaskSnapshot) {
		snapshot.Status = domain.TaskRunning
		snapshot.StartedAt = time.Now().UTC()
		snapshot.Phase = "live-prepare"
	})
	plan, err := PrepareLive(state.ctx, request)
	if err != nil {
		m.finishError(state.snapshot.ID, err)
		return
	}
	m.mu.Lock()
	if current, ok := m.tasks[state.snapshot.ID]; ok {
		current.plan = plan
		current.hasPlan = true
		current.snapshot.Plan = livePlanSummary(plan)
	}
	m.mu.Unlock()
	m.finish(state.snapshot.ID, domain.TaskCompleted, "prepared", nil)
}

func (m *LiveManager) runExecute(state *liveTaskState, planID string, plan LiveSnapshotPlan, options ExecuteOptions) {
	m.update(state.snapshot.ID, func(snapshot *LiveTaskSnapshot) {
		snapshot.Status = domain.TaskRunning
		snapshot.StartedAt = time.Now().UTC()
		snapshot.Phase = "live-execute"
	})
	result, err := ExecuteLive(state.ctx, plan, options)
	if err != nil {
		m.release(planID)
		m.finishError(state.snapshot.ID, err)
		return
	}
	m.update(state.snapshot.ID, func(snapshot *LiveTaskSnapshot) { snapshot.Result = &result })
	m.release(planID)
	m.finish(state.snapshot.ID, domain.TaskCompleted, "published", nil)
}

func (m *LiveManager) release(id string) {
	m.mu.Lock()
	if state, ok := m.tasks[id]; ok {
		state.executing = false
	}
	m.mu.Unlock()
}

func (m *LiveManager) update(id string, mutate func(*LiveTaskSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.tasks[id]; ok {
		mutate(&state.snapshot)
	}
}

func (m *LiveManager) finishError(id string, err error) {
	status := domain.TaskFailed
	if errors.Is(err, context.Canceled) {
		status = domain.TaskCancelled
	}
	m.finish(id, status, "failed", err)
}

func (m *LiveManager) finish(id string, status domain.TaskStatus, phase string, taskErr error) {
	m.update(id, func(snapshot *LiveTaskSnapshot) {
		snapshot.Status = status
		snapshot.Phase = phase
		snapshot.CompletedAt = time.Now().UTC()
		if taskErr != nil {
			snapshot.Error = taskErr.Error()
		}
	})
}

func livePlanSummary(plan LiveSnapshotPlan) *LivePlanSummary {
	summary := &LivePlanSummary{SourceRoot: plan.SourceRoot, DestinationRoot: plan.DestinationRoot, AssetCount: len(plan.Assets), ExcludedCount: len(plan.ExcludedPaths), Activity: plan.Activity, Warnings: append([]string(nil), plan.Warnings...)}
	for _, asset := range plan.Assets {
		if asset.Active {
			summary.ActiveCount++
		}
		if asset.Queued {
			summary.QueuedCount++
		}
		if asset.Complete {
			summary.StableCount++
		}
	}
	return summary
}
