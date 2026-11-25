// 中文说明：提供批量生命周期的逐项计划、统计和执行边界；每个对话仍保留独立 journal 与补偿路径。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/operation"
)

const maxBatchOperationItems = 100

type batchOperationRequest struct {
	ConversationIDs  []string `json:"conversation_ids"`
	Archive          bool     `json:"archive"`
	ArchiveLocal     bool     `json:"archive_local"`
	ArchiveRemote    bool     `json:"archive_remote"`
	Encrypted        bool     `json:"encrypted"`
	ArchivePassword  string   `json:"archive_password,omitempty"`
	Trash            bool     `json:"trash"`
	PermanentDelete  bool     `json:"permanent_delete"`
	ConfirmationCode string   `json:"confirmation_code"`
	SelectionOrigin  string   `json:"selection_origin,omitempty"`
}

type batchOperationItem struct {
	AutomaticallyIncluded bool                           `json:"automatically_included,omitempty"`
	ParentSourceID        string                         `json:"parent_source_id,omitempty"`
	ThreadKind            string                         `json:"thread_kind,omitempty"`
	ConversationID        string                         `json:"conversation_id"`
	SourceID              string                         `json:"source_id,omitempty"`
	Title                 string                         `json:"title,omitempty"`
	DiscoveredAssets      []batchOperationAsset          `json:"discovered_assets"`
	Blocked               bool                           `json:"blocked"`
	Error                 string                         `json:"error,omitempty"`
	Plan                  *lifecycle.OperationPlan       `json:"plan,omitempty"`
	Activity              *activity.Report               `json:"activity,omitempty"`
	AssetGraph            *domain.ConversationAssetGraph `json:"asset_graph,omitempty"`
	Result                *operation.SagaResult          `json:"result,omitempty"`
	Completed             bool                           `json:"completed"`
}

// batchOperationAsset 表示扫描实际发现的资产；发现不等于通过写入门禁，也不代表整个共享文件可移走。
type batchOperationAsset struct {
	RelativePath string             `json:"relative_path"`
	Kind         domain.AssetKind   `json:"kind"`
	SizeBytes    int64              `json:"size_bytes"`
	Status       domain.AssetStatus `json:"status"`
	Owners       int                `json:"owners"`
}

type batchOperationSummary struct {
	RequestedConversations int   `json:"requested_conversations"`
	Conversations          int   `json:"conversations"`
	Prepared               int   `json:"prepared"`
	Blocked                int   `json:"blocked"`
	Assets                 int   `json:"assets"`
	Bytes                  int64 `json:"bytes"`
}

type batchOperationResponse struct {
	ReadOnly          bool                  `json:"read_only"`
	SourceReadOnly    bool                  `json:"source_read_only"`
	Blocked           bool                  `json:"blocked"`
	Error             string                `json:"error,omitempty"`
	PreviewID         string                `json:"preview_id,omitempty"`
	ExecutionScope    string                `json:"execution_scope,omitempty"`
	RestrictedPreview any                   `json:"restricted_preview,omitempty"`
	ArchivePreview    *cloudPreview         `json:"archive_preview,omitempty"`
	BatchID           string                `json:"batch_id"`
	Summary           batchOperationSummary `json:"summary"`
	Items             []batchOperationItem  `json:"items"`
	SourceRoot        string                `json:"source_root"`
}

// batchOperationReview 仅暂存用户主动提交的预览，供核对真实 ID；不保存密码，也不授予写入权限。
type batchOperationReview struct {
	ReadOnly        bool                   `json:"read_only"`
	Approval        string                 `json:"approval"`
	SelectionOrigin string                 `json:"selection_origin"`
	CreatedAt       time.Time              `json:"created_at"`
	ExpiresAt       time.Time              `json:"expires_at"`
	Archive         bool                   `json:"archive"`
	Trash           bool                   `json:"trash"`
	PermanentDelete bool                   `json:"permanent_delete"`
	Preview         batchOperationResponse `json:"preview"`
}

func (request batchOperationRequest) operationRequest() operationPreviewRequest {
	return operationPreviewRequest{
		Archive: request.Archive, ArchiveLocal: request.ArchiveLocal, ArchiveRemote: request.ArchiveRemote, Encrypted: request.Encrypted, ArchivePassword: request.ArchivePassword,
		Trash: request.Trash, PermanentDelete: request.PermanentDelete, ConfirmationCode: request.ConfirmationCode,
	}
}

