// 中文说明：候选实际启动失败时按预期版本原子恢复旧指针，并抑制失败版本的自动重装。
package update

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

func (m *Manager) FailStartup(ctx context.Context, failedVersion, previousVersion, reason string) error {
	return m.restorePendingStartup(ctx, failedVersion, previousVersion, reason, true)
}

// PostponeStartup 在网络或发布暂停时恢复旧指针，保留后续重试机会，不把该版本标记为坏版本。
func (m *Manager) PostponeStartup(ctx context.Context, failedVersion, previousVersion, reason string) error {
	return m.restorePendingStartup(ctx, failedVersion, previousVersion, reason, false)
}

func (m *Manager) restorePendingStartup(ctx context.Context, failedVersion, previousVersion, reason string, reject bool) (err error) {
	finish := BeginDiagnostic(m.root, "restore_version", previousVersion)
	defer func() { finish(err) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	state, err := m.readState()
	if err != nil {
		return err
	}
	if state.ActiveVersion == previousVersion && state.RejectedVersion == failedVersion {
		return nil
	}
	if state.ActiveVersion != failedVersion || state.PreviousVersion != previousVersion || previousVersion == "" {
		return fmt.Errorf("更新指针发生变化，停止自动回退")
	}
	if err := m.VerifyVersionRoot(ctx, filepath.Join(m.root, VersionDir, safeVersionName(previousVersion))); err != nil {
		return err
	}
	state.ActiveVersion, state.PreviousVersion, state.RejectedVersion = previousVersion, "", failedVersion
	state.Online, state.DeferredVersion = nil, ""
	state.ConfirmedAt, state.CleanupAfter = nil, nil
	state.LastStatus, state.LastError, state.UpdatedAt = "startup_rolled_back", reason, time.Now().UTC()
	if !reject {
		state.RejectedVersion, state.DeferredVersion, state.LastStatus = "", failedVersion, "update_postponed"
	}
	return m.writeState(ctx, state)
}
