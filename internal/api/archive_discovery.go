// 中文说明：从实际归档位置重建只读清单，合并操作历史，并按仓库保存可在断网和重启后使用的清单缓存。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/durablefs"
)

const archiveCatalogTTL = 2 * time.Minute

type archiveRemoteEntry struct {
	Summary  archive.GitHubArchiveSummary `json:"summary"`
	Manifest *archive.Manifest            `json:"manifest,omitempty"`
	Error    string                       `json:"error,omitempty"`
}

type archiveRemoteSnapshot struct {
	Version     int                  `json:"version"`
	Identity    string               `json:"identity"`
	Target      string               `json:"target"`
	CheckedAt   time.Time            `json:"checked_at"`
	Entries     []archiveRemoteEntry `json:"entries"`
	AttemptedAt time.Time            `json:"-"`
	Error       string               `json:"-"`
	Binding     string               `json:"-"`
}

type archiveCatalogItem struct {
	Task            cloudTask `json:"task"`
	LocalAvailable  bool      `json:"local_available"`
	RemoteAvailable bool      `json:"remote_available"`
	RemoteStale     bool      `json:"remote_stale,omitempty"`
	Manageable      bool      `json:"manageable"`
	Discovered      bool      `json:"discovered,omitempty"`
	Notice          string    `json:"notice,omitempty"`
}

func (s *Server) archiveCenterList(w http.ResponseWriter, r *http.Request) {
	root := s.currentRoot()
	var tasks []cloudTask
	active := ""
	var changedAt time.Time
	issues := []string{}
	if state, err := s.cloudBackups(); state != nil {
		state.mu.Lock()
		for _, task := range state.tasks {
			if task.SourceRoot == root {
				tasks = append(tasks, task)
				if task.State == "completed" && task.ManagementState != "running" && task.UpdatedAt.After(changedAt) {
					changedAt = task.UpdatedAt
				}
			}
		}
		active = state.active
		state.mu.Unlock()
		if err != nil {
			issues = append(issues, "部分操作历史未能载入："+err.Error())
		}
	}
	localRoot := ""
	if s.lifecycle != nil {
		localRoot = s.lifecycle.ArchiveRoot
	}
	locals, localIssues := discoverLocalArchives(r.Context(), localRoot)
	issues = append(issues, localIssues...)
	remote, cacheIssue := s.discoverRemoteArchives(r.Context(), r.URL.Query().Get("refresh") == "1", changedAt)
	if cacheIssue != "" {
		issues = append(issues, cacheIssue)
	}
	items := mergeArchiveCatalog(tasks, locals, remote)
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "active": active, "local_root": localRoot, "catalog_errors": issues,
		"remote_target": remote.Target, "remote_error": remote.Error,
		"remote_checked_at": remote.CheckedAt, "remote_stale": remote.Error != "",
	})
}

func discoverLocalArchives(ctx context.Context, root string) ([]localArchiveCatalogItem, []string) {
	items := []localArchiveCatalogItem{}
	issues := []string{}
	if root == "" {
		return items, issues
	}
	root, err := filepath.Abs(root)
	resolved, resolveErr := canonicalLifecycleDestination(root)
	if err != nil || resolveErr != nil || pathHasCodexComponent(root) || pathHasCodexComponent(resolved) {
		return items, []string{"本地归档目录无效，不能位于 .codex 内"}
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return items, issues
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return items, []string{"本地归档目录无法读取，或不是普通目录"}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return items, []string{"读取本地归档目录失败：" + err.Error()}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			issues = append(issues, err.Error())
			break
		}
		// 不把上传临时目录和恢复 staging 当作用户保留的本地副本。
		if strings.HasPrefix(entry.Name(), ".") || !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		manifestPath := filepath.Join(path, "manifest.json")
		info, err := os.Lstat(manifestPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxLocalArchiveManifestBytes {
			issues = append(issues, "未读取不安全或过大的归档清单："+entry.Name())
			continue
		}
		item, err := readLocalArchiveCatalogItem(path, manifestPath, false, info.Size())
		if err != nil {
			issues = append(issues, err.Error())
			continue
		}
		items = append(items, item)
	}
	return items, issues
}