func normalizeBatchConversationIDs(values []string) ([]string, int, error) {
	requested := len(values)
	if requested == 0 {
		return nil, 0, fmt.Errorf("批量操作至少需要一个对话")
	}
	seen := make(map[string]struct{}, requested)
	ids := make([]string, 0, requested)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, 0, fmt.Errorf("批量操作不能包含空对话 ID")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		ids = append(ids, value)
		if len(ids) > maxBatchOperationItems {
			return nil, 0, fmt.Errorf("批量操作最多支持 %d 个对话", maxBatchOperationItems)
		}
	}
	return ids, requested, nil
}

func newBatchOperationID() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("生成批量操作 ID 失败: %w", err)
	}
	return "batch-" + hex.EncodeToString(random[:]), nil
}

func (s *Server) buildBatchOperationPlans(ctx context.Context, request batchOperationRequest) (batchOperationResponse, error) {
	if request.Archive && !request.ArchiveLocal && !request.ArchiveRemote {
		return batchOperationResponse{}, fmt.Errorf("归档至少选择一个保存位置")
	}
	ids, requested, err := normalizeBatchConversationIDs(request.ConversationIDs)
	if err != nil {
		return batchOperationResponse{}, err
	}
	workspace := s.currentWorkspace()
	root := workspace.Definition.Root
	if strings.TrimSpace(root) == "" {
		return batchOperationResponse{}, fmt.Errorf("尚未配置只读扫描根目录")
	}
	if request.Trash || request.PermanentDelete {
		if err := s.requireSourceChanges(root); err != nil {
			return batchOperationResponse{}, err
		}
	}
	report, err := s.scanner.Scan(ctx, root)
	if err != nil {
		return batchOperationResponse{}, err
	}
	batchID, err := newBatchOperationID()
	if err != nil {
		return batchOperationResponse{}, err
	}
	response := batchOperationResponse{
		ReadOnly: true, SourceReadOnly: true, BatchID: batchID,
		SourceRoot: root,
		Items:      make([]batchOperationItem, 0, len(ids)),
		Summary:    batchOperationSummary{RequestedConversations: requested, Conversations: len(ids)},
	}
	request.ConversationIDs = ids
	if request.Archive && s.cloudBackupSupported() {
		return s.buildArchiveBatch(ctx, request, report, response)
	}
	if s.lifecycle != nil && s.lifecycle.Enabled && s.lifecycle.AuthorizedLive != nil && !s.lifecycle.FixtureOnly {
		return s.buildRestrictedBatch(ctx, request, report, response)
	}
	requestedSet := map[string]bool{}
	for _, id := range ids {
		requestedSet[id] = true
	}
	// 旧稳定副本的控制面适配器仍按自己的快照边界执行；文件 fixture 用于验证级联确认。
	if s.lifecycle != nil && s.lifecycle.FixtureOnly && s.lifecycle.ControlPlaneMutation == nil && (request.Trash || request.PermanentDelete) {
		ids, err = expandFamilyTargets(report.Conversations, ids)
		if err != nil {
			return response, err
		}
		response.Summary.Conversations = len(ids)
	}
	code, err := randomConfirmationCode()
	if err != nil {
		return response, err
	}
	request.ConfirmationCode = code
	adapterResolution := compat.ResolveInventory(report, s.registry)
	descriptor := adapterResolution.Descriptor
	if s.lifecycle != nil && s.lifecycle.Enabled && s.lifecycle.FixtureOnly {
		descriptor = s.lifecycleDescriptor()
	}
	seenAssets := make(map[string]struct{})
	for _, conversationID := range ids {
		item := batchOperationItem{ConversationID: conversationID}
		conversation, assets, assetOwners, found := conversationAssets(report, conversationID)
		if !found {
			item.Blocked = true
			item.Error = "未找到指定对话"
			response.Summary.Blocked++
			response.Items = append(response.Items, item)
			continue
		}
		item.Title = conversation.Title
		item.AutomaticallyIncluded = !requestedSet[conversationID]
		item.ParentSourceID, item.ThreadKind = conversation.ParentThreadID, conversation.ThreadKind
		item.SourceID = conversation.SourceID
		item.AssetGraph = conversation.AssetGraph
		conversation, assets, activityReport, activityErr := s.probeConversationActivity(ctx, report, conversation, assets)
		item.Activity = &activityReport
		item.AssetGraph = conversation.AssetGraph
		item.DiscoveredAssets = make([]batchOperationAsset, 0, len(assets))
		for _, asset := range assets {
			item.DiscoveredAssets = append(item.DiscoveredAssets, batchOperationAsset{
				RelativePath: asset.RelativePath, Kind: asset.Kind, SizeBytes: asset.SizeBytes,
				Status: asset.Status, Owners: assetOwners[asset.RelativePath],
			})
			if _, exists := seenAssets[asset.RelativePath]; !exists {
				seenAssets[asset.RelativePath] = struct{}{}
				response.Summary.Assets++
				response.Summary.Bytes += asset.SizeBytes
			}
		}
		if activityErr != nil {
			item.Blocked = true
			item.Error = "活动探测失败，拒绝生成写入计划: " + activityErr.Error()
			response.Summary.Blocked++
			response.Items = append(response.Items, item)
			continue
		}
		plan, planErr := lifecycle.BuildPlan(ctx, root, conversation, assets, descriptor, lifecycle.PlanOptions{
			Archive: request.Archive, Encrypted: request.Encrypted, Trash: request.Trash,
			PermanentDelete: request.PermanentDelete, ConfirmationCode: request.ConfirmationCode, AssetOwners: assetOwners,
		})
		item.Plan = &plan
		if planErr != nil {
			item.Blocked = true
			item.Error = planErr.Error()
			if sharedControlPlaneBlocked(assets, assetOwners) {
				item.Error = "共享数据库或索引仍被占用，关联记录暂不可修改；这不等于会话正文仍在生成。" + item.Error
			}
			response.Summary.Blocked++
			response.Items = append(response.Items, item)
			continue
		}
		response.Summary.Prepared++
		response.Items = append(response.Items, item)
	}
	response.Blocked = response.Summary.Blocked > 0 || response.Summary.Prepared == 0
	if err := s.rememberExecutionReview(ctx, &response, request, code, "", ""); err != nil {
		return response, err
	}
	return response, nil
}

