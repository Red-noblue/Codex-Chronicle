// 中文说明：验证下载后撤回、暂停与断网均不切换当前版本，暂停恢复可继续原计划，暂缓重启允许安全重试。
package update

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPreparedAutomaticUpdateRechecksWithdrawalPauseAndNetwork(t *testing.T) {
	for _, scenario := range []string{"withdrawn", "paused", "offline"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, selectVersion, offline := automaticFixture(t)
			now, idle, restarts := time.Now().UTC(), false, 0
			step := func(at time.Time) error {
				return m.automaticStepAt(t.Context(), "1.0.0", func() bool { return idle }, m.VerifyVersionRoot, func(context.Context, string) error { restarts++; return nil }, at)
			}
			if err := step(now); err != nil {
				t.Fatal(err)
			}
			before, _ := m.Status(t.Context())
			if before.Automatic.PlanID == "" {
				t.Fatal("未准备候选")
			}
			if scenario == "offline" {
				offline.Store(true)
			} else {
				selectVersion(scenario + ":2.0.0")
			}
			idle = true
			if err := step(now.Add(time.Minute)); err == nil {
				t.Fatal("更新策略没有阻断旧计划")
			}
			status, _ := m.Status(t.Context())
			if restarts != 0 || status.State.ActiveVersion != "1.0.0" {
				t.Fatal("撤回、暂停或断网时仍切换版本")
			}
			if scenario == "withdrawn" {
				if status.Automatic.Phase != "blocked_version" || len(status.PendingPlanIDs) != 0 {
					t.Fatal("撤回后未废弃自动计划")
				}
				selectVersion("3.0.0")
			} else {
				if status.Automatic.FailedVersion != "" || status.Automatic.PlanID != before.Automatic.PlanID {
					t.Fatal("暂时问题被误判成坏版本，或原计划丢失")
				}
				selectVersion("2.0.0")
			}
			offline.Store(false)
			if err := step(now.Add(2 * time.Hour)); err != nil {
				t.Fatal(err)
			}
			if restarts != 1 {
				t.Fatal("恢复后没有继续更新")
			}
		})
	}
}

func TestReleaseWithdrawnDuringHealthCheckNeverCommits(t *testing.T) {
	m, _, selectVersion, _ := automaticFixture(t)
	plan, _, err := m.PrepareOnline(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Activate(t.Context(), plan, func(ctx context.Context, root string) error {
		selectVersion("withdrawn:2.0.0")
		return m.VerifyVersionRoot(ctx, root)
	})
	if !errors.Is(err, ErrOnlineReleaseWithdrawn) {
		t.Fatalf("健康预检期间撤回仍提交：%v", err)
	}
	status, _ := m.Status(t.Context())
	if status.State.ActiveVersion != "1.0.0" || status.RecoveryRequired {
		t.Fatal("撤回污染旧版本或留下未恢复事务")
	}
}

func TestPostponedStartupCanRetryIdenticalSignedVersion(t *testing.T) {
	m, _, _, _ := automaticFixture(t)
	plan, _, err := m.PrepareOnline(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	if err := m.PostponeStartup(t.Context(), "2.0.0", "1.0.0", "暂时离线"); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Status(t.Context())
	if status.State.RejectedVersion != "" || status.State.DeferredVersion != "2.0.0" || status.State.ActiveVersion != "1.0.0" {
		t.Fatal("暂缓启动错误地封禁了版本")
	}
	plan, _, err = m.PrepareOnline(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatalf("暂缓后无法重新更新：%v", err)
	}
}

func TestPolicyChangesBeforeScheduledRestartRestoreOldPointer(t *testing.T) {
	for _, scenario := range []string{"paused", "offline", "withdrawn"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, selectVersion, offline := automaticFixture(t)
			now := time.Now().UTC()
			restart := func(ctx context.Context, _ string) error {
				if scenario == "offline" {
					offline.Store(true)
				} else {
					selectVersion(scenario + ":2.0.0")
				}
				status, err := m.Status(ctx)
				if err != nil {
					return err
				}
				return m.AuthorizeOnlineSource(ctx, status.State.Online)
			}
			if err := m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, restart, now); err == nil {
				t.Fatal("重启前策略变化没有被拦截")
			}
			status, err := m.Status(t.Context())
			if err != nil || status.State.ActiveVersion != "1.0.0" {
				t.Fatalf("没有恢复仍在运行的旧版本指针：%+v %v", status, err)
			}
			if scenario == "withdrawn" {
				if status.State.RejectedVersion != "2.0.0" {
					t.Fatal("撤回版本未记录")
				}
				return
			}
			if status.State.RejectedVersion != "" || status.State.DeferredVersion != "2.0.0" {
				t.Fatal("暂时问题永久封禁了版本")
			}
			offline.Store(false)
			selectVersion("2.0.0")
			if err := m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, func(context.Context, string) error { return nil }, now.Add(2*time.Hour)); err != nil {
				t.Fatalf("策略恢复后无法更新：%v", err)
			}
		})
	}
}

func TestActivationCancellationDoesNotLeaveOwnJournalBlockingRetries(t *testing.T) {
	m, _, _, _ := automaticFixture(t)
	plan, _, err := m.PrepareOnline(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err = m.Activate(ctx, plan, func(context.Context, string) error { cancel(); return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("未返回取消原因：%v", err)
	}
	status, err := m.Status(t.Context())
	if err != nil || status.RecoveryRequired || status.State.ActiveVersion != "1.0.0" {
		t.Fatalf("取消后自己的事务阻断了重试：%+v %v", status, err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatalf("取消后无法再次更新：%v", err)
	}
}
