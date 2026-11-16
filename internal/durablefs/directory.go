// 中文说明：统一目录同步；Windows 不支持 Go 目录句柄的 fsync，保留文件级 Sync，不承诺 Unix 目录持久性。
package durablefs

import (
	"fmt"
	"os"
	"runtime"
)

func SyncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("同步目标不是目录: %s", path)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	return file.Sync()
}
