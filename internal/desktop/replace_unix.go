//go:build !windows

// 中文说明：使用系统原子重命名替换启动设置，避免写入中断留下半份配置。
package desktop

import "os"

func replaceStartupFile(source, target string) error { return os.Rename(source, target) }
