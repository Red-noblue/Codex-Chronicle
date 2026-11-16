// 中文说明：完整归档的独立管理入口；副本复制先校验后移除，恢复不覆盖已有会话，密码只在当前请求内存中使用。
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/archive"
)

type archiveManagementReview struct {
	ID               string          `json:"preview_id"`
	TaskID           string          `json:"task_id"`
	Action           string          `json:"action"`
	Source           string          `json:"source"`
	Code             string          `json:"code,omitempty"`
	ExpiresAt        time.Time       `json:"expires_at"`
	Restore          json.RawMessage `json:"restore,omitempty"`
	UpdateIfNewer    bool            `json:"update_if_newer,omitempty"`
	UpdateAvailable  bool            `json:"update_available,omitempty"`
	RecoverUpdate    bool            `json:"recover_update,omitempty"`
	RecoverAvailable bool            `json:"recover_available,omitempty"`
	UpdateReason     string          `json:"update_reason,omitempty"`
	UpdateBaseDigest string          `json:"-"`
	UpdateFromAt     time.Time       `json:"update_from_at,omitempty"`
	UpdateToAt       time.Time       `json:"update_to_at,omitempty"`
	SourceRoot       string          `json:"source_root"`
	Fingerprint      string          `json:"-"`
	Identity         string          `json:"-"`
	PackagePath      string          `json:"-"`
	ExportPath       string          `json:"-"`
	Digest           string          `json:"-"`
}

func cloudTaskRemoteCopy(task cloudTask) bool {
	return (task.RemoteCopy || !task.LocalCopy) && !task.RemoteDeleted
}

func (s *Server) archiveCenterHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	state, err := s.cloudBackups()
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/archive-center"), "/")
	if suffix == "" && r.Method == "GET" {
		state.mu.Lock()
		tasks := []cloudTask{}
		for _, task := range state.tasks {
			if task.SourceRoot == s.currentRoot() {
				tasks = append(tasks, task)
			}
		}
		active := state.active
		state.mu.Unlock()
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
		items := []map[string]any{}
		for _, task := range tasks {
			local := false
			if task.LocalPath != "" && filepath.Dir(task.LocalPath) == filepath.Clean(s.lifecycle.ArchiveRoot) {
				info, err := os.Lstat(filepath.Join(task.LocalPath, "manifest.json"))
				local = err == nil && info.Mode().IsRegular()
			}
			remote := cloudTaskRemoteCopy(task) && task.State == "completed"
			if task.State == "completed" && task.ManagementState == "completed" &&
				(strings.HasPrefix(task.ManagementAction, "delete_") || strings.HasPrefix(task.ManagementAction, "move_")) &&
				!local && !remote {
				continue
			}
			items = append(items, map[string]any{"task": task, "local_available": local, "remote_available": remote})
		}
		target, _, targetErr := s.cloudTarget(r.Context())
		message := ""
		if targetErr != nil {
			message = targetErr.Error()
		}
		writeJSON(w, 200, map[string]any{"items": items, "active": active, "local_root": s.lifecycle.ArchiveRoot, "remote_target": target, "remote_error": message})
		return
	}
	parts := strings.Split(suffix, "/")
	if r.Method != "POST" || len(parts) != 2 || !validCloudTaskID(parts[0]) {
		writeError(w, 404, "归档管理路径不存在")
		return
	}
	var input struct {
		Action        string `json:"action"`
		Source        string `json:"source"`
		Password      string `json:"archive_password"`
		UpdateIfNewer bool   `json:"update_if_newer"`
		RecoverUpdate bool   `json:"recover_update"`
		PreviewID     string `json:"preview_id"`
		Code          string `json:"confirmation_code"`
		Confirm       bool   `json:"confirm"`
	}
	if !decodeCloudInput(w, r, &input) {
		return
	}
	state.mu.Lock()
	task, ok := state.tasks[parts[0]]
	busy := state.active != ""
	state.mu.Unlock()
	if !ok || task.SourceRoot != s.currentRoot() {
		writeError(w, 404, "当前工作区没有这份归档")
		return
	}
	if busy {
		writeError(w, 409, "请先等待当前归档任务完成，或暂停任务")
		return
	}
	if parts[1] == "preview" {
		review, err := s.previewArchiveManagement(r.Context(), state, task, input.Action, input.Source, input.Password, input.UpdateIfNewer, input.RecoverUpdate)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		state.mu.Lock()
		if state.reviews == nil {
			state.reviews = map[string]archiveManagementReview{}
		}
		for id, p := range state.reviews {
			if time.Now().After(p.ExpiresAt) {
				delete(state.reviews, id)
			}
		}
		if len(state.reviews) >= 16 {
			state.mu.Unlock()
			writeError(w, 409, "待确认归档预览过多，请等待过期后重试")
			return
		}
		state.reviews[review.ID] = review
		state.mu.Unlock()
		writeJSON(w, 200, review)
		return
	}
	if parts[1] != "execute" {
		writeError(w, 404, "归档管理动作不存在")
		return
	}
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	state.mu.Lock()
	defer state.mu.Unlock()
	review, ok := state.reviews[input.PreviewID]
	if !ok || review.TaskID != task.ID || review.SourceRoot != s.currentRoot() || time.Now().After(review.ExpiresAt) || !input.Confirm || subtle.ConstantTimeCompare([]byte(input.Code), []byte(review.Code)) != 1 {
		writeError(w, 409, "归档确认不匹配或已过期，请重新查看影响")
		return
	}
	delete(state.reviews, input.PreviewID)
	if state.closed || state.active != "" || s.executionActive != "" {
		writeError(w, 409, "已有会话操作正在执行，请稍后重新查看影响")
		return
	}
	task.ManagementAction, task.ManagementState, task.ManagementError = review.Action, "running", ""
	if err := state.save(task); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	state.tasks[task.ID], state.active = task, task.ID
	ctx, cancel := context.WithCancel(context.Background())
	state.cancel, state.done = cancel, make(chan struct{})
	go func() {
		defer func() {
			cancel()
			state.mu.Lock()
			state.active = ""
			state.cancel = nil
			close(state.done)
			state.mu.Unlock()
		}()
		err := s.executeArchiveManagement(ctx, state, &task, review, input.Password)
		if err != nil {
			task.ManagementState, task.ManagementError = "needs_review", err.Error()
		} else {
			task.ManagementState = "completed"
		}
		if err := state.publish(&task); err != nil {
			state.mu.Lock()
			task.ManagementState, task.ManagementError = "needs_review", "无法保存归档操作回执，请人工核对"
			state.tasks[task.ID] = task
			state.mu.Unlock()
		}
	}()
	writeJSON(w, 202, task)
}

