// 中文说明：三平台共用简洁更新状态和下载操作，通过实例私有通道访问后台，不让托盘轮询影响自动更新空闲计时。
package desktopui

import (
	"codex-chronicle.local/internal/desktop"
	updatepkg "codex-chronicle.local/internal/update"
	"context"
	"fmt"
	"time"
)

type updateSummary struct {
	Label      string `json:"label"`
	Paused     bool   `json:"paused"`
	CanControl bool   `json:"can_control"`
}

func describeUpdate(s updatepkg.Status) updateSummary {
	result := updateSummary{Paused: s.Automatic.DownloadPaused, CanControl: s.Download != nil || s.Automatic.DownloadPaused || s.Automatic.Mode == updatepkg.ModeAutomatic || s.Automatic.Mode == updatepkg.ModeDownload}
	if result.Paused {
		result.Label = "更新下载已暂停"
		return result
	}
	if s.Download != nil && s.Download.TotalBytes > 0 {
		percent := s.Download.ReceivedBytes * 100 / s.Download.TotalBytes
		if percent > 100 {
			percent = 100
		}
		result.Label = fmt.Sprintf("更新下载中 · %d%%", percent)
		return result
	}
	labels := map[string]string{"available": "发现新版本", "download_queued": "等待后台下载", "waiting_idle": "更新已准备好，等待空闲", "waiting_install": "更新已下载，等待安装", "activating": "正在安装更新", "restart_pending": "更新等待重启", "checking": "正在检查更新", "failed": "更新未完成，可重试", "blocked_version": "已跳过不可用更新", "paused": "发布方已暂停更新", "download_paused": "下载已暂停，可继续"}
	result.Label = labels[s.Automatic.Phase]
	if result.Label == "" {
		switch s.Automatic.Mode {
		case updatepkg.ModeManual:
			result.Label = "手动检查更新"
		case updatepkg.ModeNotify:
			result.Label = "新版本提醒已开启"
		default:
			result.Label = "后台更新已开启"
		}
	}
	return result
}
func (c trayController) updateState() updateSummary {
	if c.url == "" {
		return updateSummary{Label: "更新状态暂不可用"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	instance, err := desktop.Running(ctx, c.dataDir)
	if err != nil {
		return updateSummary{Label: "更新状态暂不可用"}
	}
	status, err := instance.UpdateStatus(ctx)
	if err != nil {
		return updateSummary{Label: "更新状态暂不可用"}
	}
	return describeUpdate(status)
}
func (c trayController) toggleDownload() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	instance, err := desktop.Running(ctx, c.dataDir)
	if err != nil {
		return err
	}
	status, err := instance.UpdateStatus(ctx)
	if err != nil {
		return err
	}
	action := "pause"
	if status.Automatic.DownloadPaused {
		action = "resume"
	}
	return instance.ControlUpdateDownload(ctx, action)
}
func (c trayController) openUpdates() error {
	return desktop.OpenBrowser(c.url + "/settings/system#setting-updates")
}
