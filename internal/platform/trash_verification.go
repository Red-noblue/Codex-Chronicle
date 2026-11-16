// 中文说明：保存回收站探针和用户目视确认，位置身份只用于校验目的地，不授予任何对话写入权限。
package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

type TrashProbeRecord struct {
	ID             string    `json:"id"`
	SourceRoot     string    `json:"source_root"`
	DestinationKey string    `json:"destination_key"`
	FileName       string    `json:"file_name"`
	SHA256         string    `json:"sha256"`
	TrashPath      string    `json:"trash_path,omitempty"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
}

type TrashVerification struct {
	SourceRoot     string    `json:"source_root"`
	DestinationKey string    `json:"destination_key"`
	ProbeID        string    `json:"probe_id"`
	ConfirmedAt    time.Time `json:"confirmed_at"`
}

type ArchiveDestinationConfirmation struct {
	SourceRoot  string    `json:"source_root"`
	Path        string    `json:"path"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}

// LocationIdentity 返回卷标识和目录身份；不使用易随日常写入变化的 mtime。
func LocationIdentity(path string) (volume, identity string, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if runtime.GOOS == "windows" {
		volume = strings.ToLower(filepath.VolumeName(path))
		if volume == "" {
			return "", "", fmt.Errorf("不能确定文件所在磁盘")
		}
		return volume, strings.ToLower(filepath.Clean(path)), nil
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		device, inode := value.FieldByName("Dev"), value.FieldByName("Ino")
		if device.IsValid() && inode.IsValid() {
			volume = fmt.Sprint(device.Interface())
			return volume, volume + ":" + fmt.Sprint(inode.Interface()), nil
		}
	}
	return "", "", fmt.Errorf("当前平台不能确定目录的磁盘身份")
}