func (s *Server) archiveCopyFingerprint(ctx context.Context, task cloudTask, source, password string) (string, error) {
	if source == "local" {
		if task.LocalPath == "" || filepath.Dir(task.LocalPath) != filepath.Clean(s.lifecycle.ArchiveRoot) {
			return "", fmt.Errorf("没有本地副本，或归档目录已改变")
		}
		manifest, err := verifyArchivePackage(ctx, task.LocalPath, password)
		if err != nil {
			return "", err
		}
		if manifest.CloudBackup == nil || manifest.CloudBackup.SnapshotSHA256 != task.PreviewDigest {
			return "", fmt.Errorf("本地归档身份不匹配")
		}
		data, err := os.ReadFile(filepath.Join(task.LocalPath, "manifest.json"))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%x", sha256.Sum256(data)), nil
	}
	if source != "remote" || !cloudTaskRemoteCopy(task) {
		return "", fmt.Errorf("没有选定位置的归档副本")
	}
	_, identity, err := s.cloudTarget(ctx)
	if err != nil || identity != task.TargetIdentity {
		return "", fmt.Errorf("请先切换到这份归档使用的 GitHub 仓库与设置")
	}
	catalog := s.githubArchiveCatalog()
	if catalog == nil {
		return "", fmt.Errorf("当前仓库不支持归档清单")
	}
	items, err := catalog.ListArchives(ctx)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.ArchiveID == task.ArchiveID {
			if _, err := catalog.VerifyArchive(ctx, task.ArchiveID); err != nil {
				return "", err
			}
			return item.SourceManifestSHA256, nil
		}
	}
	return "", fmt.Errorf("云端副本已不存在，请刷新检查")
}

