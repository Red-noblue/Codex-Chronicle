// 中文说明：统一四种更新方式和可持久化的下载暂停；旧版开关保留兼容含义，人工下载不会被自动接管安装。
package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

type UpdateMode string

const (
	ModeAutomatic UpdateMode = "automatic"
	ModeDownload  UpdateMode = "download"
	ModeNotify    UpdateMode = "notify"
	ModeManual    UpdateMode = "manual"
)

var ErrInvalidUpdateMode = errors.New("请选择有效的更新方式")
var ErrDownloadPaused = errors.New("更新下载已暂停，已保留可续传数据")
var errDownloadPolicyChanged = errors.New("更新方式已改变，已停止自动下载")

type automaticSettings struct {
	Enabled        *bool      `json:"enabled,omitempty"`
	Mode           UpdateMode `json:"mode,omitempty"`
	DownloadPaused bool       `json:"download_paused,omitempty"`
}

func (s automaticSettings) allowsDownload() bool {
	return s.Mode == ModeAutomatic || s.Mode == ModeDownload
}
func (m *Manager) loadAutomaticSettings() (automaticSettings, error) {
	var s automaticSettings
	if err := readJSON(filepath.Join(m.root, "automatic-settings.json"), &s); err != nil && !os.IsNotExist(err) {
		return s, err
	}
	if s.Mode == "" {
		s.Mode = ModeAutomatic
		if s.Enabled != nil && !*s.Enabled {
			s.Mode = ModeManual
		}
	}
	if !validUpdateMode(s.Mode) {
		return s, ErrInvalidUpdateMode
	}
	return s, nil
}
func validUpdateMode(mode UpdateMode) bool {
	return mode == ModeAutomatic || mode == ModeDownload || mode == ModeNotify || mode == ModeManual
}
func (m *Manager) writeAutomaticSettings(ctx context.Context, s automaticSettings) error {
	// 回退到不认识 mode 的旧客户端时，下载/提醒/暂停均不能变成自动安装。
	enabled := s.Mode == ModeAutomatic && !s.DownloadPaused
	s.Enabled = &enabled
	return writeAtomicJSON(ctx, filepath.Join(m.root, "automatic-settings.json"), s)
}
func (m *Manager) SetMode(ctx context.Context, mode UpdateMode) error {
	if !validUpdateMode(mode) {
		return ErrInvalidUpdateMode
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.loadAutomaticSettings()
	if err != nil {
		return err
	}
	changed := s.Mode != mode
	s.Mode = mode
	if err = m.writeAutomaticSettings(ctx, s); err != nil {
		return err
	}
	if changed && m.downloadCancel == nil {
		var state AutomaticStatus
		if err := readJSON(filepath.Join(m.root, "automatic-state.json"), &state); err != nil && !os.IsNotExist(err) {
			return err
		}
		if state.Phase != "checking" && state.Phase != "downloading" && state.Phase != "activating" && state.Phase != "restart_pending" {
			state.NextCheckAt = nil
			if err := writeAtomicJSON(ctx, filepath.Join(m.root, "automatic-state.json"), state); err != nil {
				return err
			}
		}
	}
	if !s.allowsDownload() && m.downloadAutomatic && m.downloadCancel != nil {
		m.downloadCancel(errDownloadPolicyChanged)
	}
	return nil
}

// SetAutomatic 兼容旧客户端的总开关，不改变正在提交的恢复事务。
func (m *Manager) SetAutomatic(ctx context.Context, enabled bool) error {
	mode := ModeManual
	if enabled {
		mode = ModeAutomatic
	}
	return m.SetMode(ctx, mode)
}

// ControlDownload 只登记请求或中断传输；下载、预检仍由绑定后台生命周期的单一调度器推进。
func (m *Manager) ControlDownload(ctx context.Context, action string) error {
	if action != "pause" && action != "resume" && action != "retry" && action != "download" {
		return errors.New("不支持的下载操作")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	settings, err := m.loadAutomaticSettings()
	if err != nil {
		return err
	}
	var state AutomaticStatus
	if err = readJSON(filepath.Join(m.root, "automatic-state.json"), &state); err != nil && !os.IsNotExist(err) {
		return err
	}
	if action == "pause" {
		settings.DownloadPaused = true
		if err = m.writeAutomaticSettings(ctx, settings); err != nil {
			return err
		}
		if m.downloadCancel != nil {
			m.downloadCancel(ErrDownloadPaused)
		}
		return nil
	}
	if m.downloadCancel != nil && action != "resume" {
		return errUpdateBusy
	}
	if action == "download" && state.PlanID == "" && state.ManualPlanID == "" {
		pending, err := m.listPlans()
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			return errors.New("已有待确认的更新，请先处理该更新计划")
		}
	}
	if action != "resume" && (state.Phase == "checking" || state.Phase == "activating" || state.Phase == "restart_pending") {
		return errUpdateBusy
	}
	if state.FailedVersion != "" && state.FailedVersion == state.TargetVersion && action == "retry" {
		return errors.New("此版本未通过安全检查，请等待新版本或重新检查更新")
	}
	settings.DownloadPaused = false
	if err = m.writeAutomaticSettings(ctx, settings); err != nil {
		return err
	}
	if state.PlanID != "" || state.ManualPlanID != "" {
		return nil
	}
	if action == "download" || action == "retry" && (settings.Mode == ModeManual || settings.Mode == ModeNotify) {
		state.ManualDownload = true
	}
	state.NextCheckAt = nil
	if state.ManualDownload {
		state.Phase = "download_queued"
	}
	return writeAtomicJSON(ctx, filepath.Join(m.root, "automatic-state.json"), state)
}
func (m *Manager) beginDownload(ctx context.Context, automatic bool) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings, err := m.loadAutomaticSettings()
	if err != nil {
		return nil, nil, err
	}
	if settings.DownloadPaused {
		return nil, nil, ErrDownloadPaused
	}
	if automatic && !settings.allowsDownload() {
		return nil, nil, errDownloadPolicyChanged
	}
	child, cancel := context.WithCancelCause(ctx)
	m.downloadCancel, m.downloadAutomatic = cancel, automatic
	return child, func() { cancel(nil); m.mu.Lock(); m.downloadCancel = nil; m.downloadAutomatic = false; m.mu.Unlock() }, nil
}
