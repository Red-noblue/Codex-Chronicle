// 中文说明：按当前用户可用空间预检更新下载、展开、程序复制和数据快照，空间不足时保留当前版本。
package update

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

const updateSpaceReserve = 16 << 20

var ErrDiskSpaceUnavailable = errors.New("暂时无法检查更新磁盘空间")

type InsufficientSpaceError struct {
	Phase          string
	RequiredBytes  uint64
	AvailableBytes uint64
}

func (e *InsufficientSpaceError) Error() string {
	return fmt.Sprintf("%s所需磁盘空间不足：需要 %.1f MiB（含安全余量），可用 %.1f MiB", e.Phase, float64(e.RequiredBytes)/(1<<20), float64(e.AvailableBytes)/(1<<20))
}

// RequireDiskSpace 检查目标所在卷当前用户可用的空间；不能替代复制期间的实际写入错误检查。
func RequireDiskSpace(path string, bytes int64, phase string) error {
	return requireDiskSpace(path, bytes, phase, availableDiskSpace)
}

func requireDiskSpace(path string, bytes int64, phase string, available func(string) (uint64, error)) error {
	if bytes < 0 || bytes > math.MaxInt64-updateSpaceReserve {
		return errors.New("更新空间估算超过有效范围")
	}
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				path = filepath.Dir(path)
			}
			break
		}
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			return fmt.Errorf("%w：检查%s目标磁盘失败：%w", ErrDiskSpaceUnavailable, phase, err)
		}
		path = filepath.Dir(path)
	}
	free, err := available(path)
	if err != nil {
		return fmt.Errorf("%w：读取%s可用磁盘空间失败：%w", ErrDiskSpaceUnavailable, phase, err)
	}
	needed := uint64(bytes) + updateSpaceReserve
	if free < needed {
		return &InsufficientSpaceError{Phase: phase, RequiredBytes: needed, AvailableBytes: free}
	}
	return nil
}

func (m *Manager) requireDiskSpace(bytes int64, phase string) error {
	if m.diskSpaceCheck != nil {
		return m.diskSpaceCheck(m.root, bytes, phase)
	}
	return RequireDiskSpace(m.root, bytes, phase)
}