func (s *Server) previewArchiveManagement(ctx context.Context, state *cloudBackupState, task cloudTask, action, source, password string, updateIfNewer, recoverUpdate bool) (archiveManagementReview, error) {
	review := archiveManagementReview{ID: "archive-review-" + randomHex(12), TaskID: task.ID, Action: action, Source: source, ExpiresAt: time.Now().Add(5 * time.Minute), SourceRoot: s.currentRoot()}
	review.UpdateIfNewer = action == "restore" && updateIfNewer
	review.RecoverUpdate = action == "restore" && recoverUpdate
	valid := map[string]bool{"restore": true, "copy_local": true, "copy_remote": true, "move_local": true, "move_remote": true, "delete_local": true, "delete_remote": true}
	if !valid[action] {
		return review, fmt.Errorf("未知归档操作")
	}
	if strings.HasSuffix(action, "_local") {
		source = "remote"
		if action == "delete_local" {
			source = "local"
		}
	}
	if strings.HasSuffix(action, "_remote") {
		source = "local"
		if action == "delete_remote" {
			source = "remote"
		}
	}
	review.Source = source
	s.remoteConfigMu.RLock()
	defer s.remoteConfigMu.RUnlock()
	fingerprint, err := s.archiveCopyFingerprint(ctx, task, source, password)
	if err != nil {
		return review, err
	}
	review.Fingerprint = fingerprint
	if source == "remote" || strings.HasSuffix(action, "_remote") && action != "delete_remote" {
		_, review.Identity, err = s.cloudTarget(ctx)
		if err != nil {
			return review, err
		}
	}
	if action == "copy_local" || action == "move_local" {
		if err := validateLifecycleDestinations(s.currentRoot(), *s.lifecycle, operationPreviewRequest{Archive: true, ArchiveLocal: true}); err != nil {
			return review, err
		}
	}
	if strings.HasPrefix(action, "delete_") || strings.HasPrefix(action, "move_") {
		review.Code, err = randomConfirmationCode()
		if err != nil {
			return review, err
		}
	}
	if strings.HasPrefix(action, "delete_") {
		return review, nil
	}
	work := filepath.Join(state.root, "management-"+task.ID)
	if err := prepareExecutionDirectory(work); err != nil {
		return review, err
	}
	review.PackagePath = filepath.Join(work, "package-"+source)
	if source == "local" {
		if err := archive.CopyCloudBundle(ctx, task.LocalPath, review.PackagePath, password); err != nil {
			return review, err
		}
	} else {
		recovery := s.githubArchiveRecovery()
		if recovery == nil {
			return review, fmt.Errorf("远端不支持下载")
		}
		if _, err := os.Lstat(review.PackagePath); os.IsNotExist(err) {
			stage, err := os.MkdirTemp(work, ".download-*")
			if err != nil {
				return review, err
			}
			defer os.RemoveAll(stage)
			if _, err := recovery.DownloadArchive(ctx, task.ArchiveID, stage); err != nil {
				return review, err
			}
			if err := archive.CopyCloudBundle(ctx, filepath.Join(stage, task.ArchiveID), review.PackagePath, password); err != nil {
				return review, err
			}
		} else if err != nil {
			return review, err
		}
	}
	manifest, err := verifyArchivePackage(ctx, review.PackagePath, password)
	if err != nil {
		return review, err
	}
	if manifest.CloudBackup == nil || manifest.CloudBackup.SnapshotSHA256 != task.PreviewDigest {
		return review, fmt.Errorf("归档包身份不匹配")
	}
	if action != "restore" {
		return review, nil
	}
	if !s.executionAllowed(task.SourceRoot) {
		return review, fmt.Errorf("当前工作区未开放精确行恢复")
	}
	for _, id := range manifest.CloudBackup.ConversationIDs {
		if !s.lifecycle.AuthorizedLive.Allows(id) {
			return review, fmt.Errorf("归档含未授权会话")
		}
	}
	review.ExportPath = filepath.Join(work, "snapshot-"+source)
	if _, err := os.Lstat(review.ExportPath); os.IsNotExist(err) {
		if _, err := s.checkResourcesAdditional(ctx, 0, manifest.CloudBackup.OriginalBytes); err != nil {
			return review, err
		}
		if _, err := archive.ExtractCloudBundle(ctx, review.PackagePath, review.ExportPath, s.resourceBudgetSnapshot().TemporaryLimitBytes, password); err != nil {
			return review, err
		}
	} else if err != nil {
		return review, err
	} else if err := archive.VerifyCloudRecovery(ctx, review.ExportPath, *manifest.CloudBackup); err != nil {
		return review, err
	}
	args := []string{"--root", task.SourceRoot, "--socket", s.lifecycle.AuthorizedLive.Socket, "--destination", review.ExportPath}
	base, hasBase := latestRestoredArchiveBase(state, task)
	if updateIfNewer {
		if !hasBase || !task.CreatedAt.After(base.CreatedAt) {
			review.UpdateReason = "没有更晚且已验证的上次恢复版本；普通恢复仍会保留当前内容。"
		} else {
			review.UpdateBaseDigest = base.RestoreTargetDigest
			review.UpdateFromAt, review.UpdateToAt = base.CreatedAt, task.CreatedAt
			args = append(args, "--update-if-newer", "--expected-current-version-digest", base.RestoreTargetDigest)
		}
	}
	data, err := state.run(ctx, "restore-preview", filepath.Join(work, "restore-journal"), args...)
	if err != nil {
		return review, err
	}
	var result struct {
		Digest               string `json:"digest"`
		Status               string `json:"status"`
		CurrentVersionSHA256 string `json:"current_version_sha256"`
		UpdateReason         string `json:"update_reason"`
		RecoveryReason       string `json:"recovery_reason"`
	}
	if json.Unmarshal(data, &result) != nil || result.Digest == "" {
		return review, fmt.Errorf("恢复预览无效")
	}
	review.Restore, review.Digest = data, result.Digest
	if action == "restore" {
		if result.Status == "recovery_required" {
			review.RecoverAvailable = true
			review.UpdateReason = result.RecoveryReason
			if recoverUpdate {
				code, codeErr := randomConfirmationCode()
				if codeErr != nil {
					return review, codeErr
				}
				review.Code = code
			}
			return review, nil
		}
		if !updateIfNewer && result.Status == "conflict" {
			if hasBase && task.CreatedAt.After(base.CreatedAt) && result.CurrentVersionSHA256 == base.RestoreTargetDigest {
				review.UpdateAvailable = true
				review.UpdateFromAt, review.UpdateToAt = base.CreatedAt, task.CreatedAt
				review.UpdateBaseDigest = base.RestoreTargetDigest
			} else if !hasBase {
				review.UpdateReason = "当前版本没有已验证的恢复基线，不能安全覆盖。"
			} else if !task.CreatedAt.After(base.CreatedAt) {
				review.UpdateReason = "所选归档时间不晚于当前版本，不能作为更新目标。"
			} else {
				review.UpdateReason = "当前对话在上次恢复后发生了变化，不能覆盖。"
			}
		}
		if updateIfNewer {
			if result.Status != "update_ready" {
				if result.UpdateReason != "" {
					review.UpdateReason = result.UpdateReason
				}
				return review, nil
			}
			review.UpdateAvailable = true
			code, codeErr := randomConfirmationCode()
			if codeErr != nil {
				return review, codeErr
			}
			review.Code = code
		}
	}
	return review, nil
}

