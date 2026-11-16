// 中文说明：会话库云备份的只读资产预览、显式上传确认、持久任务与隔离下载；源对话始终保留。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/compat/authorizedlive"
	"codex-chronicle.local/internal/durablefs"
)

type cloudAsset struct {
	Relative string `json:"relative"`
	ID       string `json:"id"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

func randomHex(size int) string {
	data := make([]byte, size)
	// Go 的 crypto/rand.Read 在系统随机源失效时会终止进程，不会产生弱随机 ID。
	rand.Read(data)
	return hex.EncodeToString(data)
}

type cloudPreview struct {
	ID           string       `json:"id"`
	Digest       string       `json:"digest"`
	IDs          []string     `json:"ids"`
	RequestedIDs []string     `json:"requested_ids"`
	Assets       []cloudAsset `json:"assets"`
	SourceBytes  int64        `json:"source_bytes"`
	DatabaseRows []struct {
		Database string `json:"database"`
		Table    string `json:"table"`
		Rows     int    `json:"rows"`
	} `json:"database_rows"`
	IndexLines     map[string]int `json:"index_lines"`
	Target         string         `json:"target"`
	SourceRoot     string         `json:"source_root"`
	ExpiresAt      time.Time      `json:"expires_at"`
	SourceReadOnly bool           `json:"source_read_only"`
	identity       string
}

type cloudTask struct {
	ID                  string                       `json:"id"`
	State               string                       `json:"state"`
	ArchiveID           string                       `json:"archive_id"`
	SourceRoot          string                       `json:"source_root"`
	SourceIDs           []string                     `json:"source_ids"`
	Target              string                       `json:"target"`
	TargetIdentity      string                       `json:"target_identity"`
	PreviewDigest       string                       `json:"preview_digest"`
	CreatedAt           time.Time                    `json:"created_at"`
	UpdatedAt           time.Time                    `json:"updated_at"`
	CompletedBytes      int64                        `json:"completed_bytes"`
	TotalBytes          int64                        `json:"total_bytes"`
	SourceBytes         int64                        `json:"source_bytes"`
	Metadata            *archive.CloudBackupMetadata `json:"compression,omitempty"`
	Error               string                       `json:"error,omitempty"`
	CacheRetained       bool                         `json:"cache_retained"`
	DownloadState       string                       `json:"download_state,omitempty"`
	DownloadError       string                       `json:"download_error,omitempty"`
	RecoveryPath        string                       `json:"recovery_path,omitempty"`
	LocalCopy           bool                         `json:"local_copy,omitempty"`
	RemoteCopy          bool                         `json:"remote_copy,omitempty"`
	Encrypted           bool                         `json:"encrypted,omitempty"`
	LocalPath           string                       `json:"local_path,omitempty"`
	Titles              []string                     `json:"titles,omitempty"`
	RemoteDeleted       bool                         `json:"remote_deleted,omitempty"`
	ManagementState     string                       `json:"management_state,omitempty"`
	ManagementAction    string                       `json:"management_action,omitempty"`
	ManagementError     string                       `json:"management_error,omitempty"`
	RestoreStatus       string                       `json:"restore_status,omitempty"`
	RestoreSource       string                       `json:"restore_source,omitempty"`
	RestoreTargetDigest string                       `json:"restore_target_digest,omitempty"`
	RestoredAt          *time.Time                   `json:"restored_at,omitempty"`
	RestoreChecks       map[string]cloudRestoreCheck `json:"restore_checks,omitempty"`
}

// cloudRestoreCheck 记录某一归档位置最近一次与当前工作区比较或恢复的结果。
type cloudRestoreCheck struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checked_at"`
}

type cloudBackupState struct {
	mu        sync.Mutex
	root      string
	previews  map[string]cloudPreview
	reviews   map[string]archiveManagementReview
	tasks     map[string]cloudTask
	active    string
	cancel    context.CancelFunc
	done      chan struct{}
	closed    bool
	loadError error
	run       func(context.Context, string, string, ...string) (json.RawMessage, error)
}

