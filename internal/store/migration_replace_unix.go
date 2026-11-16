// 中文说明：在 Unix 平台为 archive.db 恢复提供同目录原子替换和目录持久化。
//go:build !windows

package store

import "os"

func replaceDatabaseFile(source, destination string) error {
	return os.Rename(source, destination)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
