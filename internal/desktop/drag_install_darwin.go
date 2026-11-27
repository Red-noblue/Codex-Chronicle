// 中文说明：阻止直接从只读磁盘镜像或转移卷启动 macOS 应用，用户拖入应用程序后再初始化后台。
package desktop

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DragInstallRequired 只检查完整应用入口所在卷；维护命令由调用方明确放行。
func DragInstallRequired(executable string) (bool, error) {
	directory := filepath.Dir(executable)
	contents := filepath.Dir(directory)
	if filepath.Base(directory) != "MacOS" || filepath.Base(contents) != "Contents" || filepath.Ext(filepath.Dir(contents)) != ".app" {
		return false, nil
	}
	var volume unix.Statfs_t
	if err := unix.Statfs(directory, &volume); err != nil {
		return false, err
	}
	return volume.Flags&unix.MNT_RDONLY != 0, nil
}