func (s *Server) cloudBackupSupported() bool {
	return s.lifecycle != nil && s.lifecycle.AuthorizedLive != nil && s.lifecycle.AuthorizedLive.Python != "" && s.lifecycle.RemoteRecoveryRoot != ""
}

func (s *Server) cloudBackups() (*cloudBackupState, error) {
	s.cloudMu.Lock()
	defer s.cloudMu.Unlock()
	if s.cloud != nil {
		return s.cloud, s.cloud.loadError
	}
	if !s.cloudBackupSupported() {
		return nil, fmt.Errorf("当前服务未配置完整会话快照兼容层与隔离下载目录")
	}
	root := filepath.Join(filepath.Dir(s.lifecycle.JournalRoot), "cloud-backups")
	if s.lifecycle.JournalRoot == "" || pathsOverlap(root, s.currentRoot()) || pathHasCodexComponent(root) {
		return nil, fmt.Errorf("云备份任务根必须独立于源目录")
	}
	if err := prepareExecutionDirectory(root); err != nil {
		return nil, err
	}
	state := &cloudBackupState{root: root, previews: map[string]cloudPreview{}, tasks: map[string]cloudTask{}, run: s.lifecycle.AuthorizedLive.Run}
	s.cloud = state
	entries, err := os.ReadDir(root)
	if err != nil {
		state.loadError = err
		return state, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		var task cloudTask
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			state.loadError = fmt.Errorf("云备份任务记录异常: %s", entry.Name())
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &task) != nil || !validCloudTaskID(task.ID) || entry.Name() != task.ID+".json" || task.ArchiveID != task.ID {
			state.loadError = fmt.Errorf("云备份任务记录损坏: %s", entry.Name())
			continue
		}
		switch task.State {
		case "queued", "exporting", "compressing", "uploading", "verifying":
			task.State = "interrupted"
			task.Error = "服务曾中断，点击继续上传可复用已完成分片"
		}
		if task.DownloadState == "running" {
			task.DownloadState = "interrupted"
			task.DownloadError = "下载曾中断，可重新下载"
		}
		if task.ManagementState == "running" {
			task.ManagementState = "needs_review"
			task.ManagementError = "管理操作曾中断，请重新查看影响；不会自动恢复或删除"
		}
		state.tasks[task.ID] = task
		if err := state.save(task); err != nil {
			state.loadError = err
		}
	}
	return state, state.loadError
}

func validCloudTaskID(id string) bool {
	if !strings.HasPrefix(id, "backup-") || len(id) != 31 {
		return false
	}
	for _, c := range strings.TrimPrefix(id, "backup-") {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func (state *cloudBackupState) save(task cloudTask) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(state.root, ".cloud-task-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), filepath.Join(state.root, task.ID+".json")); err != nil {
		return err
	}
	return durablefs.SyncDirectory(state.root)
}

func (state *cloudBackupState) publish(task *cloudTask) error {
	task.UpdatedAt = time.Now().UTC()
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.save(*task); err != nil {
		return err
	}
	state.tasks[task.ID] = *task
	return nil
}

func (s *Server) closeCloudBackups() {
	s.cloudMu.Lock()
	state := s.cloud
	s.cloudMu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.closed = true
	if state.cancel != nil {
		state.cancel()
	}
	done := state.done
	state.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (s *Server) cloudTarget(ctx context.Context) (string, string, error) {
	selector := s.githubArchiveSelector()
	if selector == nil {
		return "", "", fmt.Errorf("请先连接 GitHub 并选择私有仓库")
	}
	target, ok, err := selector.SelectedArchiveRepository(ctx)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", fmt.Errorf("请先在设置中选择 GitHub 私有仓库")
	}
	return target.FullName, s.githubSettingsIdentity() + "|" + target.FullName, nil
}

func cloudArgs(root, socket string, ids []string) []string {
	args := []string{"--root", root}
	if socket != "" {
		args = append(args, "--socket", socket)
	}
	for _, id := range ids {
		args = append(args, "--id", id)
	}
	return args
}

func decodeCloudInput(writer http.ResponseWriter, request *http.Request, value any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(writer, 400, "云备份请求无效")
		return false
	}
	return true
}

