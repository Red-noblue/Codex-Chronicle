// 中文说明：把临时目录、缓存和日志汇总为清理单元；目录选择在服务端展开，不受文件列表分页限制。
package maintenance

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"codex-chronicle.local/internal/domain"
)

// DirectorySummary 同时显示可进入预览和保持原位的文件；目录选择不会移动目录本身。
type DirectorySummary struct {
	RelativePath  string `json:"relative_path"`
	Category      string `json:"category"`
	FileCount     int    `json:"file_count"`
	SizeBytes     int64  `json:"size_bytes"`
	EligibleCount int    `json:"eligible_count"`
	EligibleBytes int64  `json:"eligible_bytes"`
	RetainedCount int    `json:"retained_count"`
	RetainedBytes int64  `json:"retained_bytes"`
}

// RetainedFile 说明目录中没有进入本次清理范围的文件。
type RetainedFile struct {
	RelativePath string `json:"relative_path"`
	Reason       string `json:"reason"`
}

// Selection 是明确选择展开后的固定文件范围；Retained 不会进入写入计划。
type Selection struct {
	Directories   []DirectorySummary `json:"directories"`
	Paths         []string           `json:"-"`
	FileCount     int                `json:"file_count"`
	SizeBytes     int64              `json:"size_bytes"`
	RetainedCount int                `json:"retained_count"`
	Retained      []RetainedFile     `json:"retained"`
}

func directoryCategory(relative string) string {
	switch strings.ToLower(path.Base(relative)) {
	case ".tmp", "tmp", "temp", ".temp":
		return "temporary"
	case ".cache", "cache", "caches":
		return "cache"
	case "log", "logs":
		return "log"
	}
	return ""
}

func directoryForFile(relative string) string {
	first, _, nested := strings.Cut(relative, "/")
	if nested && directoryCategory(first) != "" {
		return first
	}
	// Codex 插件安装缓存不在顶层，但仍是一个完整的维护单元。
	if strings.HasPrefix(relative, "plugins/cache/") {
		return "plugins/cache"
	}
	return ""
}

// BuildDirectories 统计扫描根下已识别的维护目录；会话、凭据和任意用户目录不会成为目录清理入口。
func BuildDirectories(report domain.InventoryReport) []DirectorySummary {
	groups := make(map[string]DirectorySummary)
	for _, file := range report.Files {
		directory := directoryForFile(file.RelativePath)
		if directory == "" {
			continue
		}
		group := groups[directory]
		group.RelativePath, group.Category = directory, directoryCategory(directory)
		group.FileCount++
		group.SizeBytes += file.SizeBytes
		candidate, ok := candidateForFile(file)
		if ok && candidate.Eligible && candidate.Action == ActionIsolate {
			group.EligibleCount++
			group.EligibleBytes += file.SizeBytes
		} else {
			group.RetainedCount++
			group.RetainedBytes += file.SizeBytes
		}
		groups[directory] = group
	}
	result := make([]DirectorySummary, 0, len(groups))
	for _, group := range groups {
		result = append(result, group)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SizeBytes != result[j].SizeBytes {
			return result[i].SizeBytes > result[j].SizeBytes
		}
		return result[i].RelativePath < result[j].RelativePath
	})
	return result
}

// ResolveSelection 将目录选择展开成完整候选集合，合并重复文件，并显式报告保留项；不读取或修改源内容。
func ResolveSelection(report domain.InventoryReport, paths, directories []string) (Selection, error) {
	selection := Selection{Directories: make([]DirectorySummary, 0), Paths: make([]string, 0), Retained: make([]RetainedFile, 0)}
	known := make(map[string]DirectorySummary)
	for _, directory := range BuildDirectories(report) {
		known[directory.RelativePath] = directory
	}
	selectedDirectories := make(map[string]bool)
	for _, directory := range directories {
		if directory != path.Clean(directory) || strings.ContainsAny(directory, "\\\x00") || directoryCategory(directory) == "" {
			return selection, fmt.Errorf("不支持清理目录 %q，请选择扫描结果中的临时、缓存或日志目录", directory)
		}
		group, ok := known[directory]
		if !ok {
			return selection, fmt.Errorf("目录 %s 不在当前扫描结果中，请重新检查", directory)
		}
		if !selectedDirectories[directory] {
			selection.Directories = append(selection.Directories, group)
			selectedDirectories[directory] = true
		}
	}
	selectedPaths := make(map[string]bool)
	for _, relative := range paths {
		selectedPaths[relative] = true
	}
	for _, file := range report.Files {
		if !selectedDirectories[directoryForFile(file.RelativePath)] {
			continue
		}
		candidate, ok := candidateForFile(file)
		if ok && candidate.Eligible && candidate.Action == ActionIsolate {
			selectedPaths[file.RelativePath] = true
			continue
		}
		selection.RetainedCount++
		if len(selection.Retained) < 20 {
			reason := candidate.BlockReason
			if reason == "" {
				reason = "不属于目录清理候选，保留原文件"
			}
			selection.Retained = append(selection.Retained, RetainedFile{RelativePath: file.RelativePath, Reason: reason})
		}
	}
	for relative := range selectedPaths {
		selection.Paths = append(selection.Paths, relative)
	}
	sort.Strings(selection.Paths)
	selection.FileCount = len(selection.Paths)
	for _, file := range report.Files {
		if selectedPaths[file.RelativePath] {
			selection.SizeBytes += file.SizeBytes
		}
	}
	return selection, nil
}
