// 中文说明：默认后台检查和下载已签名更新，空闲后切换；持久化进度、退避和失败版本，避免重复安装。
package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const AutomaticCheckInterval = 6 * time.Hour
const automaticRetryInterval = time.Hour

var ErrInvalidOnlinePackage = errors.New("更新包校验未通过")

type AutomaticStatus struct {
	Mode           UpdateMode `json:"mode"`
	DownloadPaused bool       `json:"download_paused"`
	ManualDownload bool       `json:"manual_download,omitempty"`
	ManualPlanID   string     `json:"manual_plan_id,omitempty"`
	Enabled        bool       `json:"enabled"`
	Phase          string     `json:"phase"`
	PlanID         string     `json:"plan_id,omitempty"`
	TargetVersion  string     `json:"target_version,omitempty"`
	FailedVersion  string     `json:"failed_version,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
	NextCheckAt    *time.Time `json:"next_check_at,omitempty"`
}

func (m *Manager) automaticStatus() (AutomaticStatus, error) {
	var state AutomaticStatus
	if err := readJSON(filepath.Join(m.root, "automatic-state.json"), &state); err != nil && !os.IsNotExist(err) {
		return state, err
	}
	settings, err := m.loadAutomaticSettings()
	if err != nil {
		return state, err
	}
	state.Mode, state.DownloadPaused = settings.Mode, settings.DownloadPaused
	state.Enabled = settings.Mode == ModeAutomatic && !settings.DownloadPaused
	if settings.Mode == ModeManual && state.PlanID == "" && state.ManualPlanID == "" && !state.ManualDownload {
		state.Phase = "disabled"
	} else if state.Phase == "" {
		state.Phase = "idle"
	}
	if settings.DownloadPaused && state.Phase != "activating" && state.Phase != "restart_pending" {
		state.Phase = "download_paused"
	}
	return state, nil
}

func (m *Manager) saveAutomatic(ctx context.Context, state AutomaticStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return writeAtomicJSON(ctx, filepath.Join(m.root, "automatic-state.json"), state)
}

// AutomaticStep 每次只推进一个串行更新周期；调用者须绑定桌面实例生命周期，并实时提供空闲判断。
func (m *Manager) AutomaticStep(ctx context.Context, runningVersion string, idle func() bool, health HealthCheck, restart func(context.Context, string) error) error {
	return m.automaticStepAt(ctx, runningVersion, idle, health, restart, time.Now().UTC())
}

func (m *Manager) automaticStepAt(ctx context.Context, runningVersion string, idle func() bool, health HealthCheck, restart func(context.Context, string) error, now time.Time) error {
	lease, err := acquireUpdateLock(filepath.Join(m.root, ".automatic.lock"))
	if err != nil {
		return err
	}
	defer lease.Close()
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	state := status.Automatic
	pendingIntent := false
	for _, id := range status.PendingPlanIDs {
		if id == state.PlanID || id == state.ManualPlanID {
			pendingIntent = true
		}
	}
	if state.TargetVersion != "" && state.TargetVersion == runningVersion && !pendingIntent {
		state.PlanID, state.TargetVersion, state.Phase, state.LastError = "", "", "up_to_date", ""
		state.ManualDownload, state.ManualPlanID = false, ""
		next := now.Add(AutomaticCheckInterval)
		state.NextCheckAt = &next
		return m.saveAutomatic(ctx, state)
	}
	if status.RecoveryRequired || state.Mode == ModeManual && !state.ManualDownload && state.ManualPlanID == "" || state.DownloadPaused || !status.OnlineConfigured || restart == nil || idle == nil || health == nil {
		return nil
	}
	// 人工后台准备使用单独的计划字段；旧客户端不会把它误认成自动安装意图。
	if state.ManualPlanID != "" {
		for _, id := range status.PendingPlanIDs {
			if id == state.ManualPlanID {
				return nil
			}
		}
		state.ManualPlanID, state.TargetVersion, state.Phase = "", "", "idle"
		state.ManualDownload = false
		return m.saveAutomatic(ctx, state)
	}
	// 计划先于调度状态落盘；崩溃后只接管明确标记为自动创建的唯一计划。
	if state.PlanID == "" && len(status.PendingPlanIDs) == 1 {
		plan, err := m.LoadPlan(ctx, status.PendingPlanIDs[0])
		if err != nil {
			return err
		}
		if state.ManualDownload && !plan.Automatic && plan.Online != nil {
			state.ManualPlanID, state.TargetVersion, state.Phase = plan.ID, plan.Version, "waiting_install"
			state.ManualDownload = false
			return m.saveAutomatic(ctx, state)
		}
		if plan.Automatic {
			state.PlanID, state.TargetVersion, state.Phase = plan.ID, plan.Version, "waiting_idle"
			if state.Mode != ModeAutomatic {
				state.Phase = "waiting_install"
			}
			if err := m.saveAutomatic(ctx, state); err != nil {
				return err
			}
		}
	}
	if !(state.Phase == "available" && (state.Mode == ModeAutomatic || state.Mode == ModeDownload)) && state.NextCheckAt != nil && now.Before(*state.NextCheckAt) && (state.PlanID == "" || state.Phase == "failed" || state.Phase == "paused") && state.Phase != "restart_pending" {
		return nil
	}
	fail := func(cause error, reject bool) error {
		if errors.Is(cause, ErrDownloadPaused) || errors.Is(cause, errDownloadPolicyChanged) || errors.Is(cause, context.Canceled) {
			state.Phase = "download_paused"
			if errors.Is(cause, errDownloadPolicyChanged) {
				state.Phase = "available"
			}
			state.NextCheckAt = nil
			state.LastError = ""
			return m.saveAutomatic(context.WithoutCancel(ctx), state)
		}
		state.Phase, state.LastError = "failed", cause.Error()
		if errors.Is(cause, ErrOnlineUpdatesPaused) {
			state.Phase = "paused"
		}
		if reject {
			state.Phase = "blocked_version"
			state.FailedVersion = state.TargetVersion
			if state.PlanID != "" {
				if err := m.discardAutomaticPlan(context.WithoutCancel(ctx), state.PlanID); err != nil {
					cause = errors.Join(cause, err)
				} else {
					state.PlanID = ""
				}
			}
		}
		state.LastError = cause.Error()
		next := now.Add(automaticRetryInterval)
		state.NextCheckAt = &next
		if err := m.saveAutomatic(context.WithoutCancel(ctx), state); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	// 手动选择“稍后重启”的更新保持人工决定；只重启本调度器自己提交的版本。
	if status.State.ActiveVersion != runningVersion {
		if state.Mode != ModeAutomatic || state.ManualDownload || state.TargetVersion != status.State.ActiveVersion || (state.Phase != "restart_pending" && state.Phase != "activating") || !idle() {
			return nil
		}
		state.Phase = "restart_pending"
		if state.NextCheckAt != nil && now.Before(*state.NextCheckAt) {
			return nil
		}
		if err := restart(ctx, state.TargetVersion); err != nil {
			if errors.Is(err, ErrOnlineReleaseWithdrawn) || errors.Is(err, ErrOnlineUpdatesPaused) || errors.Is(err, ErrOnlinePolicyUnavailable) {
				if restoreErr := m.restorePendingStartup(context.WithoutCancel(ctx), state.TargetVersion, runningVersion, err.Error(), errors.Is(err, ErrOnlineReleaseWithdrawn)); restoreErr != nil {
					return errors.Join(err, restoreErr)
				}
				return fail(err, errors.Is(err, ErrOnlineReleaseWithdrawn))
			}
			state.LastError = err.Error()
			next := now.Add(automaticRetryInterval)
			state.NextCheckAt = &next
			return m.saveAutomatic(context.WithoutCancel(ctx), state)
		}
		next := now.Add(10 * time.Minute)
		state.NextCheckAt = &next
		return m.saveAutomatic(ctx, state)
	}
	if state.TargetVersion == runningVersion || state.TargetVersion == status.State.RejectedVersion {
		state.PlanID, state.TargetVersion = "", ""
	}
	if state.PlanID != "" && state.TargetVersion == state.FailedVersion {
		if err := m.discardAutomaticPlan(ctx, state.PlanID); err != nil {
			return err
		}
		state.PlanID = ""
		return m.saveAutomatic(ctx, state)
	}
	if state.PlanID == "" {
		// 人工离线预检和自动计划不能相互覆盖。
		if len(status.PendingPlanIDs) != 0 {
			return nil
		}
		state.Phase, state.LastError = "checking", ""
		if err := m.saveAutomatic(ctx, state); err != nil {
			return err
		}
		check, err := m.CheckOnline(ctx)
		if err != nil {
			return fail(err, false)
		}
		state.LastCheckedAt = &now
		next := now.Add(AutomaticCheckInterval)
		state.NextCheckAt = &next
		if check.UpdatesPaused {
			state.Phase, state.LastError = "paused", check.PauseReason
			return m.saveAutomatic(ctx, state)
		}
		if !check.Available || check.Release == nil {
			state.Phase = "up_to_date"
			state.TargetVersion = ""
			state.ManualDownload = false
			return m.saveAutomatic(ctx, state)
		}
		state.TargetVersion = check.Release.Version
		if state.TargetVersion == state.FailedVersion || state.TargetVersion == status.State.RejectedVersion {
			state.Phase = "blocked_version"
			state.ManualDownload = false
			return m.saveAutomatic(ctx, state)
		}
		if state.Mode == ModeNotify && !state.ManualDownload {
			state.Phase = "available"
			return m.saveAutomatic(ctx, state)
		}
		state.Phase = "downloading"
		if err := m.saveAutomatic(ctx, state); err != nil {
			return err
		}
		plan, _, err := m.prepareOnline(ctx, false, !state.ManualDownload)
		if err != nil {
			return fail(err, errors.Is(err, ErrInvalidOnlinePackage))
		}
		state.PlanID, state.TargetVersion, state.Phase = plan.ID, plan.Version, "waiting_idle"
		if state.ManualDownload {
			state.PlanID, state.ManualPlanID = "", plan.ID
			state.ManualDownload = false
			state.Phase = "waiting_install"
			return m.saveAutomatic(ctx, state)
		}
		if state.Mode != ModeAutomatic {
			state.Phase = "waiting_install"
		}
		if err := m.saveAutomatic(ctx, state); err != nil {
			return err
		}
	}
	latest, err := m.Status(ctx)
	if err != nil {
		return err
	}
	for _, id := range latest.PendingPlanIDs {
		if id != state.PlanID {
			state.Phase = "waiting_install"
			return m.saveAutomatic(ctx, state)
		}
	}
	if latest.Automatic.Mode != ModeAutomatic || latest.Automatic.DownloadPaused || !idle() {
		return nil
	}
	if state.FailedVersion == state.TargetVersion {
		return nil
	}
	state.Phase = "activating"
	state.NextCheckAt = nil
	if err := m.saveAutomatic(ctx, state); err != nil {
		return err
	}
	result, err := m.ActivateByID(ctx, state.PlanID, health)
	if err != nil {
		// 失败版本不循环执行，删除本自动周期的计划，后续新版本仍可更新。
		var pathError *os.PathError
		var spaceError *InsufficientSpaceError
		return fail(err, ctx.Err() == nil && !errors.Is(err, errUpdateBusy) && !errors.Is(err, ErrOnlineUpdatesPaused) && !errors.Is(err, ErrOnlinePolicyUnavailable) && !errors.Is(err, ErrDiskSpaceUnavailable) && !errors.As(err, &pathError) && !errors.As(err, &spaceError))
	}
	state.PlanID, state.TargetVersion, state.Phase = "", result.ActiveVersion, "restart_pending"
	state.NextCheckAt = nil
	if err := m.saveAutomatic(ctx, state); err != nil {
		return err
	}
	// 下载和预检期间用户可能重新打开网页，切换完成后仍要重新判断。
	latest, err = m.Status(ctx)
	if err != nil {
		return err
	}
	if latest.Automatic.Mode != ModeAutomatic || latest.Automatic.DownloadPaused || !idle() {
		return nil
	}
	err = restart(ctx, result.ActiveVersion)
	if err != nil {
		if errors.Is(err, ErrOnlineReleaseWithdrawn) || errors.Is(err, ErrOnlineUpdatesPaused) || errors.Is(err, ErrOnlinePolicyUnavailable) {
			if restoreErr := m.restorePendingStartup(context.WithoutCancel(ctx), result.ActiveVersion, runningVersion, err.Error(), errors.Is(err, ErrOnlineReleaseWithdrawn)); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
			return fail(err, errors.Is(err, ErrOnlineReleaseWithdrawn))
		}
		state.LastError = err.Error()
	}
	next := now.Add(10 * time.Minute)
	if err != nil {
		next = now.Add(automaticRetryInterval)
	}
	state.NextCheckAt = &next
	return errors.Join(err, m.saveAutomatic(context.WithoutCancel(ctx), state))
}

func (m *Manager) discardAutomaticPlan(ctx context.Context, id string) error {
	lease, err := acquireUpdateLock(filepath.Join(m.root, ".download.lock"))
	if err != nil {
		return err
	}
	defer lease.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	plan, err := m.LoadPlan(ctx, id)
	if err != nil {
		return err
	}
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	for _, path := range []string{plan.BundleRoot, plan.DownloadPath} {
		if path == "" {
			continue
		}
		if filepath.Dir(path) != filepath.Join(m.root, BundlesDirName) {
			continue
		}
		name := filepath.Base(path)
		if !generatedName(name, "plan-", "") && !generatedName(name, "download-", ".archive") {
			continue
		}
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return os.Remove(m.planPath(id))
}