func (s *Server) cloudBackupsHandler(writer http.ResponseWriter, request *http.Request) {
	state, err := s.cloudBackups()
	if err != nil {
		if request.Method == http.MethodGet {
			writeJSON(writer, 200, map[string]any{"available": false, "error": err.Error(), "items": []any{}, "source_read_only": true})
		} else {
			writeError(writer, 409, err.Error())
		}
		return
	}
	suffix := strings.Trim(strings.TrimPrefix(request.URL.Path, "/api/v1/backups"), "/")
	if suffix == "" && request.Method == http.MethodGet {
		state.mu.Lock()
		items := make([]cloudTask, 0, len(state.tasks))
		for _, task := range state.tasks {
			if task.SourceRoot == s.currentRoot() {
				items = append(items, task)
			}
		}
		active := state.active
		state.mu.Unlock()
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
		target, _, targetErr := s.cloudTarget(request.Context())
		message := ""
		if targetErr != nil {
			message = targetErr.Error()
		}
		writeJSON(writer, 200, map[string]any{"available": true, "target": target, "target_error": message, "items": items, "active": active, "source_read_only": true})
		return
	}
	if request.Method != http.MethodPost {
		writeError(writer, 405, "只支持 GET 或 POST")
		return
	}
	if suffix == "preview" {
		s.cloudBackupPreview(writer, request, state)
		return
	}
	if suffix == "start" {
		s.cloudBackupStart(writer, request, state)
		return
	}
	parts := strings.Split(suffix, "/")
	if len(parts) != 2 || !validCloudTaskID(parts[0]) {
		writeError(writer, 404, "云备份任务不存在")
		return
	}
	state.mu.Lock()
	task, ok := state.tasks[parts[0]]
	active := state.active
	state.mu.Unlock()
	if !ok || task.SourceRoot != s.currentRoot() {
		writeError(writer, 404, "当前工作区没有此备份任务")
		return
	}
	if parts[1] == "pause" {
		state.mu.Lock()
		if state.active != task.ID || state.cancel == nil {
			state.mu.Unlock()
			writeError(writer, 409, "该任务当前没有运行")
			return
		}
		state.cancel()
		state.mu.Unlock()
		writeJSON(writer, 202, map[string]any{"pause_requested": true, "source_read_only": true})
		return
	}
	if active != "" {
		writeError(writer, 409, "请先等待或暂停当前云备份任务")
		return
	}
	_, identity, err := s.cloudTarget(request.Context())
	if err != nil || identity != task.TargetIdentity {
		writeError(writer, 409, "连接或仓库已改变，请恢复任务原设置后再继续")
		return
	}
	if parts[1] == "resume" && (task.State == "failed" || task.State == "paused" || task.State == "interrupted") {
		task.State, task.Error = "queued", ""
		if err := s.launchCloudTask(state, task, false); err != nil {
			writeError(writer, 409, err.Error())
			return
		}
		writeJSON(writer, 202, task)
		return
	}
	if parts[1] == "download" && task.State == "completed" {
		if err := s.launchCloudTask(state, task, true); err != nil {
			writeError(writer, 409, err.Error())
			return
		}
		writeJSON(writer, 202, map[string]any{"download_started": true})
		return
	}
	writeError(writer, 409, "当前任务状态不允许此操作")
}

