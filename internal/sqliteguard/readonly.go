// 中文说明：集中构造不会写入源文件的 SQLite immutable 只读 URI，并记录旁路文件证据。
package sqliteguard

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// ReadOnlyDSN 返回只读 immutable URI 和打开时已存在的 WAL/SHM 旁路文件。
// immutable=1 会禁止 SQLite 创建锁、WAL 或 SHM 文件；旁路文件存在时，调用方必须
// 明确提示该视图可能没有读取 WAL 中尚未合并到主库的最新提交。
func ReadOnlyDSN(path string, busyTimeoutMS int) (string, []string, error) {
	if busyTimeoutMS < 0 {
		return "", nil, fmt.Errorf("SQLite busy_timeout 不能为负数")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("解析 SQLite 路径失败: %w", err)
	}
	auxiliary := existingAuxiliaryFiles(abs)
	escapedPath := (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()
	dsn := "file:" + escapedPath + "?mode=ro&immutable=1&_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMS) + ")"
	return dsn, auxiliary, nil
}

// LiveReadOnlyDSN 返回不使用 immutable 的 WAL 感知只读 URI。
// 调用方必须在只读事务中使用该连接；它不会执行 checkpoint、写入 WAL 或创建源目录文件。
func LiveReadOnlyDSN(path string, busyTimeoutMS int) (string, []string, error) {
	if busyTimeoutMS < 0 {
		return "", nil, fmt.Errorf("SQLite busy_timeout 不能为负数")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("解析 SQLite 路径失败: %w", err)
	}
	auxiliary := existingAuxiliaryFiles(abs)
	escapedPath := (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()
	dsn := "file:" + escapedPath + "?mode=ro&_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMS) + ")"
	return dsn, auxiliary, nil
}

// ExistingAuxiliaryFiles 返回当前已经存在的 WAL/SHM 旁路文件，不会创建它们。
func ExistingAuxiliaryFiles(path string) []string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	return existingAuxiliaryFiles(abs)
}

func existingAuxiliaryFiles(path string) []string {
	files := make([]string, 0, 2)
	for _, suffix := range []string{"-wal", "-shm"} {
		candidate := path + suffix
		if _, err := os.Lstat(candidate); err == nil {
			files = append(files, candidate)
		}
	}
	return files
}
