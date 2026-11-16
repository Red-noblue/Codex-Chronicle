// 中文说明：验证平台能力报告只读、状态明确，并覆盖跨平台探针的保守边界。
package platform

import (
	"runtime"
	"testing"
)

func TestDetectCapabilitiesIsReadOnlyAndHasStableIDs(t *testing.T) {
	report := DetectCapabilities()
	if !report.ReadOnly || report.OS != runtime.GOOS || report.Architecture != runtime.GOARCH {
		t.Fatalf("平台能力报告边界错误：%+v", report)
	}
	wanted := map[string]bool{
		"system_recycle_bin":      false,
		"open_file_process_probe": false,
		"writer_lock_probe":       false,
		"unicode_paths":           false,
		"long_paths":              false,
		"sqlite_wal_shm_read":     false,
	}
	for _, capability := range report.Capabilities {
		if _, ok := wanted[capability.ID]; !ok {
			t.Fatalf("平台能力报告出现未约定 ID：%+v", capability)
		}
		if wanted[capability.ID] {
			t.Fatalf("平台能力 ID 重复：%s", capability.ID)
		}
		wanted[capability.ID] = true
		if capability.Name == "" || capability.Reason == "" || !capability.ReadOnly {
			t.Fatalf("平台能力缺少可解释只读证据：%+v", capability)
		}
		switch capability.Status {
		case CapabilityAvailable, CapabilityLimited, CapabilityUnavailable, CapabilityUnverified:
		default:
			t.Fatalf("平台能力状态不稳定：%+v", capability)
		}
	}
	for id, found := range wanted {
		if !found {
			t.Fatalf("平台能力报告缺少 %s", id)
		}
	}
}