func (s *Server) cloudBackupPreview(writer http.ResponseWriter, request *http.Request, state *cloudBackupState) {
	var input struct {
		ConversationIDs []string `json:"conversation_ids"`
	}
	if !decodeCloudInput(writer, request, &input) {
		return
	}
	if len(input.ConversationIDs) == 0 || len(input.ConversationIDs) > 100 {
		writeError(writer, 400, "请选择 1–100 个会话")
		return
	}
	target, identity, err := s.cloudTarget(request.Context())
	if err != nil {
		writeError(writer, 409, err.Error())
		return
	}
	report, failure := s.readOnlyReportWithoutGraphs(request)
	if failure.status != 0 {
		writeError(writer, failure.status, failure.message)
		return
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, id := range input.ConversationIDs {
		conversation, _, _, ok := conversationAssets(report.report, id)
		if !ok {
			writeError(writer, 409, "所选会话已不存在，请刷新会话库")
			return
		}
		if !seen[conversation.SourceID] {
			if !(authorizedlive.Config{AllConversations: true}).Allows(conversation.SourceID) {
				writeError(writer, 409, "目标缺少原始 conversation ID")
				return
			}
			ids = append(ids, conversation.SourceID)
			seen[conversation.SourceID] = true
		}
	}
	data, err := state.run(request.Context(), "backup-preview", filepath.Join(state.root, "unused-preview"), cloudArgs(report.report.Root, s.lifecycle.AuthorizedLive.Socket, ids)...)
	if err != nil {
		writeError(writer, 409, err.Error())
		return
	}
	var preview cloudPreview
	if err := json.Unmarshal(data, &preview); err != nil || preview.Digest == "" || len(preview.IDs) == 0 {
		writeError(writer, 500, "兼容层返回了无效备份预览")
		return
	}
	preview.ID = "backup-preview-" + randomHex(12)
	preview.Target = target
	preview.identity = identity
	preview.SourceRoot = report.report.Root
	preview.SourceReadOnly = true
	preview.ExpiresAt = time.Now().UTC().Add(15 * time.Minute)
	state.mu.Lock()
	for id, p := range state.previews {
		if time.Now().After(p.ExpiresAt) {
			delete(state.previews, id)
		}
	}
	if len(state.previews) >= 8 {
		state.mu.Unlock()
		writeError(writer, 409, "待确认预览过多，请稍后重试")
		return
	}
	state.previews[preview.ID] = preview
	state.mu.Unlock()
	writeJSON(writer, 200, preview)
}

func (s *Server) cloudBackupStart(writer http.ResponseWriter, request *http.Request, state *cloudBackupState) {
	var input struct {
		PreviewID     string `json:"preview_id"`
		ConfirmUpload bool   `json:"confirm_upload"`
	}
	if !decodeCloudInput(writer, request, &input) {
		return
	}
	state.mu.Lock()
	preview, ok := state.previews[input.PreviewID]
	state.mu.Unlock()
	if !input.ConfirmUpload || !ok || time.Now().After(preview.ExpiresAt) || preview.SourceRoot != s.currentRoot() {
		writeError(writer, 409, "请先检查所选资产，并明确确认上传")
		return
	}
	_, identity, err := s.cloudTarget(request.Context())
	if err != nil || identity != preview.identity {
		writeError(writer, 409, "仓库或传输设置已变化，请重新检查")
		return
	}
	if preview.SourceBytes < 0 || preview.SourceBytes > 1<<40 {
		writeError(writer, 409, "备份大小超出本轮资源预算")
		return
	}
	if _, err := s.checkResourcesAdditional(request.Context(), 0, preview.SourceBytes*2+maxCloudRowReserve); err != nil {
		writeError(writer, 409, err.Error())
		return
	}
	task := cloudTask{ID: "backup-" + randomHex(12), State: "queued", SourceRoot: preview.SourceRoot, SourceIDs: preview.RequestedIDs, Target: preview.Target, TargetIdentity: preview.identity, PreviewDigest: preview.Digest, CreatedAt: time.Now().UTC(), SourceBytes: preview.SourceBytes}
	task.ArchiveID = task.ID
	if err := s.launchCloudTask(state, task, false); err != nil {
		writeError(writer, 409, err.Error())
		return
	}
	state.mu.Lock()
	delete(state.previews, preview.ID)
	state.mu.Unlock()
	writeJSON(writer, 202, task)
}

const maxCloudRowReserve int64 = 256 << 20

func (s *Server) launchCloudTask(state *cloudBackupState, task cloudTask, download bool) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || state.active != "" {
		return fmt.Errorf("已有云备份任务正在运行")
	}
	if download {
		task.DownloadState = "running"
		task.DownloadError = ""
		task.RecoveryPath = ""
	} else {
		task.State = "queued"
	}
	task.UpdatedAt = time.Now().UTC()
	if err := state.save(task); err != nil {
		return err
	}
	state.tasks[task.ID] = task
	state.active = task.ID
	ctx, cancel := context.WithCancel(context.Background())
	state.cancel = cancel
	state.done = make(chan struct{})
	go func() {
		defer func() {
			cancel()
			state.mu.Lock()
			state.active = ""
			state.cancel = nil
			close(state.done)
			state.mu.Unlock()
		}()
		err := s.runCloudTask(ctx, state, &task, download)
		if download {
			if err != nil {
				task.DownloadState = "failed"
				task.DownloadError = err.Error()
				if errors.Is(err, context.Canceled) {
					task.DownloadState = "interrupted"
				}
			} else {
				task.DownloadState = "completed"
			}
		} else if err != nil {
			task.State = "failed"
			task.Error = err.Error()
			if ctx.Err() != nil {
				task.State = "paused"
				task.Error = "已暂停，可继续上传"
			}
		}
		if err := state.publish(&task); err != nil {
			state.mu.Lock()
			task.State = "failed"
			task.Error = "无法保存任务记录，请检查磁盘空间"
			state.tasks[task.ID] = task
			state.mu.Unlock()
		}
	}()
	return nil
}

