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
