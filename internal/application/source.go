// 中文说明：解析相对于固定扫描根的只读源文件路径，阻止穿越和符号链接逃逸。
package application

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveReadOnlyPath 只允许访问 root 内的普通文件，并拒绝路径上的符号链接。
func ResolveReadOnlyPath(root, relative string) (string, error) {
	if strings.TrimSpace(relative) == "" {
		return "", fmt.Errorf("源文件路径不能为空")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("解析源根失败: %w", err)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("读取源根失败: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", fmt.Errorf("源根必须是普通目录")
	}
	relativePath := filepath.FromSlash(relative)
	if filepath.IsAbs(relativePath) {
		return "", fmt.Errorf("源文件路径必须是相对路径")
	}
	clean := filepath.Clean(relativePath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("源文件路径越出扫描根")
	}

	candidate := absoluteRoot
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		candidate = filepath.Join(candidate, part)
		info, statErr := os.Lstat(candidate)
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("源文件路径包含符号链接")
		}
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("源路径不是普通文件")
	}
	return candidate, nil
}