func (s *Server) runCloudTask(ctx context.Context, state *cloudBackupState, task *cloudTask, download bool, passwords ...string) error {
	// 长任务锁定远端配置；设置页的 TryLock 会明确提示用户先暂停任务。
	s.remoteConfigMu.RLock()
	defer s.remoteConfigMu.RUnlock()
	password := ""
	if len(passwords) > 0 {
		password = passwords[0]
	}
	remoteCopy := task.RemoteCopy || !task.LocalCopy
	if remoteCopy {
		_, identity, err := s.cloudTarget(ctx)
		if err != nil {
			return err
		}
		if identity != task.TargetIdentity {
			return fmt.Errorf("任务绑定的仓库或设置已改变")
		}
	}
	provider := s.remoteArchiveProvider()
	if remoteCopy && provider == nil {
		return fmt.Errorf("请先连接 GitHub")
	}
	if download {
		return s.downloadCloudTask(ctx, state, task, provider, password)
	}
	work := filepath.Join(state.root, task.ID)
	if err := prepareExecutionDirectory(work); err != nil {
		return err
	}
	if pathsOverlap(work, task.SourceRoot) {
		return fmt.Errorf("备份缓存与源重叠")
	}
	packagePath := filepath.Join(work, "package")
	if _, err := os.Stat(packagePath); os.IsNotExist(err) {
		exportPath := filepath.Join(work, "export")
		// 仅清除本任务上次中断的自建快照，重新走完整只读门禁。
		if err := os.RemoveAll(exportPath); err != nil {
			return err
		}
		task.State = "exporting"
		task.CacheRetained = true
		if err := state.publish(task); err != nil {
			return err
		}
		args := cloudArgs(task.SourceRoot, s.lifecycle.AuthorizedLive.Socket, task.SourceIDs)
		args = append(args, "--expected-digest", task.PreviewDigest)
		if _, err := state.run(ctx, "backup-export", exportPath, args...); err != nil {
			return err
		}
		task.State = "compressing"
		if err := state.publish(task); err != nil {
			return err
		}
		building := filepath.Join(work, "building")
		if err := os.RemoveAll(building); err != nil {
			return err
		}
		metadata, err := archive.CreateCloudBundle(ctx, exportPath, building, task.ID)
		if err != nil {
			return err
		}
		if task.Encrypted {
			if err := archive.EncryptCloudBundle(ctx, building, password); err != nil {
				return err
			}
		}
		if err := os.Rename(building, packagePath); err != nil {
			return err
		}
		task.Metadata = &metadata
		if err := state.publish(task); err != nil {
			return err
		}
		if err := os.RemoveAll(exportPath); err != nil {
			return err
		}
	}
	manifest, err := verifyArchivePackage(ctx, packagePath, password)
	if err != nil {
		return err
	}
	if manifest.CloudBackup == nil || manifest.CloudBackup.SnapshotSHA256 != task.PreviewDigest {
		return fmt.Errorf("缓存与已确认快照不匹配，请重新检查")
	}
	task.Metadata = manifest.CloudBackup
	task.SourceIDs = append([]string(nil), manifest.CloudBackup.ConversationIDs...)
	if task.LocalCopy {
		if task.LocalPath == "" || filepath.Dir(task.LocalPath) != filepath.Clean(s.lifecycle.ArchiveRoot) {
			return fmt.Errorf("本地归档目录已改变")
		}
		if err := archive.CopyCloudBundle(ctx, packagePath, task.LocalPath, password); err != nil {
			return err
		}
		if !remoteCopy {
			return finishCloudTask(state, task, work)
		}
	}
	task.CompletedBytes = 0
	task.TotalBytes = 0
	for _, a := range manifest.Assets {
		task.TotalBytes += a.SizeBytes
	}
	info, err := os.Stat(filepath.Join(packagePath, "manifest.json"))
	if err != nil {
		return err
	}
	task.TotalBytes += info.Size()
	task.State = "uploading"
	if err := state.publish(task); err != nil {
		return err
	}
	last := time.Now()
	transferCtx, cancelTransfer := context.WithCancel(ctx)
	defer cancelTransfer()
	var progressErr error
	progress := archive.WithTransferProgress(transferCtx, func(size int64) {
		task.CompletedBytes += size
		if time.Since(last) > 300*time.Millisecond {
			if err := state.publish(task); err != nil {
				progressErr = err
				cancelTransfer()
			}
			last = time.Now()
		}
	})
	if _, err := provider.UploadDirectory(progress, packagePath, task.ArchiveID); err != nil {
		if progressErr != nil {
			return fmt.Errorf("保存续传进度失败: %w", progressErr)
		}
		return err
	}
	if progressErr != nil {
		return progressErr
	}
	task.State = "verifying"
	if err := state.publish(task); err != nil {
		return err
	}
	if _, err := provider.VerifyArchive(ctx, task.ArchiveID); err != nil {
		return err
	}
	return finishCloudTask(state, task, work)
}

