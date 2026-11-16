// 中文说明：串行推进已确认任务，逐项落盘进度并保护停止边界；异常、存储失败和配置变化均停止后续项。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/operation"
	"codex-chronicle.local/internal/store"
)

func cloneExecutionTask(task *executionTask) *executionTask {
	data, _ := json.Marshal(task)
	var copy executionTask
	_ = json.Unmarshal(data, &copy)
	return &copy
}

func (s *Server) publishExecutionTask(task *executionTask, begin bool) error {
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	stored := s.executionTasks[task.ID]
	if stored != nil && stored.StopRequested {
		task.StopRequested = true
		if begin {
			task.State = "stopped"
		}
	}
	if err := s.saveExecutionTask(task); err != nil {
		task.State, task.Error = "needs_review", "执行状态保存失败；已停止后续项，请核对逐项 journal: "+err.Error()
		if task.Completed > 0 {
			s.tasks.InvalidateInventory(task.SourceRoot)
		}
		s.executionTasks[task.ID] = cloneExecutionTask(task)
		s.executionActive = ""
		return err
	}
	newlyCompleted := task.Completed > 0 && (stored == nil || task.Completed > stored.Completed)
	finished := task.State != "running" && (stored == nil || stored.State == "running")
	// 每项提交就失效旧视图；只在整批结束后预热一次增量扫描，不阻塞执行完成回执。
	if newlyCompleted || finished && task.Completed > 0 {
		s.tasks.InvalidateInventory(task.SourceRoot)
	}
	s.executionTasks[task.ID] = cloneExecutionTask(task)
	if task.State != "running" {
		if finished && task.Completed > 0 {
			s.tasks.StartInventoryWithOptions(task.SourceRoot, application.ScanOptions{})
		}
		s.executionActive = ""
		// 终态由持久任务和 journal 接管；不继续占用待确认预览名额，也不删除恢复包。
		delete(s.executionReviews, task.PreviewID)
	}
	return nil
}

