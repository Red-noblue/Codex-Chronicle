// 中文说明：汇总当前平台的只读能力探针，供 readiness、API 和工作台解释活动读取边界。
package platform

import (
	"os/exec"
	"runtime"
	"strings"
)

// CapabilityStatus 是平台能力的保守状态，不代表已经通过所有真实平台验收。
type CapabilityStatus string

const (
	CapabilityAvailable   CapabilityStatus = "available"
	CapabilityLimited     CapabilityStatus = "limited"
	CapabilityUnavailable CapabilityStatus = "unavailable"
	CapabilityUnverified  CapabilityStatus = "unverified"
)

// Capability 是单项平台能力的只读说明。
type Capability struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Status   CapabilityStatus `json:"status"`
	ReadOnly bool             `json:"read_only"`
	Reason   string           `json:"reason"`
}

// CapabilityReport 是当前 daemon 运行时的平台能力快照。
type CapabilityReport struct {
	OS           string       `json:"os"`
	Architecture string       `json:"architecture"`
	ReadOnly     bool         `json:"read_only"`
	Capabilities []Capability `json:"capabilities"`
}

// DetectCapabilities 只调用本机查询接口，不创建探针文件、不获取写锁，也不移动用户文件。
func DetectCapabilities() CapabilityReport {
	result := CapabilityReport{
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
		ReadOnly:     true,
		Capabilities: make([]Capability, 0, 6),
	}
	result.Capabilities = append(result.Capabilities,
		detectRecycleBinCapability(),
		detectProcessProbeCapability(),
		Capability{ID: "writer_lock_probe", Name: "Codex writer lock 读取", Status: CapabilityAvailable, ReadOnly: true, Reason: "可读取 .codex/thread-writer-locks 中的普通 .lock 文件；锁文件本身不能单独证明当前仍在写入"},
		Capability{ID: "unicode_paths", Name: "Unicode 路径读取", Status: CapabilityAvailable, ReadOnly: true, Reason: "Go 文件 API 支持当前平台的 Unicode 路径；仍需结合权限和文件系统实测"},
		detectLongPathCapability(),
		Capability{ID: "sqlite_wal_shm_read", Name: "SQLite WAL/SHM 只读读取", Status: CapabilityAvailable, ReadOnly: true, Reason: "使用隔离副本和 WAL 感知只读连接；不会把源 SHM 当作可发布快照"},
	)
	return result
}

func detectRecycleBinCapability() Capability {
	detection := DetectSystemTrash()
	status := CapabilityUnavailable
	if detection.Supported && detection.Exists && detection.IsDirectory {
		status = CapabilityAvailable
	} else if detection.Supported {
		status = CapabilityLimited
	}
	reason := strings.TrimSpace(detection.Reason)
	if reason == "" {
		reason = "未返回回收站探测原因"
	}
	return Capability{ID: "system_recycle_bin", Name: "系统回收站探测", Status: status, ReadOnly: true, Reason: reason}
}

func detectProcessProbeCapability() Capability {
	if runtime.GOOS == "windows" {
		return Capability{ID: "open_file_process_probe", Name: "打开文件进程探测", Status: CapabilityLimited, ReadOnly: true, Reason: "Windows 默认没有权威的打开文件路径探测器；活动副本按 conservative/unknown 处理"}
	}
	if _, err := exec.LookPath("lsof"); err == nil {
		return Capability{ID: "open_file_process_probe", Name: "打开文件进程探测", Status: CapabilityAvailable, ReadOnly: true, Reason: "可使用 lsof 做短窗口、只读的目标文件探测"}
	}
	return Capability{ID: "open_file_process_probe", Name: "打开文件进程探测", Status: CapabilityLimited, ReadOnly: true, Reason: "未找到 lsof；无法确认打开文件进程，稳定副本严格模式将保守阻断"}
}

func detectLongPathCapability() Capability {
	if runtime.GOOS == "windows" {
		return Capability{ID: "long_paths", Name: "长路径读取", Status: CapabilityUnverified, ReadOnly: true, Reason: "Windows 长路径取决于系统长路径策略和目标文件系统，当前运行时没有修改策略或创建探针"}
	}
	return Capability{ID: "long_paths", Name: "长路径读取", Status: CapabilityLimited, ReadOnly: true, Reason: "Unix 路径受文件系统和 PATH_MAX 限制，当前只提供安全拼接，仍需目标设备实测"}
}
