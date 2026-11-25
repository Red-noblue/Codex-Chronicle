// 中文说明：保存已审阅计划，以服务端一次性数字确认启动持久化任务；每批最多十项，失败或重启后不自动续删。
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/compat/authorizedlive"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/durablefs"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/operation"
	"codex-chronicle.local/internal/platform"
)

const executionBatchSize = 10
const confirmationLifetime = 5 * time.Minute
const maximumExecutionReviews = 32

type executionReview struct {
	Preview          batchOperationResponse
	Input            batchOperationRequest
	ExpiresAt        time.Time
	InternalCode     string
	Destination      string
	RestrictedWork   string
	RestrictedDigest string
	ArchivePreview   *cloudPreview
}

type executionConfirmation struct {
	ID             string    `json:"confirmation_id"`
	PreviewID      string    `json:"preview_id"`
	Code           string    `json:"code"`
	AdditionalCode string    `json:"additional_code,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	BatchSize      int       `json:"batch_size"`
	Total          int       `json:"total"`
	TaskID         string    `json:"-"`
	Consumed       bool      `json:"-"`
}

type executionRequest struct {
	ConfirmationID  string `json:"confirmation_id"`
	Code            string `json:"confirmation_code"`
	AdditionalCode  string `json:"additional_code"`
	ArchivePassword string `json:"archive_password"`
}

// executionTask 不保存确认码和密码；独立 journal/恢复包决定物理结果，重启只标记人工复核。
type executionTask struct {
	ID               string                  `json:"id"`
	PreviewID        string                  `json:"preview_id"`
	SourceRoot       string                  `json:"source_root"`
	State            string                  `json:"state"`
	CreatedAt        time.Time               `json:"created_at"`
	UpdatedAt        time.Time               `json:"updated_at"`
	BatchSize        int                     `json:"batch_size"`
	CurrentBatch     int                     `json:"current_batch"`
	CurrentItem      int                     `json:"current_item"`
	Completed        int                     `json:"completed"`
	StopRequested    bool                    `json:"stop_requested"`
	Error            string                  `json:"error,omitempty"`
	RecoveryPath     string                  `json:"recovery_path,omitempty"`
	RecoveryCommand  string                  `json:"recovery_command,omitempty"`
	Restricted       bool                    `json:"restricted"`
	Sequential       bool                    `json:"sequential"`
	RecoveryPurged   bool                    `json:"recovery_purged"`
	PackagedTrash    bool                    `json:"packaged_trash,omitempty"`
	TrashPackagePath string                  `json:"trash_package_path,omitempty"`
	ArchiveTaskID    string                  `json:"archive_task_id,omitempty"`
	Preview          batchOperationResponse  `json:"preview"`
	Strategy         operationPreviewRequest `json:"strategy"`
}

// executionProgressView 只保留进度面板所需字段，避免轮询反复传输完整资产图和写入计划。
func executionProgressView(task *executionTask) *executionTask {
	view := *task
	view.Preview = batchOperationResponse{Items: make([]batchOperationItem, 0, len(task.Preview.Items))}
	for _, item := range task.Preview.Items {
		entry := batchOperationItem{ConversationID: item.ConversationID, SourceID: item.SourceID,
			Title: item.Title, Completed: item.Completed, Blocked: item.Blocked, Error: item.Error}
		if item.Result != nil {
			entry.Result = &operation.SagaResult{JournalPath: item.Result.JournalPath,
				Phase: item.Result.Phase, NeedsReview: item.Result.NeedsReview}
		}
		view.Preview.Items = append(view.Preview.Items, entry)
	}
	return &view
}

func randomConfirmationCode() (string, error) {
	number, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", number.Int64()), nil
}

func (s *Server) executionDestination(ctx context.Context) (string, error) {
	if s.lifecycle == nil {
		return "", fmt.Errorf("未启用生命周期执行")
	}
	status, err := s.operationDestinations(ctx)
	if err != nil {
		return "", err
	}
	var remote any
	if selector := s.githubArchiveSelector(); selector != nil {
		repository, configured, err := selector.SelectedArchiveRepository(ctx)
		if err != nil {
			return "", err
		}
		remote = []any{repository, configured}
	}
	_, journalIdentity, err := platform.LocationIdentity(s.lifecycle.JournalRoot)
	if err != nil {
		journalIdentity = "unprepared"
	}
	data, err := json.Marshal([]any{status.SourceRoot, status.Key, status.Mode, status.TrashPath,
		s.lifecycle.ArchiveRoot, s.lifecycle.TrashRoot, s.lifecycle.JournalRoot, s.lifecycle.UseSystemTrash,
		s.lifecycle.FixtureOnly, s.lifecycleDescriptor().ID, fmt.Sprintf("%T", s.lifecycle.TrashProvider),
		s.lifecycle.AuthorizedLive, remote, journalIdentity, s.githubSettingsIdentity(), s.sourcePolicyFor(status.SourceRoot)})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Server) rememberExecutionReview(ctx context.Context, response *batchOperationResponse, input batchOperationRequest, code, work, digest string) error {
	if response.Blocked {
		return nil
	}
	destination := ""
	if s.lifecycle != nil && s.lifecycle.Enabled {
		// 只准备独立输出目录，冻结目录身份；创建隔离区不能在确认之后被误判为更换目的地。
		if validateLifecycleDestinations(response.SourceRoot, *s.lifecycle, input.operationRequest()) == nil {
			if err := prepareExecutionDirectory(s.executionTaskRoot()); err != nil {
				return err
			}
			if s.lifecycle.FixtureOnly && input.Trash && !s.lifecycle.UseSystemTrash && s.lifecycle.TrashProvider == nil {
				if _, err := platform.NewDirectoryTrash(s.lifecycle.TrashRoot); err != nil {
					return err
				}
			}
		}
		var err error
		destination, err = s.executionDestination(ctx)
		if err != nil {
			return err
		}
	}
	input.ArchivePassword, input.ConfirmationCode = "", ""
	response.PreviewID = response.BatchID
	review := &executionReview{Preview: *response, Input: input, InternalCode: code, Destination: destination,
		RestrictedWork: work, RestrictedDigest: digest, ArchivePreview: response.ArchivePreview, ExpiresAt: time.Now().Add(15 * time.Minute)}
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	s.pruneExecutionReviewsLocked()
	if len(s.executionReviews) >= maximumExecutionReviews {
		return fmt.Errorf("待确认预览过多，请等待已有预览过期后重试")
	}
	s.executionReviews[response.PreviewID] = review
	return nil
}

// 仅清理本进程生成、没有执行任务引用且仍处于空 prepared 阶段的过期包；不猜测处理中或旧恢复包。
func (s *Server) pruneExecutionReviewsLocked() {
	for id, review := range s.executionReviews {
		if time.Now().After(review.ExpiresAt) {
			s.discardExecutionReviewLocked(id)
		}
	}
	for id, confirmation := range s.executionConfirmations {
		if time.Now().After(confirmation.ExpiresAt) {
			delete(s.executionConfirmations, id)
		}
	}
}

func (s *Server) discardExecutionReviewLocked(id string) {
	review := s.executionReviews[id]
	delete(s.executionReviews, id)
	if review == nil || review.RestrictedWork == "" {
		return
	}
	for _, task := range s.executionTasks {
		if task.RecoveryPath == review.RestrictedWork {
			return
		}
	}
	s.removeUnusedPreparedWork(review.RestrictedWork, review.RestrictedDigest)
}

func (s *Server) removeUnusedPreparedWork(work, digest string) {
	if s.lifecycle == nil || filepath.Dir(work) != filepath.Clean(s.lifecycle.JournalRoot) || !strings.HasPrefix(filepath.Base(work), "authorized-batch-") {
		return
	}
	actual, _, err := authorizedlive.PlanDigest(work)
	if err != nil || actual != digest {
		return
	}
	var journal struct {
		Phase    string                     `json:"phase"`
		Receipts map[string]json.RawMessage `json:"receipts"`
		Steps    []json.RawMessage          `json:"steps"`
	}
	data, err := os.ReadFile(filepath.Join(work, "journal.json"))
	if err != nil || json.Unmarshal(data, &journal) != nil || journal.Phase != "prepared" || len(journal.Receipts) != 0 || len(journal.Steps) != 0 {
		return
	}
	_ = os.RemoveAll(work)
}

func (s *Server) executionAllowed(root string) bool {
	if s.lifecycle == nil || !s.lifecycle.Enabled {
		return false
	}
	if s.lifecycle.FixtureOnly {
		return !pathHasCodexComponent(root) && s.lifecycleWriteEnabled()
	}
	return s.lifecycle.AuthorizedLive != nil && s.lifecycle.AuthorizedLive.Validate(root) == nil
}

func (s *Server) validateExecutionReview(ctx context.Context, review *executionReview, dryRun bool) error {
	root := s.currentWorkspace().Definition.Root
	if root != review.Preview.SourceRoot || !s.executionAllowed(root) {
		return fmt.Errorf("源根或执行权限已变化，请重新预览")
	}
	if review.Input.Trash || review.Input.PermanentDelete {
		if err := s.requireSourceChanges(root); err != nil {
			return err
		}
	}
	if time.Now().After(review.ExpiresAt) {
		return fmt.Errorf("影响预览已过期，请重新预览")
	}
	destination, err := s.executionDestination(ctx)
	if err != nil {
		return err
	}
	if destination != review.Destination {
		return fmt.Errorf("回收站、归档目录或执行配置已变化，请重新预览")
	}
	if err := validateLifecycleDestinations(root, *s.lifecycle, review.Input.operationRequest()); err != nil {
		return err
	}
	if err := s.requireOperationDestinations(ctx, review.Input.operationRequest()); err != nil {
		return err
	}
	if review.ArchivePreview != nil {
		if err := s.validateArchiveReview(ctx, review); err != nil {
			return err
		}
		if !review.Input.Trash && !review.Input.PermanentDelete {
			return nil
		}
	}
	if review.RestrictedWork != "" {
		settings, err := s.loadTrashSettings(ctx)
		if err != nil {
			return err
		}
		config := s.lifecycle.AuthorizedLive
		if ((review.Input.Archive || review.Input.Encrypted) && review.ArchivePreview == nil) || !config.AllowsAction(review.Input.Trash, review.Input.PermanentDelete) || (review.Input.Trash && settings.CustomPath != "") {
			return fmt.Errorf("当前真实执行不支持此操作组合；回收站须使用原生位置，永久删除须开启全会话模式")
		}
		digest, data, err := authorizedlive.PlanDigest(review.RestrictedWork)
		if err != nil {
			return err
		}
		if digest != review.RestrictedDigest {
			return fmt.Errorf("受限影响预览发生变化，确认失效")
		}
		var plan struct {
			Operation string `json:"operation"`
		}
		if json.Unmarshal(data, &plan) != nil || (review.Input.PermanentDelete != (plan.Operation == "permanent_delete")) {
			return fmt.Errorf("执行方式与审阅的计划不一致")
		}
		if _, err := s.lifecycle.AuthorizedLive.Run(ctx, "check", review.RestrictedWork); err != nil {
			return err
		}
		if dryRun {
			_, err = s.lifecycle.AuthorizedLive.Run(ctx, "dry-run", review.RestrictedWork)
		}
		return err
	}
	for _, item := range review.Preview.Items {
		if item.Plan == nil || item.Blocked {
			return fmt.Errorf("预览包含不可执行项")
		}
		if err := lifecycle.RevalidatePlan(ctx, *item.Plan); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) executionConfirmationHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var input struct {
		PreviewID string `json:"preview_id"`
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 4096)
	if json.NewDecoder(request.Body).Decode(&input) != nil {
		writeError(writer, http.StatusBadRequest, "确认请求不是有效 JSON")
		return
	}
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	review := s.executionReviews[input.PreviewID]
	if review == nil {
		writeError(writer, http.StatusConflict, "没有有效的已审阅计划，请重新生成影响预览")
		return
	}
	if s.executionActive != "" {
		writeError(writer, http.StatusConflict, "已有执行任务，请等待完成或停止后续项")
		return
	}
	if len(s.executionLoadErrors) > 0 {
		writeError(writer, http.StatusConflict, "执行记录读取失败，需先人工复核："+strings.Join(s.executionLoadErrors, "；"))
		return
	}
	s.trashProbeMu.Lock()
	defer s.trashProbeMu.Unlock()
	if err := s.validateExecutionReview(request.Context(), review, true); err != nil {
		s.discardExecutionReviewLocked(input.PreviewID)
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	for id, old := range s.executionConfirmations {
		if old.PreviewID == input.PreviewID || time.Now().After(old.ExpiresAt) {
			delete(s.executionConfirmations, id)
		}
	}
	id, err := newBatchOperationID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	code, err := randomConfirmationCode()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	confirmation := &executionConfirmation{ID: "confirm-" + id, PreviewID: input.PreviewID, Code: code,
		ExpiresAt: time.Now().Add(confirmationLifetime), BatchSize: executionBatchSize, Total: len(review.Preview.Items)}
	if confirmation.ExpiresAt.After(review.ExpiresAt) {
		confirmation.ExpiresAt = review.ExpiresAt
	}
	if !review.Input.Trash && !review.Input.PermanentDelete {
		confirmation.Code = ""
	}
	if confirmation.Code != "" && confirmation.Total > executionBatchSize {
		for {
			confirmation.AdditionalCode, err = randomConfirmationCode()
			if err != nil {
				writeError(writer, http.StatusInternalServerError, err.Error())
				return
			}
			if confirmation.AdditionalCode != code {
				break
			}
		}
	}
	s.executionConfirmations[confirmation.ID] = confirmation
	writeJSON(writer, http.StatusOK, confirmation)
}

// confirmedExecutionHandler 也是旧执行 URL 的唯一入口，不接受客户端重建目标集合或自选确认码。
func (s *Server) confirmedExecutionHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if !s.executionAllowed(s.currentWorkspace().Definition.Root) {
		writeError(writer, http.StatusForbidden, "当前工作区未启用合成 fixture 或受限真实执行权限")
		return
	}
	var input executionRequest
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		writeError(writer, http.StatusBadRequest, "只接受服务端确认凭据，不能在执行时更换目标或方式")
		return
	}
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	confirmation := s.executionConfirmations[input.ConfirmationID]
	if confirmation == nil {
		writeError(writer, http.StatusConflict, "请重新取得本次计划的确认数字")
		return
	}
	correct := subtle.ConstantTimeCompare([]byte(input.Code), []byte(confirmation.Code)) == 1 &&
		subtle.ConstantTimeCompare([]byte(input.AdditionalCode), []byte(confirmation.AdditionalCode)) == 1
	if confirmation.Consumed {
		if correct && confirmation.TaskID != "" {
			task := s.executionTasks[confirmation.TaskID]
			if task == nil || task.SourceRoot != s.currentWorkspace().Definition.Root {
				writeError(writer, http.StatusConflict, "确认所属工作区已变化，请切回原工作区查询任务")
				return
			}
			writeJSON(writer, http.StatusOK, task)
			return
		}
		writeError(writer, http.StatusConflict, "确认已失效，请重新获取")
		return
	}
	confirmation.Consumed = true
	if !correct || time.Now().After(confirmation.ExpiresAt) {
		writeError(writer, http.StatusConflict, "确认数字不匹配或已过期，请重新获取")
		return
	}
	review := s.executionReviews[confirmation.PreviewID]
	if review == nil {
		writeError(writer, http.StatusConflict, "影响预览已失效，请重新预览")
		return
	}
	if s.executionActive != "" {
		writeError(writer, http.StatusConflict, "已有执行任务，不允许并发删除")
		return
	}
	if len(s.executionLoadErrors) > 0 {
		writeError(writer, http.StatusConflict, "执行记录读取失败，需先人工复核："+strings.Join(s.executionLoadErrors, "；"))
		return
	}
	s.trashProbeMu.Lock()
	err := s.validateExecutionReview(request.Context(), review, false)
	s.trashProbeMu.Unlock()
	if err != nil {
		s.discardExecutionReviewLocked(confirmation.PreviewID)
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	if review.Input.Encrypted && strings.TrimSpace(input.ArchivePassword) == "" {
		writeError(writer, http.StatusBadRequest, "加密归档必须提供密码")
		return
	}
	var planBytes int64
	if review.ArchivePreview != nil {
		planBytes = review.ArchivePreview.SourceBytes + maxCloudRowReserve
	}
	for _, item := range review.Preview.Items {
		if item.Plan != nil {
			planBytes += lifecyclePlanBytes(*item.Plan)
		}
	}
	if _, err := s.checkResourcesAdditional(request.Context(), planBytes, planBytes); err != nil {
		writeError(writer, http.StatusInsufficientStorage, err.Error())
		return
	}
	id, err := newBatchOperationID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	// 复制计划，后续只允许按已提交动作刷新明确共享的资产指纹，不改动缓存中的审阅集合。
	data, _ := json.Marshal(review.Preview)
	var preview batchOperationResponse
	_ = json.Unmarshal(data, &preview)
	now := time.Now().UTC()
	task := &executionTask{ID: "execution-" + id, PreviewID: confirmation.PreviewID, SourceRoot: preview.SourceRoot,
		State: "running", CreatedAt: now, UpdatedAt: now, BatchSize: executionBatchSize, Preview: preview,
		Restricted: review.RestrictedWork != "", RecoveryPath: review.RestrictedWork, Strategy: review.Input.operationRequest()}
	task.Sequential = task.Restricted && s.lifecycle.AuthorizedLive.AllConversations
	task.PackagedTrash = task.Restricted && task.Strategy.Trash
	if task.Restricted {
		// 恢复包只按行合并，不提供 Finder 放回正文或整库覆盖的错误恢复方法。
		task.RecoveryCommand = fmt.Sprintf("python3 docs/项目验证/authorized_live.py recover --work %q --bridge %q", task.RecoveryPath, s.lifecycle.AuthorizedLive.Bridge)
		if task.Sequential {
			task.RecoveryCommand += " --all-conversations"
		}
	}
	release := func() {}
	if review.Input.Trash || review.Input.PermanentDelete {
		var err error
		release, err = s.beginSourceChanges(task.SourceRoot)
		if err != nil {
			writeError(writer, http.StatusForbidden, err.Error())
			return
		}
	}
	// 操作计数持有后复核确认摘要，封住验证期间策略来回切换的窗口。
	if destination, err := s.executionDestination(request.Context()); err != nil || destination != review.Destination {
		release()
		writeError(writer, http.StatusConflict, "执行配置已变化，请重新预览")
		return
	}
	if err := s.saveExecutionTask(task); err != nil {
		release()
		writeError(writer, http.StatusInternalServerError, "执行记录落盘失败，未修改源: "+err.Error())
		return
	}
	confirmation.TaskID = task.ID
	s.executionTasks[task.ID], s.executionActive = task, task.ID
	archive := s.currentWorkspace().Archive
	go func() {
		defer release()
		s.runExecutionTask(task.ID, review, input.ArchivePassword, archive)
	}()
	writeJSON(writer, http.StatusAccepted, task)
}

func (s *Server) executionTasksHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/api/v1/operations/executions"), "/")
	if path == "" && request.Method == http.MethodPost {
		s.confirmedExecutionHandler(writer, request)
		return
	}
	view := request.URL.Query().Get("view")
	if view != "" && view != "progress" {
		writeError(writer, http.StatusBadRequest, "view 仅支持 progress")
		return
	}
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	root := s.currentWorkspace().Definition.Root
	if path == "" && request.Method == http.MethodGet {
		items := make([]*executionTask, 0)
		for _, task := range s.executionTasks {
			if task.SourceRoot == root {
				if view == "progress" {
					items = append(items, executionProgressView(task))
				} else {
					items = append(items, task)
				}
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
		writeJSON(writer, http.StatusOK, map[string]any{"items": items, "errors": s.executionLoadErrors})
		return
	}
	parts := strings.Split(path, "/")
	task := s.executionTasks[parts[0]]
	if task == nil || task.SourceRoot != root {
		writeError(writer, http.StatusNotFound, "未找到当前工作区的执行任务")
		return
	}
	if len(parts) == 1 && request.Method == http.MethodGet {
		if view == "progress" {
			writeJSON(writer, http.StatusOK, executionProgressView(task))
		} else {
			writeJSON(writer, http.StatusOK, task)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && request.Method == http.MethodPost {
		if task.State == "running" {
			task.StopRequested = true
			if err := s.saveExecutionTask(task); err != nil {
				writeError(writer, http.StatusInternalServerError, "停止请求落盘失败: "+err.Error())
				return
			}
		}
		writeJSON(writer, http.StatusOK, task)
		return
	}
	writeError(writer, http.StatusMethodNotAllowed, "不支持的执行任务请求")
}

func (s *Server) executionTaskRoot() string {
	if s.lifecycle == nil || s.lifecycle.JournalRoot == "" {
		return ""
	}
	return filepath.Join(s.lifecycle.JournalRoot, "executions")
}

func (s *Server) saveExecutionTask(task *executionTask) error {
	root := s.executionTaskRoot()
	if root == "" {
		return fmt.Errorf("未配置执行任务记录目录")
	}
	if err := prepareExecutionDirectory(root); err != nil {
		return err
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("执行记录目录不安全")
	}
	task.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".execution-*.tmp")
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
	if err := os.Rename(file.Name(), filepath.Join(root, task.ID+".json")); err != nil {
		return err
	}
	return durablefs.SyncDirectory(root)
}

// prepareExecutionDirectory 逐级同步新建目录；拒绝输出根经符号链接落入任何 .codex。
func prepareExecutionDirectory(path string) error {
	canonical, err := canonicalLifecycleDestination(path)
	if err != nil {
		return err
	}
	if pathHasCodexComponent(canonical) {
		return fmt.Errorf("执行记录不能位于 .codex")
	}
	var missing []string
	for current := canonical; ; current = filepath.Dir(current) {
		if _, err := os.Stat(current); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, current)
	}
	for index := len(missing) - 1; index >= 0; index-- {
		if err := os.Mkdir(missing[index], 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		err = durablefs.SyncDirectory(filepath.Dir(missing[index]))
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) loadExecutionTasks() {
	root := s.executionTaskRoot()
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			s.executionLoadErrors = append(s.executionLoadErrors, "执行记录目录不可读："+err.Error())
		}
		return
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "execution-batch-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		var task executionTask
		if err != nil || json.Unmarshal(data, &task) != nil || task.ID+".json" != entry.Name() {
			s.executionLoadErrors = append(s.executionLoadErrors, "损坏或不可读的执行记录："+entry.Name())
			continue
		}
		if task.State == "running" {
			task.State, task.Error = "needs_review", "服务曾在执行中退出；未自动续删，请核对逐项 journal 和恢复材料"
			if err := s.saveExecutionTask(&task); err != nil {
				s.executionLoadErrors = append(s.executionLoadErrors, "中断任务复核状态无法保存："+entry.Name())
			}
		}
		s.executionTasks[task.ID] = &task
	}
}

func (s *Server) checkReviewedItem(ctx context.Context, root string, item batchOperationItem, completed []batchOperationItem) error {
	report, err := s.scanner.Scan(ctx, root)
	if err != nil {
		return err
	}
	conversation, assets, owners, found := conversationAssets(report, item.ConversationID)
	if !found || conversation.SourceID != item.SourceID {
		return fmt.Errorf("目标身份变化或已不存在，请重新预览")
	}
	conversation, assets, _, err = s.probeConversationActivity(ctx, report, conversation, assets)
	if err != nil {
		return err
	}
	if conversation.Status != domain.StatusStable {
		return fmt.Errorf("目标活动状态 %s 禁止执行", conversation.Status)
	}
	if item.Plan == nil || len(assets) != len(item.Plan.Assets) {
		return fmt.Errorf("资产关联集合发生变化，请重新预览")
	}
	for _, asset := range assets {
		if asset.Status != domain.StatusStable || asset.ChangedDuringScan {
			return fmt.Errorf("目标资产状态变化，停止执行")
		}
		found := false
		for _, planned := range item.Plan.Assets {
			if planned.RelativePath == asset.RelativePath && planned.Kind == asset.Kind {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("发现未审阅的资产，停止执行")
		}
		expectedOwners := 0
		for _, original := range item.DiscoveredAssets {
			if original.RelativePath == asset.RelativePath {
				expectedOwners = original.Owners
				break
			}
		}
		// 前序已提交项会减少共享索引/数据库的引用数；只接受本任务明确解释的减少。
		for _, done := range completed {
			if !done.Completed {
				continue
			}
			for _, original := range done.DiscoveredAssets {
				if original.RelativePath == asset.RelativePath {
					expectedOwners--
					break
				}
			}
		}
		if expectedOwners < 1 || owners[asset.RelativePath] != expectedOwners {
			return fmt.Errorf("资产共享关系变化，停止执行")
		}
	}
	return lifecycle.RevalidatePlan(ctx, *item.Plan)
}

func (s *Server) executionTrash(ctx context.Context, root string, input batchOperationRequest) (platform.TrashProvider, error) {
	if !input.Trash {
		return nil, nil
	}
	if s.lifecycle.TrashProvider != nil {
		return s.lifecycle.TrashProvider, nil
	}
	if s.lifecycle.UseSystemTrash {
		return s.systemTrashProvider(ctx, root)
	}
	if s.lifecycle.TrashRoot == "" {
		return nil, fmt.Errorf("未配置回收站目的地")
	}
	return platform.NewDirectoryTrash(s.lifecycle.TrashRoot)
}