func (s *Server) runExecutionTask(id string, review *executionReview, password string, archive *store.Store) {
	s.executionMu.Lock()
	task := cloneExecutionTask(s.executionTasks[id])
	s.executionMu.Unlock()
	defer func() {
		if problem := recover(); problem != nil {
			task.State, task.Error = "needs_review", "执行发生异常，已停止后续项；请检查 journal 和恢复材料"
			_ = s.publishExecutionTask(task, false)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	fail := func(err error) {
		task.State, task.Error = "needs_review", err.Error()
		task.Preview.Blocked, task.Preview.Error = true, task.Error
		_ = s.publishExecutionTask(task, false)
	}
	if review.ArchivePreview != nil {
		if err := s.archiveBeforeRemoval(ctx, task, review, password); err != nil {
			fail(err)
			return
		}
		if !review.Input.Trash && !review.Input.PermanentDelete {
			for i := range task.Preview.Items {
				task.Preview.Items[i].Completed = true
			}
			task.Completed, task.State = len(task.Preview.Items), "completed"
			_ = s.publishExecutionTask(task, false)
			return
		}
	}
	s.remoteConfigMu.RLock()
	defer s.remoteConfigMu.RUnlock()
	if task.Restricted {
		if task.Sequential {
			if err := s.runSequentialLiveExecution(ctx, task, review, archive); err != nil {
				fail(err)
			}
			return
		}
		task.CurrentBatch, task.CurrentItem = 1, 1
		if err := s.publishExecutionTask(task, true); err != nil || task.State == "stopped" {
			return
		}
		s.trashProbeMu.Lock()
		err := s.validateExecutionReview(ctx, review, false)
		if err == nil {
			_, err = s.lifecycle.AuthorizedLive.Run(ctx, "apply", review.RestrictedWork)
		}
		s.trashProbeMu.Unlock()
		phase := lifecycle.PhaseNeedsReview
		if err == nil {
			phase = lifecycle.PhaseCommitted
		}
		applyErr := err
		for index := range task.Preview.Items {
			item := &task.Preview.Items[index]
			item.Result = &operation.SagaResult{OperationID: item.Plan.OperationID,
				JournalPath: filepath.Join(review.RestrictedWork, "journal.json"), Phase: phase, NeedsReview: applyErr != nil}
			if applyErr == nil {
				item.Completed = true
				task.Completed++
				if archive != nil {
					if cleanupErr := archive.RemoveConversationDerived(ctx, task.SourceRoot, item.ConversationID); cleanupErr != nil {
						err = fmt.Errorf("源操作已提交，派生数据清理失败: %w", cleanupErr)
					}
				}
			}
		}
		if progressErr := readLiveExecutionProgress(task, review.RestrictedWork); progressErr != nil && err == nil {
			err = progressErr
		}
		if err != nil {
			fail(err)
			return
		}
		task.State, task.Preview.ReadOnly, task.Preview.SourceReadOnly = "completed", false, false
		_ = s.publishExecutionTask(task, false)
		return
	}
	trash, err := s.executionTrash(ctx, task.SourceRoot, review.Input)
	if err != nil {
		fail(err)
		return
	}
	remote := s.remoteArchiveProvider()
	if selector := s.githubArchiveSelector(); selector != nil {
		_, configured, err := selector.SelectedArchiveRepository(ctx)
		if err != nil {
			fail(err)
			return
		}
		if !configured {
			remote = nil
		}
	}
	for batchStart := 0; batchStart < len(task.Preview.Items); batchStart += executionBatchSize {
		batchEnd := min(batchStart+executionBatchSize, len(task.Preview.Items))
		for index := batchStart; index < batchEnd; index++ {
			task.CurrentBatch, task.CurrentItem = batchStart/executionBatchSize+1, index+1
			if err := s.publishExecutionTask(task, true); err != nil || task.State == "stopped" {
				return
			}
			if task.SourceRoot != s.currentWorkspace().Definition.Root || !s.executionAllowed(task.SourceRoot) {
				fail(fmt.Errorf("工作区或权限变化，已停止后续项"))
				return
			}
			destination, err := s.executionDestination(ctx)
			if err != nil || destination != review.Destination {
				fail(fmt.Errorf("执行目的地变化，已停止后续项"))
				return
			}
			item := &task.Preview.Items[index]
			s.trashProbeMu.Lock()
			err = s.requireOperationDestinations(ctx, review.Input.operationRequest())
			if err == nil {
				err = s.checkReviewedItem(ctx, task.SourceRoot, *item, task.Preview.Items[:index])
			}
			var result operation.SagaResult
			if err == nil {
				result, err = operation.ExecuteLocalSagaWithOptions(ctx, *item.Plan, s.lifecycle.ArchiveRoot, s.lifecycle.JournalRoot, trash, operation.ExecutionOptions{
					ConfirmationCode: review.InternalCode, ArchivePassword: password,
					ControlPlaneMutation: s.lifecycle.ControlPlaneMutation, RemoteArchive: remote, RemoteArchiveID: task.ID + fmt.Sprintf("-%d", index),
					RegisterArtifact: s.cleanupArtifactRegistrar(),
				})
				item.Result = &result
			}
			s.trashProbeMu.Unlock()
			if err != nil {
				item.Blocked, item.Error = true, err.Error()
				fail(fmt.Errorf("第 %d 项停止: %w", index+1, err))
				return
			}
			item.Completed = true
			task.Completed++
			task.Preview.ReadOnly, task.Preview.SourceReadOnly = false, false
			// 先保留已提交结果，再清理本项目派生数据；后者失败不能把物理成功伪装成未执行。
			if err := s.publishExecutionTask(task, false); err != nil {
				return
			}
			if archive != nil && (review.Input.Trash || review.Input.PermanentDelete) {
				if err := archive.RemoveConversationDerived(ctx, task.SourceRoot, item.ConversationID); err != nil {
					fail(fmt.Errorf("源操作已提交，派生数据清理失败: %w", err))
					return
				}
			}
			pending := make([]*lifecycle.OperationPlan, 0)
			for next := index + 1; next < len(task.Preview.Items); next++ {
				pending = append(pending, task.Preview.Items[next].Plan)
			}
			if err := lifecycle.RebaseSharedAssetFingerprints(ctx, s.lifecycleDescriptor(), *item.Plan, pending); err != nil {
				fail(fmt.Errorf("已提交项的共享资产复核失败，后续项未执行: %w", err))
				return
			}
		}
	}
	task.State = "completed"
	_ = s.publishExecutionTask(task, false)
}

// runSequentialLiveExecution 读取执行器的持久进度；停止只写入本次操作目录，当前项安全结束后停止。
func (s *Server) runSequentialLiveExecution(ctx context.Context, task *executionTask, review *executionReview, archive *store.Store) error {
	task.CurrentBatch, task.CurrentItem = 1, 1
	if err := s.publishExecutionTask(task, true); err != nil || task.State == "stopped" {
		return err
	}
	s.trashProbeMu.Lock()
	defer s.trashProbeMu.Unlock()
	if err := s.validateExecutionReview(ctx, review, false); err != nil {
		return err
	}
	applyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.lifecycle.AuthorizedLive.Run(applyCtx, "apply", review.RestrictedWork)
		done <- err
	}()
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	var applyErr error
running:
	for {
		select {
		case applyErr = <-done:
			break running
		case <-ticker.C:
			if err := s.syncLiveExecutionProgress(task, review.RestrictedWork); err != nil {
				cancel()
				<-done
				return err
			}
		}
	}
	if err := s.syncLiveExecutionProgress(task, review.RestrictedWork); err != nil {
		return err
	}
	// 即使后续项失败，已经提交的对话也必须清除本项目的摘要、搜索索引和选择状态。
	for _, item := range task.Preview.Items {
		if item.Completed && archive != nil {
			if err := archive.RemoveConversationDerived(ctx, task.SourceRoot, item.ConversationID); err != nil {
				return fmt.Errorf("源操作已提交，派生数据清理失败：%w", err)
			}
		}
	}
	if applyErr != nil {
		return applyErr
	}
	if task.Completed == len(task.Preview.Items) {
		task.State = "completed"
	} else if task.StopRequested {
		task.State = "stopped"
	} else {
		return fmt.Errorf("执行器未完成全部目标，保留材料并停止后续项")
	}
	task.Preview.ReadOnly, task.Preview.SourceReadOnly = task.Completed == 0, task.Completed == 0
	return s.publishExecutionTask(task, false)
}

// syncLiveExecutionProgress 仅在持久进度变化时重写任务；停止请求仍在每轮检查并及时交给执行器。
func (s *Server) syncLiveExecutionProgress(task *executionTask, work string) error {
	previous := *task
	s.executionMu.Lock()
	if stored := s.executionTasks[task.ID]; stored != nil && stored.StopRequested {
		task.StopRequested = true
	}
	s.executionMu.Unlock()
	if task.SourceRoot != s.currentRoot() {
		task.StopRequested = true
	}
	if err := readLiveExecutionProgress(task, work); err != nil {
		return err
	}
	if task.Completed != previous.Completed || task.CurrentBatch != previous.CurrentBatch || task.CurrentItem != previous.CurrentItem ||
		task.RecoveryPurged != previous.RecoveryPurged || task.TrashPackagePath != previous.TrashPackagePath ||
		task.RecoveryCommand != previous.RecoveryCommand || task.PackagedTrash != previous.PackagedTrash || task.StopRequested != previous.StopRequested {
		if err := s.publishExecutionTask(task, false); err != nil {
			return err
		}
	}
	if task.StopRequested && !previous.StopRequested {
		if err := os.WriteFile(filepath.Join(work, "stop-requested"), []byte("stop after current item\n"), 0o600); err != nil {
			return fmt.Errorf("停止请求保存失败：%w", err)
		}
	}
	return nil
}

func readLiveExecutionProgress(task *executionTask, work string) error {
	data, err := os.ReadFile(filepath.Join(work, "journal.json"))
	if err != nil {
		return err
	}
	var journal struct {
		CompletedIDs   []string `json:"completed_ids"`
		CurrentBatch   int      `json:"current_batch"`
		CurrentItem    int      `json:"current_item"`
		RecoveryPurged bool     `json:"recovery_purged"`
		TrashPackage   *struct {
			State   string `json:"state"`
			Receipt *struct {
				TrashPath string `json:"trash_path"`
			} `json:"receipt"`
		} `json:"trash_package"`
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return err
	}
	completed := make(map[string]bool)
	for _, id := range journal.CompletedIDs {
		completed[id] = true
	}
	task.Completed = 0
	for index := range task.Preview.Items {
		item := &task.Preview.Items[index]
		if completed[item.SourceID] {
			delete(completed, item.SourceID)
			item.Completed = true
			item.Result = &operation.SagaResult{OperationID: item.Plan.OperationID, JournalPath: filepath.Join(work, "journal.json"), Phase: lifecycle.PhaseCommitted}
			task.Completed++
		}
	}
	if len(completed) != 0 {
		return fmt.Errorf("执行记录出现计划外目标，已停止")
	}
	task.CurrentBatch, task.CurrentItem = journal.CurrentBatch, journal.CurrentItem
	task.RecoveryPurged = journal.RecoveryPurged
	if journal.TrashPackage != nil {
		task.PackagedTrash = true
		if journal.TrashPackage.State == "published" && journal.TrashPackage.Receipt != nil {
			task.TrashPackagePath = journal.TrashPackage.Receipt.TrashPath
		}
	}
	if task.RecoveryPurged && task.TrashPackagePath == "" {
		task.RecoveryCommand = ""
	}
	return nil
}
