// 中文说明：仅在实际新版本通过启动健康检查后清理更新缓存，保留当前、回滚、安装基线和待确认计划；不触碰外部离线包。
package update

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type CleanupResult struct {
	RunningVersion string    `json:"running_version"`
	RemovedItems   int       `json:"removed_items"`
	FreedBytes     int64     `json:"freed_bytes"`
	Warnings       []string  `json:"warnings,omitempty"`
	CompletedAt    time.Time `json:"completed_at"`
}

// ConfirmRunning 只由已核验实际进程的启动器调用；预检、版本切换和旧后台都不能触发清理。
func (m *Manager) ConfirmRunning(ctx context.Context, runningVersion string) (CleanupResult, error) {
	if m == nil {
		return CleanupResult{}, errors.New("更新管理器未初始化")
	}
	lease, err := acquireUpdateLock(filepath.Join(m.root, ".download.lock"))
	if err != nil {
		return CleanupResult{}, err
	}
	defer lease.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return CleanupResult{}, err
	}
	defer lock.Close()
	if required, message := m.journalStatus(); required {
		return CleanupResult{}, fmt.Errorf("更新恢复完成前不清理：%s", message)
	}
	state, err := m.readState()
	if err != nil {
		return CleanupResult{}, err
	}
	if runningVersion == "" || state.ActiveVersion != runningVersion {
		return CleanupResult{}, fmt.Errorf("实际运行版本尚未确认，不清理更新文件")
	}
	if err := m.VerifyVersionRoot(ctx, filepath.Join(m.root, VersionDir, safeVersionName(runningVersion))); err != nil {
		return CleanupResult{}, err
	}
	protected := map[string]bool{}
	for _, version := range []string{state.ActiveVersion, state.PreviousVersion, state.InstalledVersion} {
		if version != "" {
			protected[filepath.Join(m.root, VersionDir, safeVersionName(version))] = true
		}
	}
	ids, err := m.listPlans()
	if err != nil {
		return CleanupResult{}, err
	}
	keepArchives := false
	for _, id := range ids {
		var plan Plan
		if err := readJSON(m.planPath(id), &plan); err != nil || plan.InstallRoot != m.root || plan.ID != id {
			return CleanupResult{}, fmt.Errorf("待确认计划无法核验，停止自动清理：%s", id)
		}
		protected[filepath.Clean(plan.BundleRoot)] = true
		if safeVersion(plan.Version) {
			protected[filepath.Join(m.root, VersionDir, safeVersionName(plan.Version))] = true
		}
		if plan.DownloadPath == "" {
			keepArchives = true // 旧版计划未登记对应下载包，保守保留压缩包。
		} else {
			protected[filepath.Clean(plan.DownloadPath)] = true
		}
	}
	result := CleanupResult{RunningVersion: runningVersion}
	for _, name := range []string{BundlesDirName, VersionDir} {
		root := filepath.Join(m.root, name)
		if _, err := safeExistingDirectory(root); err != nil {
			return result, err
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return result, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			path := filepath.Join(root, entry.Name())
			keep := protected[path]
			for target := range protected {
				if rel, err := filepath.Rel(path, target); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
					keep = true
				}
			}
			if keep || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			archive := name == BundlesDirName && entry.Type().IsRegular() && !keepArchives && generatedName(entry.Name(), "download-", ".archive")
			bundle := entry.IsDir() && (name == VersionDir && safeVersion(entry.Name()) && !strings.HasPrefix(entry.Name(), ".") || name == BundlesDirName && generatedName(entry.Name(), "plan-", ""))
			retired := generatedName(entry.Name(), ".cleanup-", "")
			if !archive && !bundle && !retired {
				continue
			}
			if bundle {
				manifest, _, err := m.readAndValidateManifest(ctx, path)
				if err != nil || name == VersionDir && manifest.Version != entry.Name() {
					result.Warnings = append(result.Warnings, "已保留无法核验的更新目录："+entry.Name())
					continue
				}
			}
			var size int64
			err = filepath.WalkDir(path, func(_ string, item fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				info, err := item.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() && !info.IsDir() {
					return fmt.Errorf("包含非普通文件")
				}
				if info.Mode().IsRegular() {
					size += info.Size()
				}
				return nil
			})
			if err != nil {
				result.Warnings = append(result.Warnings, "已保留无法安全清理的项目："+entry.Name())
				continue
			}
			// 先改名再删除；Windows 占用或中断导致的部分删除可在下次启动重试，不再依赖已删掉的清单。
			if !retired {
				id, err := newID("cleanup")
				if err != nil {
					return result, err
				}
				retiredPath := filepath.Join(root, "."+id)
				if err := os.Rename(path, retiredPath); err != nil {
					result.Warnings = append(result.Warnings, "暂未清理被占用的项目："+entry.Name())
					continue
				}
				path = retiredPath
			}
			if err := os.RemoveAll(path); err != nil {
				result.Warnings = append(result.Warnings, "暂未清理被占用的项目："+entry.Name())
				continue
			}
			result.RemovedItems++
			result.FreedBytes += size
		}
	}
	result.CompletedAt = time.Now().UTC()
	if err := writeAtomicJSON(ctx, filepath.Join(m.root, "last-cleanup.json"), result); err != nil {
		return result, err
	}
	return result, nil
}

func generatedName(name, prefix, suffix string) bool {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 12
}