// 清单缓存的身份只取仓库、API 服务、分支和根路径；切换代理或凭证不会另建一份相同清单。
func (s *Server) archiveCatalogIdentity(target, fallback string) string {
	identity := fallback
	if s.githubSettings != nil && s.githubSettings.editable {
		settings := s.githubSettings.settings
		identity = strings.Join([]string{settings.APIBaseURL, settings.ArchiveRoot, settings.Branch, target}, "\n")
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

func (s *Server) archiveCatalogCachePath(identity string) string {
	if s.lifecycle == nil || s.lifecycle.JournalRoot == "" {
		return ""
	}
	path := filepath.Join(filepath.Dir(s.lifecycle.JournalRoot), "archive-catalog", identity+".json")
	canonical, err := canonicalLifecycleDestination(path)
	if err != nil || pathHasCodexComponent(path) || pathHasCodexComponent(canonical) || s.currentRoot() != "" && pathsOverlap(canonical, s.currentRoot()) {
		return ""
	}
	return path
}

func (s *Server) discoverRemoteArchives(ctx context.Context, force bool, changedAt time.Time) (archiveRemoteSnapshot, string) {
	s.remoteConfigMu.RLock()
	defer s.remoteConfigMu.RUnlock()
	s.archiveCatalogMu.Lock()
	defer s.archiveCatalogMu.Unlock()
	target, targetIdentity, err := s.cloudTarget(ctx)
	if err != nil {
		return archiveRemoteSnapshot{Target: target, Error: err.Error()}, ""
	}
	identity := s.archiveCatalogIdentity(target, targetIdentity)
	if s.archiveCatalogCache == nil {
		s.archiveCatalogCache = map[string]archiveRemoteSnapshot{}
	}
	cache, exists := s.archiveCatalogCache[identity]
	cachePath := s.archiveCatalogCachePath(identity)
	issue := ""
	if !exists && cachePath != "" {
		info, statErr := os.Lstat(cachePath)
		if statErr == nil {
			if info.Mode().IsRegular() && info.Size() <= maxLocalArchiveManifestBytes {
				data, readErr := os.ReadFile(cachePath)
				if readErr != nil || json.Unmarshal(data, &cache) != nil || cache.Version != 1 || cache.Identity != identity || cache.Target != target {
					cache = archiveRemoteSnapshot{}
					issue = "上次云端清单无法载入，已重新扫描"
				}
			} else {
				issue = "上次云端清单不是安全的普通文件，未载入"
			}
		} else if !os.IsNotExist(statErr) {
			issue = "无法读取上次云端清单：" + statErr.Error()
		}
	}
	if !force && !cache.AttemptedAt.IsZero() && !changedAt.After(cache.AttemptedAt) && time.Since(cache.AttemptedAt) < archiveCatalogTTL {
		return cache, issue
	}
	cache.Version, cache.Identity, cache.Target = 1, identity, target
	cache.Binding = targetIdentity
	cache.AttemptedAt = time.Now().UTC()
	catalog := s.githubArchiveCatalog()
	if catalog == nil {
		cache.Error = "当前远端不提供归档清单"
	} else {
		bounded, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		entries, err := catalog.ListArchives(bounded)
		if err != nil {
			cache.Error = "云端未核验：" + err.Error()
		} else {
			previous := map[string]archiveRemoteEntry{}
			for _, entry := range cache.Entries {
				previous[entry.Summary.ArchiveID] = entry
			}
			found := make([]archiveRemoteEntry, 0, len(entries))
			reader, canRead := catalog.(archive.RemoteArchiveManifestReader)
			for _, summary := range entries {
				entry := archiveRemoteEntry{Summary: summary}
				if old, ok := previous[summary.ArchiveID]; ok && old.Manifest != nil && old.Summary.SourceManifestSHA256 == summary.SourceManifestSHA256 {
					entry.Manifest = old.Manifest
				} else if canRead {
					manifest, err := reader.ReadArchiveManifest(bounded, summary.ArchiveID, summary.SourceManifestSHA256)
					if err != nil {
						entry.Error = err.Error()
					} else {
						entry.Manifest = &manifest
					}
				}
				found = append(found, entry)
			}
			cache.Entries, cache.CheckedAt, cache.Error = found, time.Now().UTC(), ""
			if cachePath != "" {
				if err := saveArchiveCatalog(cachePath, cache); err != nil {
					issue = "本次清单已读取，但无法保存供重启使用：" + err.Error()
				}
			}
		}
	}
	s.archiveCatalogCache[identity] = cache
	return cache, issue
}

func saveArchiveCatalog(path string, snapshot archiveRemoteSnapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(data) > int(maxLocalArchiveManifestBytes) {
		return fmt.Errorf("清单缓存超过 16 MiB，未写入磁盘")
	}
	if err := prepareExecutionDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".archive-catalog-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return durablefs.SyncDirectory(filepath.Dir(path))
}

func mergeArchiveCatalog(tasks []cloudTask, locals []localArchiveCatalogItem, remote archiveRemoteSnapshot) []archiveCatalogItem {
	items := []archiveCatalogItem{}
	usedLocal := map[int]bool{}
	usedRemote := map[int]bool{}
	for _, task := range tasks {
		item := archiveCatalogItem{Task: task, Manageable: true}
		localHash := ""
		for i, local := range locals {
			if !usedLocal[i] && local.OperationID == task.ArchiveID && local.Compression != nil && local.Compression.SnapshotSHA256 == task.PreviewDigest {
				item.LocalAvailable, usedLocal[i], localHash = true, true, local.ManifestSHA256
				if local.Path != task.LocalPath {
					item.Manageable, item.Notice = false, "本地副本位置已改变，操作记录尚未关联到新位置"
					item.Task.LocalPath = local.Path
				}
				item.Task.Metadata, item.Task.Encrypted = local.Compression, local.Encrypted
				break
			}
		}
		for i, entry := range remote.Entries {
			if usedRemote[i] || entry.Summary.ArchiveID != task.ArchiveID || task.Target != remote.Target {
				continue
			}
			manifest := entry.Manifest
			matches := manifest != nil && manifest.OperationID == task.ArchiveID && manifest.CloudBackup != nil && manifest.CloudBackup.SnapshotSHA256 == task.PreviewDigest
			if !matches && (localHash == "" || localHash != entry.Summary.SourceManifestSHA256) {
				continue
			}
			if localHash != "" && localHash != entry.Summary.SourceManifestSHA256 {
				continue // 同名但源清单不同的副本单独列出，不能称作双副本。
			}
			item.RemoteAvailable, item.RemoteStale, usedRemote[i] = true, remote.Error != "", true
			if item.Task.Metadata == nil && manifest != nil {
				item.Task.Metadata, item.Task.Encrypted = manifest.CloudBackup, manifest.Encrypted
			}
			if !cloudTaskRemoteCopy(task) || task.TargetIdentity != remote.Binding {
				item.Manageable, item.Notice = false, "重新发现了云端副本，原操作记录未关联此位置"
			}
			break
		}
		if task.State == "completed" && task.ManagementState == "completed" &&
			(strings.HasPrefix(task.ManagementAction, "delete_") || strings.HasPrefix(task.ManagementAction, "move_")) && !item.LocalAvailable && !item.RemoteAvailable {
			continue
		}
		items = append(items, item)
	}
	for i, local := range locals {
		if usedLocal[i] {
			continue
		}
		task := discoveredArchiveTask(local.OperationID, local.ManifestSHA256, local.CreatedAt, local.ConversationID, local.Compression)
		task.LocalPath, task.Encrypted = local.Path, local.Encrypted
		task.ID = "discovered-" + fmt.Sprintf("%x", sha256.Sum256([]byte(local.Path)))[:24]
		item := archiveCatalogItem{Task: task, LocalAvailable: true, Discovered: true, Notice: "已从归档目录发现；缺少当前工作区的操作记录，仅供查看"}
		for j, entry := range remote.Entries {
			if !usedRemote[j] && entry.Summary.ArchiveID == local.OperationID && entry.Summary.SourceManifestSHA256 == local.ManifestSHA256 {
				item.RemoteAvailable, item.RemoteStale, usedRemote[j] = true, remote.Error != "", true
				item.Task.Target = remote.Target
				break
			}
		}
		items = append(items, item)
	}
	for i, entry := range remote.Entries {
		if usedRemote[i] {
			continue
		}
		task := discoveredArchiveTask(entry.Summary.ArchiveID, entry.Summary.SourceManifestSHA256, entry.Summary.CreatedAt, "", nil)
		if manifest := entry.Manifest; manifest != nil {
			task = discoveredArchiveTask(entry.Summary.ArchiveID, entry.Summary.SourceManifestSHA256, manifest.CreatedAt, manifest.ConversationID, manifest.CloudBackup)
			task.Encrypted = manifest.Encrypted
		}
		task.Target = remote.Target
		item := archiveCatalogItem{Task: task, RemoteAvailable: true, RemoteStale: remote.Error != "", Discovered: true, Notice: "已从云端清单发现；来源工作区尚未关联，仅供查看"}
		if entry.Error != "" {
			item.Notice = "已发现云端条目，源清单尚未确认：" + entry.Error
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Task.CreatedAt.Equal(items[j].Task.CreatedAt) {
			return items[i].Task.ID < items[j].Task.ID
		}
		return items[i].Task.CreatedAt.After(items[j].Task.CreatedAt)
	})
	return items
}

func discoveredArchiveTask(id, hash string, created time.Time, conversationID string, metadata *archive.CloudBackupMetadata) cloudTask {
	ids := []string{}
	if metadata != nil {
		ids = metadata.ConversationIDs
	} else if conversationID != "" {
		ids = []string{conversationID}
	}
	task := cloudTask{ID: "discovered-" + fmt.Sprintf("%x", sha256.Sum256([]byte(id+"\n"+hash)))[:24], ArchiveID: id,
		State: "completed", SourceIDs: ids, Metadata: metadata, CreatedAt: created}
	if metadata != nil {
		task.PreviewDigest, task.SourceBytes = metadata.SnapshotSHA256, metadata.OriginalBytes
	}
	return task
}