func sharedControlPlaneBlocked(assets []domain.FileInventory, owners map[string]int) bool {
	blocked := false
	for _, asset := range assets {
		if asset.Status == domain.StatusStable && !asset.ChangedDuringScan {
			continue
		}
		if asset.Status != domain.StatusActiveWriting || owners[asset.RelativePath] <= 1 || (asset.Kind != domain.AssetKindSQLite && asset.Kind != domain.AssetKindSessionIndex) {
			return false
		}
		blocked = true
	}
	return blocked
}

func (s *Server) batchOperationPreviewHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input batchOperationRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "批量操作预览请求不是有效 JSON")
		return
	}
	response, err := s.buildBatchOperationPlans(request.Context(), input)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	// 预览中即使存在阻断，也保留明确的选择范围；该记录不进入执行器或授权清单。
	now := time.Now().UTC()
	origin := "user_selection"
	if input.SelectionOrigin == "reconstructed_from_report" {
		origin = input.SelectionOrigin
	}
	s.batchReviewMu.Lock()
	s.batchReview = &batchOperationReview{
		ReadOnly: true, Approval: "required", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
		SelectionOrigin: origin,
		Archive:         input.Archive, Trash: input.Trash, PermanentDelete: input.PermanentDelete, Preview: response,
	}
	s.batchReviewMu.Unlock()
	status := http.StatusOK
	if response.Blocked {
		status = http.StatusConflict
	}
	writeJSON(writer, status, response)
}

// latestBatchOperationPreviewHandler 只读返回当前工作区最近的预览，进程重启或过期后必须重新提交。
func (s *Server) latestBatchOperationPreviewHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	root := s.currentWorkspace().Definition.Root
	s.batchReviewMu.RLock()
	review := s.batchReview
	s.batchReviewMu.RUnlock()
	if review == nil || review.Preview.SourceRoot != root || !time.Now().Before(review.ExpiresAt) {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "error": "当前工作区没有有效的批量预览，请在保留选择的页面点击影响预览"})
		return
	}
	writeJSON(writer, http.StatusOK, review)
}

// batchOperationExecuteHandler 与单条操作共用一次性确认和十项分批任务。
func (s *Server) batchOperationExecuteHandler(writer http.ResponseWriter, request *http.Request) {
	s.confirmedExecutionHandler(writer, request)
}
