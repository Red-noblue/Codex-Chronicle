// 中文说明：在后台构建有界元数据投影；前台可取消等待，分页复用已发布快照，磁盘缓存仅写本项目数据库。
package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const maxProjectionCacheBytes = 16 << 20
const maxProjectionMemoryBytes = 32 << 20

type ProjectionCache interface {
	LoadReaderProjection(context.Context, string, string) ([]byte, error)
	SaveReaderProjection(context.Context, string, string, []byte) error
}
type projectionJob struct {
	done     chan struct{}
	progress atomic.Int64
	value    *Projection
	err      error
	size     int
	cancel   context.CancelFunc
	ctx      context.Context
	persist  sync.Once
}
type Manager struct {
	mu    sync.Mutex
	jobs  map[string]*projectionJob
	order []string
	gate  chan struct{}
}

func NewManager() *Manager {
	return &Manager{jobs: map[string]*projectionJob{}, gate: make(chan struct{}, 1)}
}

var DefaultManager = NewManager()

// ScopeKey 包含绝对来源但只存摘要，隔离不同机器目录及不同物理历史。
func ScopeKey(plan Plan) string {
	value := ""
	for _, s := range plan.Segments {
		value += s.Path + "\x00"
	}
	return shortHash([]byte(value))
}
func (m *Manager) job(plan Plan, cache ProjectionCache) *projectionJob {
	key := ScopeKey(plan) + ":" + Stamp(plan)
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[key]; job != nil {
		select {
		case <-job.done:
			if job.err == nil {
				return job
			}
			job.cancel()
			delete(m.jobs, key)
			kept := m.order[:0]
			for _, old := range m.order {
				if old != key {
					kept = append(kept, old)
				}
			}
			m.order = kept
		default:
			return job
		}
	}
	if len(m.jobs) >= 16 {
		j := &projectionJob{done: make(chan struct{}), err: fmt.Errorf("历史构建队列已满，请稍后重试")}
		close(j.done)
		return j
	}
	job := &projectionJob{done: make(chan struct{})}
	job.ctx, job.cancel = context.WithCancel(context.Background())
	m.jobs[key] = job
	m.order = append(m.order, key)
	go func() {
		ctx, cancel := context.WithTimeout(job.ctx, 10*time.Minute)
		defer cancel()
		select {
		case m.gate <- struct{}{}:
		case <-ctx.Done():
			job.err = ctx.Err()
			close(job.done)
			return
		}
		defer func() { <-m.gate }()
		if cache != nil {
			if b, err := cache.LoadReaderProjection(ctx, ScopeKey(plan), Stamp(plan)); err == nil && len(b) > 0 && len(b) <= maxProjectionCacheBytes {
				var p Projection
				if json.Unmarshal(b, &p) == nil && p.validFor(plan) && ctx.Err() == nil {
					p.initialize()
					job.value = &p
					job.size = len(b)
					job.persist.Do(func() {})
					close(job.done)
					m.trim()
					return
				}
			}
		}
		job.value, job.err = BuildProjection(ctx, plan, &job.progress)
		if job.err == nil {
			b, err := json.Marshal(job.value)
			if err != nil || len(b) > maxProjectionMemoryBytes {
				job.value = nil
				job.err = fmt.Errorf("导航元数据超过 32 MiB 上限，未发布部分投影")
			} else {
				job.size = len(b)
				if err := job.save(ctx, plan, cache); err != nil {
					job.value.Diagnostics = append(job.value.Diagnostics, "导航缓存未保存，当前窗口仍使用已验证的内存快照")
				}
			}
		}
		close(job.done)
		m.trim()
	}()
	return job
}
func (m *Manager) trim() {
	m.mu.Lock()
	defer m.mu.Unlock()
	bytes := 0
	for _, j := range m.jobs {
		select {
		case <-j.done:
			bytes += j.size
		default:
		}
	}
	kept := []string{}
	for _, key := range m.order {
		job := m.jobs[key]
		if job == nil {
			continue
		}
		select {
		case <-job.done:
			if len(m.jobs) > 8 || bytes > maxProjectionMemoryBytes {
				job.cancel()
				delete(m.jobs, key)
				bytes -= job.size
				continue
			}
		default:
		}
		kept = append(kept, key)
	}
	m.order = kept
}

// save 允许库存先创建内存投影，后续阅读请求再补写磁盘；失效时取消尚在进行的保存。
func (job *projectionJob) save(ctx context.Context, plan Plan, cache ProjectionCache) (err error) {
	// 超过单份磁盘缓存上限时，内存中的完整导航仍能用于搜索和分页。
	if cache == nil || job.value == nil || job.err != nil || job.size > maxProjectionCacheBytes {
		return nil
	}
	job.persist.Do(func() {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(job.ctx, cancel)
		defer stop()
		if job.ctx.Err() != nil {
			cancel()
		}
		var b []byte
		b, err = json.Marshal(job.value)
		if err == nil {
			err = cache.SaveReaderProjection(ctx, ScopeKey(plan), Stamp(plan), b)
		}
	})
	return err
}

// Invalidate 取消尚未发布的派生任务并释放阅读元数据，供删除后的派生清理使用。
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		job.cancel()
	}
	m.jobs = map[string]*projectionJob{}
	m.order = nil
}
func (m *Manager) Prepare(ctx context.Context, plan Plan, cache ProjectionCache, wait time.Duration) (*Projection, int64, error) {
	job := m.job(plan, cache)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, job.progress.Load(), ctx.Err()
	case <-job.done:
		_ = job.save(ctx, plan, cache)
		return job.value, job.progress.Load(), job.err
	case <-timer.C:
		return nil, job.progress.Load(), nil
	}
}
func (m *Manager) Wait(ctx context.Context, plan Plan, cache ProjectionCache) (*Projection, error) {
	job := m.job(plan, cache)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-job.done:
		_ = job.save(ctx, plan, cache)
		return job.value, job.err
	}
}