func latestRestoredArchiveBase(state *cloudBackupState, selected cloudTask) (cloudTask, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	var latest cloudTask
	found := false
	for _, candidate := range state.tasks {
		if candidate.ID == selected.ID || candidate.SourceRoot != selected.SourceRoot ||
			candidate.RestoreTargetDigest == "" || candidate.RestoredAt == nil ||
			!sameConversationIDs(candidate.SourceIDs, selected.SourceIDs) ||
			!(candidate.RestoreStatus == "restored" || candidate.RestoreStatus == "updated" ||
				candidate.RestoreStatus == "already_present") {
			continue
		}
		if !found || candidate.CreatedAt.After(latest.CreatedAt) {
			latest, found = candidate, true
		}
	}
	return latest, found
}

func sameConversationIDs(left, right []string) bool {
	if len(left) != len(right) || len(left) == 0 {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Server) executeArchiveManagement(ctx context.Context, state *cloudBackupState, task *cloudTask, review archiveManagementReview, password string) error {
	s.remoteConfigMu.RLock()
	defer s.remoteConfigMu.RUnlock()
	if review.SourceRoot != s.currentRoot() {
		return fmt.Errorf("工作区已变化，未继续执行")
	}
	fingerprint, err := s.archiveCopyFingerprint(ctx, *task, review.Source, password)
	if err != nil {
		return err
	}
	if fingerprint != review.Fingerprint {
		return fmt.Errorf("归档副本在预览后变化，未继续执行")
	}
	if review.Identity != "" {
		_, identity, err := s.cloudTarget(ctx)
		if err != nil || identity != review.Identity {
			return fmt.Errorf("归档仓库配置已变化")
		}
	}
	action := review.Action
	if action == "restore" {
		if !s.executionAllowed(task.SourceRoot) {
			return fmt.Errorf("恢复权限已变化")
		}
		work := filepath.Join(state.root, "management-"+task.ID, "restore-journal")
		args := []string{"--root", task.SourceRoot, "--socket", s.lifecycle.AuthorizedLive.Socket, "--destination", review.ExportPath, "--expected-digest", review.Digest}
		if review.UpdateIfNewer {
			if !review.UpdateAvailable || review.UpdateBaseDigest == "" || !task.CreatedAt.After(review.UpdateFromAt) {
				return fmt.Errorf("更新条件已失效，请重新查看影响")
			}
			args = append(args, "--update-if-newer", "--expected-current-version-digest", review.UpdateBaseDigest)
		}
		if review.RecoverUpdate {
			if !review.RecoverAvailable {
				return fmt.Errorf("没有可回滚的中断更新，请重新查看影响")
			}
			args = append(args, "--recover-update")
		}
		data, err := state.run(ctx, "restore-apply", work, args...)
		if err != nil {
			return err
		}
		var result struct {
			Status                 string `json:"status"`
			InstalledVersionSHA256 string `json:"installed_version_sha256"`
		}
		if json.Unmarshal(data, &result) != nil || result.Status != "update_rolled_back" && result.InstalledVersionSHA256 == "" {
			return fmt.Errorf("恢复结果无效")
		}
		task.RestoreStatus = result.Status
		task.RestoreSource = review.Source
		if result.InstalledVersionSHA256 != "" {
			task.RestoreTargetDigest = result.InstalledVersionSHA256
		}
		now := time.Now().UTC()
		if task.RestoreChecks == nil {
			task.RestoreChecks = make(map[string]cloudRestoreCheck)
		}
		task.RestoreChecks[review.Source] = cloudRestoreCheck{Status: result.Status, CheckedAt: now}
		if result.Status == "restored" || result.Status == "updated" || result.Status == "already_present" || result.Status == "update_rolled_back" {
			task.RestoredAt = &now
			s.tasks.InvalidateInventory(task.SourceRoot)
			s.tasks.StartInventoryWithOptions(task.SourceRoot, application.ScanOptions{})
		}
	} else if action == "copy_local" || action == "move_local" {
		if err := validateLifecycleDestinations(task.SourceRoot, *s.lifecycle, operationPreviewRequest{Archive: true, ArchiveLocal: true}); err != nil {
			return err
		}
		path := filepath.Join(s.lifecycle.ArchiveRoot, task.ID)
		if err := archive.CopyCloudBundle(ctx, review.PackagePath, path, password); err != nil {
			return err
		}
		task.LocalPath, task.LocalCopy = path, true
		if err := state.publish(task); err != nil {
			return err
		}
	} else if action == "copy_remote" || action == "move_remote" {
		provider := s.remoteArchiveProvider()
		if provider == nil {
			return fmt.Errorf("远端未连接")
		}
		if _, err := provider.UploadDirectory(ctx, review.PackagePath, task.ID); err != nil {
			return err
		}
		if _, err := provider.VerifyArchive(ctx, task.ID); err != nil {
			return err
		}
		task.Target, task.TargetIdentity, err = s.cloudTarget(ctx)
		if err != nil {
			return err
		}
		task.RemoteCopy, task.RemoteDeleted = true, false
		if err := state.publish(task); err != nil {
			return err
		}
	}
	if action == "delete_remote" || action == "move_local" {
		deletion := s.githubArchiveDeletion()
		if deletion == nil {
			return fmt.Errorf("远端不支持删除")
		}
		// 再核对来源，防止复制耗时期间仓库归档被其他客户端替换。
		value, err := s.archiveCopyFingerprint(ctx, *task, "remote", password)
		if err != nil {
			return err
		}
		if value != review.Fingerprint {
			return fmt.Errorf("云端副本已变化，保留两份副本")
		}
		if err := deletion.DeleteArchive(ctx, task.ID); err != nil {
			return err
		}
		task.RemoteDeleted = true
	}
	if action == "delete_local" || action == "move_remote" {
		value, err := s.archiveCopyFingerprint(ctx, *task, "local", password)
		if err != nil {
			return err
		}
		if value != review.Fingerprint {
			return fmt.Errorf("本地副本已变化，未删除")
		}
		if err := os.RemoveAll(task.LocalPath); err != nil {
			return fmt.Errorf("删除本地归档失败: %w", err)
		}
		task.LocalPath = ""
		task.LocalCopy = false
		// 本地单副本删除后不可被旧任务的默认远端兼容规则误识别为云端副本。
		if !task.RemoteCopy {
			task.RemoteDeleted = true
		}
	}
	// 成功后清理本轮自建明文和传输缓存；保留持久化回执及恢复 journal。
	for _, name := range []string{"package-local", "package-remote", "snapshot-local", "snapshot-remote"} {
		if err := os.RemoveAll(filepath.Join(state.root, "management-"+task.ID, name)); err != nil {
			return fmt.Errorf("操作完成，临时缓存清理失败: %w", err)
		}
	}
	return nil
}
