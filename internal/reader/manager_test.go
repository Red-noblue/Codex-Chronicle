// 中文说明：验证冷构建进度、可取消等待、全局失效及超大行阻断，不发布部分导航。
package reader

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

type waitingProjectionCache struct {
	entered chan struct{}
	release chan struct{}
}

func (c *waitingProjectionCache) LoadReaderProjection(ctx context.Context, _, _ string) ([]byte, error) {
	close(c.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
		return nil, nil
	}
}
func (*waitingProjectionCache) SaveReaderProjection(ctx context.Context, _, _ string, _ []byte) error {
	return ctx.Err()
}

func TestProjectionWaitingCanCancelAndInvalidationStopsPublish(t *testing.T) {
	for _, invalidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel_wait", true: "invalidate"}[invalidate], func(t *testing.T) {
			s := source(t, t.TempDir(), "waiting.jsonl", message("keep"))
			plan := BuildPlan(t.Context(), []Source{s}, []string{s.Relative})
			m := NewManager()
			defer m.Invalidate()
			cache := &waitingProjectionCache{entered: make(chan struct{}), release: make(chan struct{})}
			p, _, err := m.Prepare(t.Context(), plan, cache, time.Millisecond)
			if err != nil || p != nil {
				t.Fatal("cold build did not yield", p, err)
			}
			select {
			case <-cache.entered:
			case <-time.After(time.Second):
				t.Fatal("builder never started")
			}
			if invalidate {
				job := m.job(plan, cache)
				m.Invalidate()
				select {
				case <-job.done:
					if !errors.Is(job.err, context.Canceled) || job.value != nil {
						t.Fatal("invalidated result published", job.err)
					}
				case <-time.After(time.Second):
					t.Fatal("invalidation did not stop build")
				}
			} else {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if _, err = m.Wait(ctx, plan, cache); !errors.Is(err, context.Canceled) {
					t.Fatal("cancel ignored", err)
				}
				close(cache.release)
				if p, err = m.Wait(t.Context(), plan, cache); err != nil || len(p.Turns) != 1 {
					t.Fatal("cancelled waiter damaged shared build", p, err)
				}
			}
		})
	}
}

func TestProjectionRejectsOversizedRecordWithoutPartialResult(t *testing.T) {
	s := source(t, t.TempDir(), "large.jsonl", message("keep")+message(strings.Repeat("x", MaxLineBytes)))
	m := NewManager()
	defer m.Invalidate()
	plan := BuildPlan(t.Context(), []Source{s}, []string{s.Relative})
	if p, err := m.Wait(t.Context(), plan, nil); err == nil || p != nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatal("oversized source published partial projection", p, err)
	}
}

type projectionCacheRecorder struct{ writes int }

func (*projectionCacheRecorder) LoadReaderProjection(context.Context, string, string) ([]byte, error) {
	return nil, nil
}

func (c *projectionCacheRecorder) SaveReaderProjection(context.Context, string, string, []byte) error {
	c.writes++
	return nil
}

func TestProjectionOverDiskBudgetRemainsReadable(t *testing.T) {
	const turns = 36000
	line := message(strings.Repeat("对", 100))
	data := strings.Repeat(line, turns-1) + message("last question")
	s := source(t, t.TempDir(), "rollout-2026-11-21T10-00-00-projection.jsonl", data)
	plan := BuildPlan(t.Context(), []Source{s}, []string{s.Relative})
	m := NewManager()
	defer m.Invalidate()
	cache := &projectionCacheRecorder{}
	job := m.job(plan, cache)
	p, err := m.Wait(t.Context(), plan, cache)
	if err != nil {
		t.Fatal(err)
	}
	if job.size <= maxProjectionCacheBytes || job.size > maxProjectionMemoryBytes || cache.writes != 0 || len(p.Turns) != turns {
		t.Fatalf("应保留完整内存投影且跳过超预算缓存：大小 %d，保存 %d 次，轮次 %d", job.size, cache.writes, len(p.Turns))
	}
	plan.Projection = p
	page, err := ReadPage(t.Context(), plan, Position{Path: s.Relative, Offset: int64((turns - 1) * len(line)), Line: turns - 1}, 1, nil, true)
	if err != nil || len(page.Items) != 1 || page.Items[0].Text != "last question" {
		t.Fatalf("应能从末尾结果定位正文：%v，%+v", err, page.Items)
	}
}

func TestProjectionRejectsTurnLimitAtEndOfSnapshot(t *testing.T) {
	var data strings.Builder
	for i := 0; i <= maxProjectionEntries; i++ {
		data.WriteString(evt("task_started", map[string]any{"turn_id": strconv.Itoa(i)}))
	}
	s := source(t, t.TempDir(), "turn-limit.jsonl", data.String())
	plan := BuildPlan(t.Context(), []Source{s}, []string{s.Relative})
	if p, err := BuildProjection(t.Context(), plan, nil); err == nil || p != nil || !strings.Contains(err.Error(), "100000") {
		t.Fatal("turn limit silently discarded metadata", p, err)
	}
}
