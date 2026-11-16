// 中文说明：管理只读摘要任务、取消和进度快照；结果仅保存在当前 daemon 内存中。
package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"codex-chronicle.local/internal/domain"
	summarypkg "codex-chronicle.local/internal/summary"
)

const maxSummaryTaskItems = 100

// SummaryTaskError 是单个对话摘要失败的可展示错误。
type SummaryTaskError struct {
	ConversationID string `json:"conversation_id"`
	Error          string `json:"error"`
}

// SummaryTaskSnapshot 是摘要任务的进度和派生结果快照。
type SummaryTaskSnapshot struct {
	ID                    string                             `json:"id"`
	Kind                  string                             `json:"kind"`
	Root                  string                             `json:"root"`
	ProviderID            string                             `json:"provider_id"`
	Status                domain.TaskStatus                  `json:"status"`
	CreatedAt             time.Time                          `json:"created_at"`
	StartedAt             time.Time                          `json:"started_at,omitempty"`
	CompletedAt           time.Time                          `json:"completed_at,omitempty"`
	Requested             int                                `json:"requested"`
	Matched               int                                `json:"matched"`
	Completed             int                                `json:"completed"`
	Failed                int                                `json:"failed"`
	CurrentConversationID string                             `json:"current_conversation_id,omitempty"`
	Results               []domain.ConversationSummaryResult `json:"results,omitempty"`
	Errors                []SummaryTaskError                 `json:"errors,omitempty"`
	Error                 string                             `json:"error,omitempty"`
}

type summaryTaskState struct {
	snapshot SummaryTaskSnapshot
	cancel   context.CancelFunc
}

// SummaryTaskManager 管理当前进程内的批量摘要任务。
type SummaryTaskManager struct {
	scanner  Scanner
	registry summarypkg.Registry
	mu       sync.RWMutex
	tasks    map[string]*summaryTaskState
}

