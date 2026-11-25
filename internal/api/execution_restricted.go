// 中文说明：按启动授权建立本机真实资产预览；全会话模式移除固定 ID 范围，仍拒绝活动目标、复杂关联和未知 schema。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"codex-chronicle.local/internal/compat/authorizedlive"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

func (s *Server) buildRestrictedBatch(ctx context.Context, request batchOperationRequest, report domain.InventoryReport, response batchOperationResponse) (batchOperationResponse, error) {
	response.ExecutionScope = "authorized_local_ids_only"
	config := s.lifecycle.AuthorizedLive
	if config.AllConversations {
		response.ExecutionScope = "local_idle_conversations"
	}
	expandedIDs, err := expandFamilyTargets(report.Conversations, request.ConversationIDs)
	if err != nil {
		return response, err
	}
	requested := map[string]bool{}
	for _, id := range request.ConversationIDs {
		requested[id] = true
	}
	response.Summary.Conversations = len(expandedIDs)
	for _, id := range expandedIDs {
		conversation, assets, owners, found := conversationAssets(report, id)
		item := batchOperationItem{ConversationID: id, SourceID: conversation.SourceID, Title: conversation.Title, AssetGraph: conversation.AssetGraph,
			AutomaticallyIncluded: !requested[id], ParentSourceID: conversation.ParentThreadID, ThreadKind: conversation.ThreadKind,
			DiscoveredAssets: make([]batchOperationAsset, 0)}
		for _, asset := range assets {
			item.DiscoveredAssets = append(item.DiscoveredAssets, batchOperationAsset{RelativePath: asset.RelativePath, Kind: asset.Kind, SizeBytes: asset.SizeBytes, Status: asset.Status, Owners: owners[asset.RelativePath]})
		}
		switch {
		case !found:
			item.Error = "目标已不存在，请刷新会话库"
		case !config.Allows(conversation.SourceID):
			item.Error = "该原始 conversation ID 未获得本机真实执行授权；没有修改或备份任何源数据"
		case request.Archive || request.Encrypted || !config.AllowsAction(request.Trash, request.PermanentDelete):
			item.Error = "请选择移入原生废纸篓或永久删除；真实归档尚未接入此执行器"
		}
		item.Blocked = item.Error != ""
		if item.Blocked {
			response.Summary.Blocked++
		}
		response.Items = append(response.Items, item)
	}
	if response.Summary.Blocked > 0 {
		response.Blocked = true
		return response, nil
	}
	if err := s.lifecycle.AuthorizedLive.Validate(response.SourceRoot); err != nil {
		return response, err
	}
	if err := validateLifecycleDestinations(response.SourceRoot, *s.lifecycle, request.operationRequest()); err != nil {
		return response, err
	}
	// 恢复包较大：串行准备并限制未执行预览数量，避免反复点击占满磁盘。
	s.restrictedPreviewMu.Lock()
	defer s.restrictedPreviewMu.Unlock()
	s.executionMu.Lock()
	s.pruneExecutionReviewsLocked()
	pending := 0
	for _, review := range s.executionReviews {
		if review.RestrictedWork != "" {
			pending++
		}
	}
	busy := s.executionActive != "" || len(s.executionLoadErrors) > 0
	s.executionMu.Unlock()
	if busy || pending >= 4 {
		return response, fmt.Errorf("已有执行任务、待复核记录或四份待确认真实预览；请先核对任务或等待预览过期")
	}
	work := filepath.Join(s.lifecycle.JournalRoot, "authorized-"+response.BatchID)
	ids := make([]string, 0, len(response.Items))
	args := []string{"--root", response.SourceRoot, "--socket", s.lifecycle.AuthorizedLive.Socket}
	if request.PermanentDelete {
		args = append(args, "--operation", "permanent_delete")
	}
	for _, item := range response.Items {
		ids = append(ids, item.SourceID)
		if requested[item.ConversationID] {
			args = append(args, "--id", item.SourceID)
		}
	}
	if _, err := s.lifecycle.AuthorizedLive.Run(ctx, "prepare", work, args...); err != nil {
		// prepare 不修改源；失败的本轮临时备份没有执行意义。
		if _, statErr := os.Stat(filepath.Join(work, "journal.json")); os.IsNotExist(statErr) {
			_ = os.RemoveAll(work)
		}
		response.Blocked, response.Error, response.Summary.Blocked = true, err.Error(), len(response.Items)
		for i := range response.Items {
			response.Items[i].Blocked, response.Items[i].Error = true, response.Error
		}
		return response, nil
	}
	digest, data, err := authorizedlive.PlanDigest(work)
	if err != nil {
		return response, err
	}
	remembered := false
	defer func() {
		if !remembered {
			s.removeUnusedPreparedWork(work, digest)
		}
	}()
	var plan struct {
		IDs         []string        `json:"ids"`
		Family      json.RawMessage `json:"family"`
		OperationID string          `json:"operation_id"`
		Assets      []struct {
			ID       string `json:"id"`
			Relative string `json:"relative"`
			Kind     string `json:"kind"`
			SHA256   string `json:"sha256"`
			Bytes    int64  `json:"bytes"`
			MTimeNS  int64  `json:"mtime_ns"`
		} `json:"assets"`
		Metadata      json.RawMessage `json:"metadata"`
		Texts         json.RawMessage `json:"texts"`
		Schemas       json.RawMessage `json:"schemas"`
		SQLitePreview json.RawMessage `json:"sqlite_preview"`
		Auxiliary     json.RawMessage `json:"auxiliary_sqlite"`
		Protected     json.RawMessage `json:"protected"`
		Activity      json.RawMessage `json:"activity"`
		TrashPackage  json.RawMessage `json:"trash_package"`
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return response, err
	}
	if len(plan.IDs) != len(ids) {
		return response, fmt.Errorf("子代理集合在扫描后变化，请刷新并重新预览")
	}
	bySource := map[string]batchOperationItem{}
	for _, item := range response.Items {
		bySource[item.SourceID] = item
	}
	response.Items = nil
	for _, id := range plan.IDs {
		item, found := bySource[id]
		if !found {
			return response, fmt.Errorf("执行器发现未展示的子代理，请刷新并重新预览")
		}
		response.Items = append(response.Items, item)
		delete(bySource, id)
	}
	if len(bySource) > 0 {
		return response, fmt.Errorf("子代理集合不一致，请重新扫描")
	}
	response.RestrictedPreview = map[string]any{"conversation_ids": plan.IDs, "family": plan.Family, "assets": plan.Assets, "session_thread_relations": plan.Metadata,
		"text_lines": plan.Texts, "sqlite_objects_and_rows": plan.SQLitePreview, "sqlite_schema_fingerprints": plan.Schemas, "auxiliary_sqlite": plan.Auxiliary,
		"shared_and_protected": plan.Protected, "activity": plan.Activity, "recovery_path": work,
		"permanent_delete": request.PermanentDelete, "purge_recovery_on_success": true, "trash_package": plan.TrashPackage,
		"limitations": "按子代理优先顺序处理完整后代；附件、共享资产或关系不完整时拒绝执行。SQLite 仅删除专属行和目标子代理的关联边。"}
	for index := range response.Items {
		item := &response.Items[index]
		action := lifecycle.ActionTrash
		if request.PermanentDelete {
			action = lifecycle.ActionPermanentDelete
		}
		adapterID := "authorized-local-macos-reviewed"
		compensation := "目标文件、数据库行和索引记录集中移入一个回收文件夹；成功后清理临时备份"
		if config.AllConversations {
			adapterID = "local-idle-macos-reviewed"
		}
		if request.PermanentDelete {
			compensation = "全部成功后清理临时恢复正文；中断时保留材料供人工复核"
		}
		item.Plan = &lifecycle.OperationPlan{OperationID: plan.OperationID, Root: response.SourceRoot, SourceID: item.SourceID,
			ConversationID: item.ConversationID, Status: lifecycle.PlanPrepared, AdapterID: adapterID,
			Actions: []lifecycle.Action{action}, ReadOnly: true, RequiresConfirm: true,
			Compensation: []string{compensation}, Verification: []string{"六库完整性及外键校验", "非目标行事务内摘要不变", "文件操作结果和共享文本校验"}}
		for _, asset := range plan.Assets {
			if asset.ID != item.SourceID {
				continue
			}
			item.Plan.Assets = append(item.Plan.Assets, lifecycle.AssetFingerprint{RelativePath: asset.Relative, SHA256: asset.SHA256, SizeBytes: asset.Bytes, ModTime: time.Unix(0, asset.MTimeNS).UTC()})
			response.Summary.Assets++
			response.Summary.Bytes += asset.Bytes
		}
		if len(item.Plan.Assets) == 0 {
			return response, fmt.Errorf("受限预览未包含目标物理资产")
		}
	}
	response.Summary.Prepared = len(response.Items)
	if err := s.rememberExecutionReview(ctx, &response, request, "", work, digest); err != nil {
		return response, err
	}
	remembered = true
	return response, nil
}