func finishCloudTask(state *cloudBackupState, task *cloudTask, work string) error {
	task.State = "completed"
	task.Error = ""
	task.CompletedBytes = task.TotalBytes
	// 先持久化远端已验证状态，再清除自建缓存；突然退出后最多留下可清理缓存。
	if err := state.publish(task); err != nil {
		return err
	}
	if err := os.RemoveAll(work); err != nil {
		task.Error = "归档已校验，本地工作缓存清理失败"
	} else {
		task.CacheRetained = false
	}
	return nil
}

func (s *Server) downloadCloudTask(ctx context.Context, state *cloudBackupState, task *cloudTask, provider archive.RemoteArchiveProvider, passwords ...string) error {
	password := ""
	if len(passwords) > 0 {
		password = passwords[0]
	}
	recovery, ok := provider.(archive.RemoteArchiveRecovery)
	if !ok {
		return fmt.Errorf("当前远端不支持下载")
	}
	root := filepath.Join(s.lifecycle.RemoteRecoveryRoot, "cloud-"+task.ID)
	if pathHasCodexComponent(root) || pathsOverlap(root, task.SourceRoot) {
		return fmt.Errorf("隔离下载目录与源冲突")
	}
	if err := prepareExecutionDirectory(root); err != nil {
		return err
	}
	// 中断材料只存在带本任务归属标记的 staging；完整结果始终校验后原子发布。
	staging, err := prepareCloudRecoveryStaging(root, *task)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	downloaded := filepath.Join(root, task.ArchiveID)
	if _, err := os.Lstat(downloaded); os.IsNotExist(err) {
		if _, err := recovery.DownloadArchive(ctx, task.ArchiveID, staging); err != nil {
			return err
		}
		staged := filepath.Join(staging, task.ArchiveID)
		manifest, err := verifyArchivePackage(ctx, staged, password)
		if err != nil {
			return err
		}
		if manifest.CloudBackup == nil || manifest.CloudBackup.SnapshotSHA256 != task.PreviewDigest {
			return fmt.Errorf("下载包与已确认目标不匹配")
		}
		if err := os.Rename(staged, downloaded); err != nil {
			return err
		}
		if err := syncCloudDirectory(root); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	manifest, err := verifyArchivePackage(ctx, downloaded, password)
	if err != nil {
		return err
	}
	if manifest.CloudBackup == nil || manifest.CloudBackup.SnapshotSHA256 != task.PreviewDigest {
		return fmt.Errorf("下载包与已确认目标不匹配")
	}
	if _, err := s.checkResourcesAdditional(ctx, 0, manifest.CloudBackup.OriginalBytes); err != nil {
		return err
	}
	output := filepath.Join(root, "recovered")
	if _, err := os.Lstat(output); err == nil {
		if err := archive.VerifyCloudRecovery(ctx, output, *manifest.CloudBackup); err != nil {
			return fmt.Errorf("已有隔离恢复目录校验失败，未覆盖: %w", err)
		}
		task.RecoveryPath = output
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	stagedOutput := filepath.Join(staging, "recovered")
	if _, err := archive.ExtractCloudBundle(ctx, downloaded, stagedOutput, s.resourceBudgetSnapshot().TemporaryLimitBytes, password); err != nil {
		return err
	}
	if err := os.Rename(stagedOutput, output); err != nil {
		return err
	}
	if err := syncCloudDirectory(root); err != nil {
		return err
	}
	task.RecoveryPath = output
	return nil
}

func verifyArchivePackage(ctx context.Context, path, password string) (archive.Manifest, error) {
	if password != "" {
		return archive.VerifyLocalEncrypted(ctx, path, password)
	}
	return archive.VerifyLocal(ctx, path)
}

// prepareCloudRecoveryStaging 只回收本任务自建的未完成目录，遇到未知内容保留并停止。
func prepareCloudRecoveryStaging(root string, task cloudTask) (string, error) {
	staging := filepath.Join(root, ".recovery-staging")
	marker := filepath.Join(staging, ".chronicle-owner")
	identity := task.ID + "\n" + task.PreviewDigest + "\n"
	if info, err := os.Lstat(staging); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("下载临时目录不是普通目录，未清理")
		}
		markerInfo, markerErr := os.Lstat(marker)
		owned := false
		if markerErr == nil && markerInfo.Mode().IsRegular() && markerInfo.Size() == int64(len(identity)) {
			data, err := os.ReadFile(marker)
			owned = err == nil && string(data) == identity
		}
		if !owned {
			entries, err := os.ReadDir(staging)
			if err != nil || len(entries) != 0 {
				return "", fmt.Errorf("下载临时目录归属不明，未清理")
			}
		}
		if err := os.RemoveAll(staging); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, err = file.WriteString(identity)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := syncCloudDirectory(staging); err != nil {
		return "", err
	}
	if err := syncCloudDirectory(root); err != nil {
		return "", err
	}
	return staging, nil
}

func syncCloudDirectory(path string) error {
	return durablefs.SyncDirectory(path)
}