// ActiveCount 供退出时等待已启动的摘要任务完成。
func (m *SummaryTaskManager) ActiveCount() int {
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

// NewSummaryTaskManager 创建摘要任务管理器。
func NewSummaryTaskManager(scanner Scanner, registry summarypkg.Registry) *SummaryTaskManager {
	return &SummaryTaskManager{scanner: scanner, registry: registry, tasks: make(map[string]*summaryTaskState)}
}

// Start 在后台启动批量摘要；每个任务最多处理 100 个指定对话。
func (m *SummaryTaskManager) Start(root string, conversationIDs []string, providerID string, maxEvents, maxChars int) (SummaryTaskSnapshot, error) {
	if m == nil {
		return SummaryTaskSnapshot{}, fmt.Errorf("摘要任务管理器未初始化")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return SummaryTaskSnapshot{}, fmt.Errorf("尚未配置只读扫描根目录")
	}
	ids, err := normalizeSummaryIDs(conversationIDs)
	if err != nil {
		return SummaryTaskSnapshot{}, err
	}
	provider, err := m.registry.Resolve(providerID)
	if err != nil {
		return SummaryTaskSnapshot{}, err
	}
	providerID = provider.ID()
	if maxEvents <= 0 {
		maxEvents = defaultSummaryEventLimit
	}
	if maxEvents > maxSummaryEventLimit {
		maxEvents = maxSummaryEventLimit
	}
	if maxChars <= 0 {
		maxChars = 1800
	}
	if maxChars > 12000 {
		maxChars = 12000
	}
	id, err := newSummaryTaskID()
	if err != nil {
		return SummaryTaskSnapshot{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	snapshot := SummaryTaskSnapshot{ID: id, Kind: "summary", Root: root, ProviderID: providerID, Status: domain.TaskQueued, CreatedAt: now, Requested: len(ids), Results: make([]domain.ConversationSummaryResult, 0), Errors: make([]SummaryTaskError, 0)}
	state := &summaryTaskState{snapshot: snapshot, cancel: cancel}
	m.mu.Lock()
	m.tasks[id] = state
	m.mu.Unlock()
	go m.run(ctx, state, ids, maxEvents, maxChars)
	return snapshot, nil
}

// GenerateNow 同步生成单个对话摘要，供详情页按需展示。
func (m *SummaryTaskManager) GenerateNow(ctx context.Context, root, conversationID, providerID string, maxEvents, maxChars int) (domain.ConversationSummaryResult, error) {
	if m == nil {
		return domain.ConversationSummaryResult{}, fmt.Errorf("摘要任务管理器未初始化")
	}
	provider, err := m.registry.Resolve(providerID)
	if err != nil {
		return domain.ConversationSummaryResult{}, err
	}
	report, err := m.scanner.Scan(ctx, root)
	if err != nil {
		return domain.ConversationSummaryResult{}, err
	}
	for _, conversation := range report.Conversations {
		if conversation.ID != conversationID {
			continue
		}
		return m.generate(ctx, root, conversation, provider, maxEvents, maxChars)
	}
	return domain.ConversationSummaryResult{}, fmt.Errorf("未找到指定对话")
}

// Get 返回摘要任务快照；调用方得到的是独立切片副本。
func (m *SummaryTaskManager) Get(id string) (SummaryTaskSnapshot, bool) {
	m.mu.RLock()
	state, ok := m.tasks[id]
	if !ok {
		m.mu.RUnlock()
		return SummaryTaskSnapshot{}, false
	}
	snapshot := cloneSummarySnapshot(state.snapshot)
	m.mu.RUnlock()
	return snapshot, true
}

// Cancel 请求取消摘要任务。
func (m *SummaryTaskManager) Cancel(id string) bool {
	m.mu.RLock()
	state, ok := m.tasks[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	state.cancel()
	return true
}

func (m *SummaryTaskManager) run(ctx context.Context, state *summaryTaskState, ids []string, maxEvents, maxChars int) {
	m.update(state.snapshot.ID, func(snapshot *SummaryTaskSnapshot) {
		snapshot.Status = domain.TaskRunning
		snapshot.StartedAt = time.Now().UTC()
	})
	report, err := m.scanner.Scan(ctx, state.snapshot.Root)
	if err != nil {
		m.update(state.snapshot.ID, func(snapshot *SummaryTaskSnapshot) {
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
	provider, err := m.registry.Resolve(state.snapshot.ProviderID)
	if err != nil {
		m.failTask(state.snapshot.ID, err)
		return
	}
	byID := make(map[string]domain.ConversationSummary, len(report.Conversations))
	for _, conversation := range report.Conversations {
		byID[conversation.ID] = conversation
	}
	m.update(state.snapshot.ID, func(snapshot *SummaryTaskSnapshot) {
		for _, id := range ids {
			if _, ok := byID[id]; ok {
				snapshot.Matched++
			}
		}
	})
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			m.update(state.snapshot.ID, func(snapshot *SummaryTaskSnapshot) {
				snapshot.Status = domain.TaskCancelled
				snapshot.CompletedAt = time.Now().UTC()
			})
			return
		}
		m.update(state.snapshot.ID, func(snapshot *SummaryTaskSnapshot) { snapshot.CurrentConversationID = id })
		conversation, ok := byID[id]
		if !ok {
			m.addError(state.snapshot.ID, id, "未找到指定对话")
			continue
		}
		result, generateErr := m.generate(ctx, state.snapshot.Root, conversation, provider, maxEvents, maxChars)
		if generateErr != nil {
			if errors.Is(generateErr, context.Canceled) {
				m.update(state.snapshot.ID, func(snapshot *SummaryTaskSnapshot) {
					snapshot.Status = domain.TaskCancelled
					snapshot.CompletedAt = time.Now().UTC()
				})
				return
			}
			m.addError(state.snapshot.ID, id, generateErr.Error())
			continue
		}
		m.update(state.snapshot.ID, func(snapshot *SummaryTaskSnapshot) {
			snapshot.Results = append(snapshot.Results, result)
			snapshot.Completed++
		})
	}
	m.update(state.snapshot.ID, func(snapshot *SummaryTaskSnapshot) {
		snapshot.Status = domain.TaskCompleted
		snapshot.CompletedAt = time.Now().UTC()
		snapshot.CurrentConversationID = ""
	})
}

func (m *SummaryTaskManager) generate(ctx context.Context, root string, conversation domain.ConversationSummary, provider summarypkg.Provider, maxEvents, maxChars int) (domain.ConversationSummaryResult, error) {
	events, warnings, err := LoadSummaryEvents(ctx, root, conversation, maxEvents)
	if err != nil {
		return domain.ConversationSummaryResult{}, err
	}
	// 库存首问可能来自已撤回轮次，摘要主题必须由同一有效窗口重新确定；消耗统计原样保留。
	conversation.Title, conversation.FirstUserText = "", ""
	for _, event := range events {
		if event.Role == "user" && event.Kind != "agent_task" {
			conversation.FirstUserText = event.Text
			conversation.Title = limitRunes(event.Text, 100)
			break
		}
	}
	result, err := provider.Summarize(ctx, summarypkg.Request{Conversation: conversation, Events: events, MaxChars: maxChars})
	if err != nil {
		return domain.ConversationSummaryResult{}, err
	}
	if len(warnings) > 0 {
		result.Limitations = append(result.Limitations, warnings...)
	}
	return result, nil
}

func (m *SummaryTaskManager) failTask(id string, err error) {
	m.update(id, func(snapshot *SummaryTaskSnapshot) {
		snapshot.Status = domain.TaskFailed
		snapshot.CompletedAt = time.Now().UTC()
		snapshot.Error = err.Error()
	})
}

func (m *SummaryTaskManager) addError(id, conversationID, message string) {
	m.update(id, func(snapshot *SummaryTaskSnapshot) {
		snapshot.Failed++
		snapshot.Errors = append(snapshot.Errors, SummaryTaskError{ConversationID: conversationID, Error: message})
	})
}

func (m *SummaryTaskManager) update(id string, mutate func(*SummaryTaskSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.tasks[id]; ok {
		mutate(&state.snapshot)
	}
}

func normalizeSummaryIDs(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("批量摘要至少需要一个对话")
	}
	seen := make(map[string]struct{}, len(values))
	ids := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("批量摘要不能包含空对话 ID")
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		ids = append(ids, value)
		if len(ids) > maxSummaryTaskItems {
			return nil, fmt.Errorf("批量摘要最多支持 %d 个对话", maxSummaryTaskItems)
		}
	}
	return ids, nil
}

func newSummaryTaskID() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("生成摘要任务 ID 失败: %w", err)
	}
	return "summary-" + hex.EncodeToString(random[:]), nil
}

func cloneSummarySnapshot(snapshot SummaryTaskSnapshot) SummaryTaskSnapshot {
	snapshot.Results = append([]domain.ConversationSummaryResult(nil), snapshot.Results...)
	snapshot.Errors = append([]SummaryTaskError(nil), snapshot.Errors...)
	return snapshot
}
