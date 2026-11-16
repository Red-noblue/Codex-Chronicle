// 中文说明：检测当前设备的系统回收站候选路径和只读能力，不创建探针文件或移动用户文件。
package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// TrashDetection 是 UI 展示的回收站探测结果。
type TrashDetection struct {
	OS            string `json:"os"`
	Architecture  string `json:"architecture"`
	CandidatePath string `json:"candidate_path,omitempty"`
	DefaultPath   string `json:"default_path,omitempty"`
	SettingsPath  string `json:"settings_path,omitempty"`
	Configured    bool   `json:"configured"`
	Exists        bool   `json:"exists"`
	IsDirectory   bool   `json:"is_directory"`
	Writable      bool   `json:"writable"`
	Supported     bool   `json:"supported"`
	ReadOnly      bool   `json:"read_only"`
	Reason        string `json:"reason"`
}

// DetectSystemTrash 使用当前用户主目录和平台约定路径做无写入探测。
func DetectSystemTrash() TrashDetection {
	home, err := os.UserHomeDir()
	if err != nil {
		return TrashDetection{OS: runtime.GOOS, Architecture: runtime.GOARCH, ReadOnly: true, Reason: "无法读取当前用户主目录"}
	}
	return DetectTrashFor(runtime.GOOS, runtime.GOARCH, home)
}

// DetectTrashFor 允许测试和 UI 使用显式平台/主目录进行确定性探测。
func DetectTrashFor(goos, goarch, home string) TrashDetection {
	result := TrashDetection{OS: goos, Architecture: goarch, ReadOnly: true}
	home = strings.TrimSpace(home)
	if home == "" {
		result.Reason = "用户主目录为空"
		return result
	}
	var candidate string
	switch goos {
	case "darwin":
		candidate = filepath.Join(home, ".Trash")
	case "linux", "freebsd", "openbsd", "netbsd":
		dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
		if dataHome == "" {
			dataHome = filepath.Join(home, ".local", "share")
		}
		candidate = filepath.Join(dataHome, "Trash", "files")
	case "windows":
		// Windows 回收站位于每个卷的根目录；保留无卷名测试路径的兼容候选。
		candidate = filepath.Join(home, "Recycle.Bin")
		if volume := filepath.VolumeName(home); volume != "" {
			candidate = filepath.Join(volume+string(filepath.Separator), "$Recycle.Bin")
		}
	default:
		result.Reason = "该平台没有内置回收站路径约定"
		return result
	}
	result.CandidatePath = candidate
	info, err := os.Stat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			result.Reason = "候选回收站目录不存在，未执行创建或修复"
		} else {
			result.Reason = "候选回收站目录不可读取: " + err.Error()
		}
		return result
	}
	result.Exists = true
	result.IsDirectory = info.IsDir()
	result.Writable = info.IsDir() && info.Mode().Perm()&0o200 != 0
	if !result.IsDirectory {
		result.Reason = "候选路径不是目录"
		return result
	}
	if !result.Writable {
		result.Reason = "候选目录权限不显示为可写"
		return result
	}
	if goos == "windows" {
		result.Supported = true
		result.Reason = "已发现 Windows 回收站候选目录；执行时使用原生 Shell provider"
		return result
	}
	result.Supported = true
	result.Reason = "已发现可探测的系统回收站候选目录；移动前仍需原生 provider 验证"
	return result
}
