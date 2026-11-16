// 中文说明：统一完整会话归档与源移除；所有选定副本校验成功后，才进入既有移除执行器。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

func (s *Server) buildArchiveBatch(ctx context.Context, input batchOperationRequest, report domain.InventoryReport, response batchOperationResponse) (batchOperationResponse, error) {
	if input.Trash && input.PermanentDelete {
		return response, fmt.Errorf("移入回收站与删除不能同时选择")
	}
	if err := validateLifecycleDestinations(response.SourceRoot, *s.lifecycle, input.operationRequest()); err != nil {
		return response, err
	}
	state, err := s.cloudBackups()
	if err != nil {
		return response, err
	}
	preview := cloudPreview{SourceRoot: response.SourceRoot, SourceReadOnly: true, ExpiresAt: time.Now().Add(15 * time.Minute)}
	if input.ArchiveRemote {
		preview.Target, preview.identity, err = s.cloudTarget(ctx)
		if err != nil {
			return response, err
		}
	}
	ids := []string{}
	requested := map[string]bool{}
	for _, id := range input.ConversationIDs {
		conversation, _, _, found := conversationAssets(report, id)
		if !found || conversation.SourceID == "" {
			return response, fmt.Errorf("所选会话不存在或缺少原始 ID")
		}
		if s.lifecycle.AuthorizedLive != nil && !s.lifecycle.AuthorizedLive.Allows(conversation.SourceID) {
			return response, fmt.Errorf("目标不在执行授权范围内")
		}
		ids = append(ids, conversation.SourceID)
		requested[conversation.SourceID] = true
	}
	data, err := state.run(ctx, "backup-preview", filepath.Join(state.root, "unused-preview"), cloudArgs(response.SourceRoot, s.lifecycle.AuthorizedLive.Socket, ids)...)
	if err != nil {
		return response, err
	}
	if json.Unmarshal(data, &preview) != nil || preview.Digest == "" || len(preview.IDs) == 0 {
		return response, fmt.Errorf("兼容层未返回完整归档预览")
	}
	preview.ID = response.BatchID
	response.ArchivePreview = &preview
	if input.Trash || input.PermanentDelete {
		removal := input
		removal.Archive, removal.Encrypted = false, false
		response, err = s.buildRestrictedBatch(ctx, removal, report, response)
		if err != nil || response.Blocked {
			return response, err
		}
		s.executionMu.Lock()
		review := s.executionReviews[response.PreviewID]
		if review != nil {
			review.Input = input
			review.Input.ArchivePassword = ""
			review.ArchivePreview = &preview
		}
		s.executionMu.Unlock()
		return response, nil
	}
	response.ExecutionScope = "complete_snapshot_archive"
	response.Summary = batchOperationSummary{RequestedConversations: len(input.ConversationIDs), Conversations: len(preview.IDs), Prepared: len(preview.IDs), Assets: len(preview.Assets), Bytes: preview.SourceBytes}
	for _, sourceID := range preview.IDs {
		var found *domain.ConversationSummary
		for i := range report.Conversations {
			if report.Conversations[i].SourceID == sourceID {
				found = &report.Conversations[i]
				break
			}
		}
		if found == nil {
			return response, fmt.Errorf("归档发现尚未展示的子代理，请刷新后重试")
		}
		response.ArchivePreview.Target = preview.Target
		actions := []lifecycle.Action{lifecycle.ActionArchive}
		if input.Encrypted {
			actions = []lifecycle.Action{lifecycle.ActionEncryptedArchive}
		}
		plan := &lifecycle.OperationPlan{OperationID: response.BatchID, ConversationID: found.ID, SourceID: sourceID, Root: response.SourceRoot, Status: lifecycle.PlanPrepared, Actions: actions, ReadOnly: true, RequiresConfirm: true, AdapterID: "complete-conversation-snapshot-v1"}
		for _, asset := range preview.Assets {
			if asset.ID == sourceID {
				plan.Assets = append(plan.Assets, lifecycle.AssetFingerprint{RelativePath: asset.Relative, SizeBytes: asset.Bytes, SHA256: asset.SHA256})
			}
		}
		response.Items = append(response.Items, batchOperationItem{ConversationID: found.ID, SourceID: sourceID, Title: found.Title, AutomaticallyIncluded: !requested[sourceID], ParentSourceID: found.ParentThreadID, ThreadKind: found.ThreadKind, Plan: plan})
	}
	if err := s.rememberExecutionReview(ctx, &response, input, "", "", ""); err != nil {
		return response, err
	}
	return response, nil
}

func (s *Server) validateArchiveReview(ctx context.Context, review *executionReview) error {
	p := review.ArchivePreview
	if review.Input.ArchiveRemote {
		_, identity, err := s.cloudTarget(ctx)
		if err != nil || identity != p.identity {
			return fmt.Errorf("归档仓库或传输设置已改变，请重新预览")
		}
	}
	state, err := s.cloudBackups()
	if err != nil {
		return err
	}
	data, err := state.run(ctx, "backup-preview", filepath.Join(state.root, "unused-preview"), cloudArgs(p.SourceRoot, s.lifecycle.AuthorizedLive.Socket, p.RequestedIDs)...)
	if err != nil {
		return err
	}
	var current cloudPreview
	if json.Unmarshal(data, &current) != nil || current.Digest != p.Digest {
		return fmt.Errorf("归档源内容或关联已变化，请重新预览")
	}
	return nil
}

func (s *Server) archiveBeforeRemoval(ctx context.Context, task *executionTask, review *executionReview, password string) error {
	state, err := s.cloudBackups()
	if err != nil {
		return err
	}
	p := review.ArchivePreview
	backup := cloudTask{ID: "backup-" + randomHex(12), State: "queued", SourceRoot: p.SourceRoot, SourceIDs: p.RequestedIDs, Target: p.Target, TargetIdentity: p.identity, PreviewDigest: p.Digest, SourceBytes: p.SourceBytes, CreatedAt: time.Now().UTC(), LocalCopy: review.Input.ArchiveLocal, RemoteCopy: review.Input.ArchiveRemote, Encrypted: review.Input.Encrypted}
	for _, item := range review.Preview.Items {
		if item.Title != "" {
			backup.Titles = append(backup.Titles, item.Title)
		}
	}
	backup.ArchiveID = backup.ID
	if backup.LocalCopy {
		backup.LocalPath = filepath.Join(s.lifecycle.ArchiveRoot, backup.ID)
	}
	state.mu.Lock()
	if state.closed || state.active != "" {
		state.mu.Unlock()
		return fmt.Errorf("另一个归档任务正在执行，请稍后重新预览")
	}
	if err := state.save(backup); err != nil {
		state.mu.Unlock()
		return err
	}
	state.tasks[backup.ID], state.active = backup, backup.ID
	archiveCtx, cancel := context.WithCancel(ctx)
	state.cancel, state.done = cancel, make(chan struct{})
	state.mu.Unlock()
	defer func() {
		cancel()
		state.mu.Lock()
		state.active = ""
		state.cancel = nil
		close(state.done)
		state.mu.Unlock()
	}()
	task.ArchiveTaskID = backup.ID
	if err := s.publishExecutionTask(task, true); err != nil || task.State == "stopped" {
		return fmt.Errorf("执行已停止，源数据保留")
	}
	if err := s.runCloudTask(archiveCtx, state, &backup, false, password); err != nil {
		backup.State, backup.Error = "failed", err.Error()
		_ = state.publish(&backup)
		return fmt.Errorf("归档未完成，源数据保留: %w", err)
	}
	if err := state.publish(&backup); err != nil {
		return err
	}
	return nil
}
