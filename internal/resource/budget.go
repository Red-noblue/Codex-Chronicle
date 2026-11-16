// 中文说明：提供本项目派生数据和临时目录的资源预算检查；不统计或修改用户源 .codex 文件。
package resource

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	DefaultMemoryLimitBytes     int64 = 2 << 30
	DefaultPersistentLimitBytes int64 = 10 << 30
	DefaultTemporaryLimitBytes  int64 = 20 << 30
)

// Budget 是本项目派生数据的资源上限。0 表示使用对应默认值。
type Budget struct {
	MemoryLimitBytes     int64 `json:"memory_limit_bytes"`
	PersistentLimitBytes int64 `json:"persistent_limit_bytes"`
	TemporaryLimitBytes  int64 `json:"temporary_limit_bytes"`
}

// Paths 将应用生成的目录按持久空间和临时空间分组。
type Paths struct {
	Persistent []string `json:"persistent,omitempty"`
	Temporary  []string `json:"temporary,omitempty"`
}

// Snapshot 是一次非破坏性预算采样结果。
type Snapshot struct {
	MemoryBytes          int64    `json:"memory_bytes"`
	MemoryLimitBytes     int64    `json:"memory_limit_bytes"`
	PersistentBytes      int64    `json:"persistent_bytes"`
	PersistentLimitBytes int64    `json:"persistent_limit_bytes"`
	TemporaryBytes       int64    `json:"temporary_bytes"`
	TemporaryLimitBytes  int64    `json:"temporary_limit_bytes"`
	PersistentPaths      []string `json:"persistent_paths,omitempty"`
	TemporaryPaths       []string `json:"temporary_paths,omitempty"`
	WithinBudget         bool     `json:"within_budget"`
	Errors               []string `json:"errors,omitempty"`
}

// Normalize 补齐默认值并拒绝负数预算。
func (b Budget) Normalize() (Budget, error) {
	if b.MemoryLimitBytes == 0 {
		b.MemoryLimitBytes = DefaultMemoryLimitBytes
	}
	if b.PersistentLimitBytes == 0 {
		b.PersistentLimitBytes = DefaultPersistentLimitBytes
	}
	if b.TemporaryLimitBytes == 0 {
		b.TemporaryLimitBytes = DefaultTemporaryLimitBytes
	}
	if b.MemoryLimitBytes < 0 || b.PersistentLimitBytes < 0 || b.TemporaryLimitBytes < 0 {
		return Budget{}, fmt.Errorf("资源预算不能为负数")
	}
	return b, nil
}

// Inspect 只读取目录元数据和文件大小，不跟随符号链接，不读取正文。
func (b Budget) Inspect(ctx context.Context, paths Paths) Snapshot {
	normalized, err := b.Normalize()
	if err != nil {
		return Snapshot{WithinBudget: false, Errors: []string{err.Error()}}
	}
	snapshot := Snapshot{
		MemoryLimitBytes: normalized.MemoryLimitBytes, PersistentLimitBytes: normalized.PersistentLimitBytes,
		TemporaryLimitBytes: normalized.TemporaryLimitBytes,
		PersistentPaths:     uniquePaths(paths.Persistent), TemporaryPaths: uniquePaths(paths.Temporary),
		WithinBudget: true,
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	snapshot.MemoryBytes = int64(memory.HeapAlloc)
	if snapshot.MemoryLimitBytes > 0 && snapshot.MemoryBytes > snapshot.MemoryLimitBytes {
		snapshot.WithinBudget = false
		snapshot.Errors = append(snapshot.Errors, fmt.Sprintf("运行时内存已超过预算：%d > %d", snapshot.MemoryBytes, snapshot.MemoryLimitBytes))
	}
	for _, path := range snapshot.PersistentPaths {
		size, sizeErr := directoryBytes(ctx, path)
		snapshot.PersistentBytes += size
		if sizeErr != nil {
			snapshot.Errors = append(snapshot.Errors, "持久目录 "+path+"："+sizeErr.Error())
		}
	}
	for _, path := range snapshot.TemporaryPaths {
		size, sizeErr := directoryBytes(ctx, path)
		snapshot.TemporaryBytes += size
		if sizeErr != nil {
			snapshot.Errors = append(snapshot.Errors, "临时目录 "+path+"："+sizeErr.Error())
		}
	}
	if snapshot.PersistentLimitBytes > 0 && snapshot.PersistentBytes > snapshot.PersistentLimitBytes {
		snapshot.WithinBudget = false
		snapshot.Errors = append(snapshot.Errors, fmt.Sprintf("持久空间已超过预算：%d > %d", snapshot.PersistentBytes, snapshot.PersistentLimitBytes))
	}
	if snapshot.TemporaryLimitBytes > 0 && snapshot.TemporaryBytes > snapshot.TemporaryLimitBytes {
		snapshot.WithinBudget = false
		snapshot.Errors = append(snapshot.Errors, fmt.Sprintf("临时空间已超过预算：%d > %d", snapshot.TemporaryBytes, snapshot.TemporaryLimitBytes))
	}
	return snapshot
}

// Check 返回可直接展示给用户的超限错误；目录不存在视为空目录。
func (b Budget) Check(ctx context.Context, paths Paths) (Snapshot, error) {
	return b.CheckAdditional(ctx, paths, 0, 0)
}

// CheckAdditional 在当前目录用量之外预留即将写入的字节，供写操作执行前门禁使用。
func (b Budget) CheckAdditional(ctx context.Context, paths Paths, additionalPersistent, additionalTemporary int64) (Snapshot, error) {
	snapshot := b.Inspect(ctx, paths)
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	if additionalPersistent < 0 || additionalTemporary < 0 {
		return snapshot, fmt.Errorf("额外资源预算不能为负数")
	}
	if snapshot.PersistentLimitBytes > 0 && snapshot.PersistentBytes+additionalPersistent > snapshot.PersistentLimitBytes {
		snapshot.WithinBudget = false
		snapshot.Errors = append(snapshot.Errors, fmt.Sprintf("写入后持久空间将超过预算：%d > %d", snapshot.PersistentBytes+additionalPersistent, snapshot.PersistentLimitBytes))
	}
	if snapshot.TemporaryLimitBytes > 0 && snapshot.TemporaryBytes+additionalTemporary > snapshot.TemporaryLimitBytes {
		snapshot.WithinBudget = false
		snapshot.Errors = append(snapshot.Errors, fmt.Sprintf("写入后临时空间将超过预算：%d > %d", snapshot.TemporaryBytes+additionalTemporary, snapshot.TemporaryLimitBytes))
	}
	if !snapshot.WithinBudget {
		return snapshot, fmt.Errorf("资源预算检查未通过：%s", strings.Join(snapshot.Errors, "；"))
	}
	return snapshot, nil
}

func directoryBytes(ctx context.Context, root string) (int64, error) {
	if strings.TrimSpace(root) == "" || root == ":memory:" {
		return 0, nil
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("路径不能是符号链接")
	}
	if !info.IsDir() {
		return info.Size(), nil
	}
	var total int64
	err = filepath.WalkDir(absolute, func(path string, entry os.DirEntry, walkErr error) error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		total += entryInfo.Size()
		return nil
	})
	return total, err
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" || raw == ":memory:" {
			continue
		}
		absolute, err := filepath.Abs(raw)
		if err != nil {
			continue
		}
		absolute = filepath.Clean(absolute)
		if _, ok := seen[absolute]; ok {
			continue
		}
		seen[absolute] = struct{}{}
		result = append(result, absolute)
	}
	return result
}
