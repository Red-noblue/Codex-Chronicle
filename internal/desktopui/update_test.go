// 中文说明：保证三平台共用的更新摘要简洁一致，暂停状态优先显示，进度不会超过100%。
package desktopui

import (
	updatepkg "codex-chronicle.local/internal/update"
	"strings"
	"testing"
)

func TestUpdateSummaryUsesSharedModePauseAndProgress(t *testing.T) {
	s := updatepkg.Status{Automatic: updatepkg.AutomaticStatus{Mode: updatepkg.ModeDownload, Phase: "waiting_install"}}
	summary := describeUpdate(s)
	if !summary.CanControl || !strings.Contains(summary.Label, "等待安装") {
		t.Fatal(summary)
	}
	s.Download = &updatepkg.DownloadProgress{ReceivedBytes: 150, TotalBytes: 100}
	summary = describeUpdate(s)
	if !strings.Contains(summary.Label, "100%") {
		t.Fatal(summary)
	}
	s.Automatic.DownloadPaused = true
	summary = describeUpdate(s)
	if !summary.Paused || !strings.Contains(summary.Label, "已暂停") {
		t.Fatal(summary)
	}
	s.Download = nil
	s.Automatic = updatepkg.AutomaticStatus{Mode: updatepkg.ModeNotify, Phase: "available"}
	summary = describeUpdate(s)
	if summary.CanControl || summary.Label != "发现新版本" {
		t.Fatal(summary)
	}
}
