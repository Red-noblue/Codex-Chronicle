// 中文说明：实现运行时生命周期操作的持久化暂存、连续空闲探测和人工重新确认。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
	pendingpkg "codex-chronicle.local/internal/pending"
)

const pendingRequiredWindows = 3

type pendingOperationResponse struct {
	ReadOnly       bool                 `json:"read_only"`
	SourceReadOnly bool                 `json:"source_read_only"`
	Expired        bool                 `json:"expired"`
	RequiresReview bool                 `json:"requires_review"`
	Operation      pendingpkg.Operation `json:"operation"`
	Error          string               `json:"error,omitempty"`
	Idempotent     bool                 `json:"idempotent,omitempty"`
}

type pendingOperationCreateRequest struct {
	operationPreviewRequest
	Kind string `json:"kind,omitempty"`
}

type pendingOperationConfirmRequest struct {
	ConfirmationCode string `json:"confirmation_code"`
	ArchivePassword  string `json:"archive_password,omitempty"`
}

type pendingObservation struct {
	report           domain.InventoryReport
	conversation     domain.ConversationSummary
	assets           []domain.FileInventory
	assetOwners      map[string]int
	activity         activity.Report
	fingerprints     []lifecycle.AssetFingerprint
	idle             bool
	reasons          []string
	fingerprintErr   error
	structuralReview bool
}

// pendingStoreForRoot 返回与源根绑定的独立状态存储；该目录不会位于源 .codex 内。
func (s *Server) pendingStoreForRoot(sourceRoot string) (*pendingpkg.Store, error) {
	root := strings.TrimSpace(sourceRoot)
	if root == "" {
		return nil, fmt.Errorf("pending 操作缺少源根")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("解析 pending 源根失败: %w", err)
	}
	key := filepath.Clean(abs)
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if store := s.pendingStores[key]; store != nil {
		return store, nil
	}
	store, err := pendingpkg.NewStore(pendingpkg.DefaultRoot(abs, ""))
	if err != nil {
		return nil, err
	}
	s.pendingStores[key] = store
	return store, nil
}

func (s *Server) currentPendingStore() (*pendingpkg.Store, error) {
	workspace := s.currentWorkspace()
	if strings.TrimSpace(workspace.Definition.Root) == "" {
		return nil, fmt.Errorf("尚未配置工作区源根")
	}
	if workspace.Archive != nil && workspace.Archive.Path() != ":memory:" {
		s.pendingMu.Lock()
		defer s.pendingMu.Unlock()
		key, _ := filepath.Abs(filepath.Clean(workspace.Definition.Root))
		if store := s.pendingStores[key]; store != nil {
			return store, nil
		}
		store, err := pendingpkg.NewStore(pendingpkg.DefaultRoot(workspace.Definition.Root, workspace.Archive.Path()))
		if err != nil {
			return nil, err
		}
		s.pendingStores[key] = store
		return store, nil
	}
	return s.pendingStoreForRoot(workspace.Definition.Root)
}

func pendingStrategy(input pendingOperationCreateRequest) pendingpkg.Strategy {
	return pendingpkg.Strategy{Archive: input.Archive, Encrypted: input.Encrypted, Trash: input.Trash, PermanentDelete: input.PermanentDelete}
}

func pendingStrategyValid(strategy pendingpkg.Strategy, kind string) error {
	if strategy.Encrypted && !strategy.Archive {
		return fmt.Errorf("加密归档必须同时选择归档")
	}
	if !strategy.Archive && !strategy.Trash && !strategy.PermanentDelete {
		if strings.EqualFold(strings.TrimSpace(kind), "repair") || strings.EqualFold(strings.TrimSpace(kind), "cleanup") || strings.EqualFold(strings.TrimSpace(kind), "maintenance") {
			return nil
		}
		return fmt.Errorf("暂存操作至少需要选择归档、回收站或永久删除之一")
	}
	if strategy.Trash && strategy.PermanentDelete {
		return fmt.Errorf("回收站和永久删除不能同时选择")
	}
	return nil
}

func pendingOperationAssets(fingerprints []lifecycle.AssetFingerprint, files []domain.FileInventory) []pendingpkg.SourceAsset {
	byPath := make(map[string]pendingpkg.SourceAsset, len(fingerprints)+len(files))
	for _, fingerprint := range fingerprints {
		byPath[fingerprint.RelativePath] = pendingpkg.SourceAsset{RelativePath: fingerprint.RelativePath, Kind: fingerprint.Kind, SizeBytes: fingerprint.SizeBytes, ModTime: fingerprint.ModTime, SHA256: fingerprint.SHA256, ControlPlaneOnly: fingerprint.ControlPlaneOnly}
	}
	for _, file := range files {
		if _, exists := byPath[file.RelativePath]; exists {
			continue
		}
		byPath[file.RelativePath] = pendingpkg.SourceAsset{RelativePath: file.RelativePath, Kind: file.Kind, SizeBytes: file.SizeBytes, ModTime: file.ModTime}
	}
	assets := make([]pendingpkg.SourceAsset, 0, len(byPath))
	for _, asset := range byPath {
		assets = append(assets, asset)
	}
	sort.Slice(assets, func(left, right int) bool { return assets[left].RelativePath < assets[right].RelativePath })
	return assets
}

func pendingAssetHash(assets []pendingpkg.SourceAsset, conversationHash string) string {
	if strings.TrimSpace(conversationHash) != "" {
		return conversationHash
	}
	return pendingpkg.HashAssetSet(assets)
}

func pendingBlockReasons(probe activity.Report, fingerprintErr error) []string {
	reasons := make([]string, 0)
	if fingerprintErr != nil {
		reasons = append(reasons, "无法建立完整源文件指纹："+fingerprintErr.Error())
	}
	for _, result := range probe.Results {
		if !result.MutationBlocked && result.Status == domain.StatusStable {
			continue
		}
		reason := result.RelativePath + "：" + string(result.Status)
		if result.Evidence.Reason != "" {
			reason += "，" + result.Evidence.Reason
		} else if result.Error != "" {
			reason += "，" + result.Error
		}
		reasons = append(reasons, reason)
	}
	if len(probe.Results) == 0 {
		reasons = append(reasons, "没有可验证的目标资产")
	}
	return uniquePendingStrings(reasons)
}

func uniquePendingStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func pendingActivityIdle(probe activity.Report, requiredWindows int) bool {
	if requiredWindows <= 0 {
		requiredWindows = pendingRequiredWindows
	}
	if probe.WindowCount < requiredWindows || len(probe.Windows) < requiredWindows || len(probe.Results) == 0 {
		return false
	}
	for _, window := range probe.Windows {
		if window.ActiveCount != 0 || window.BlockedCount != 0 || window.ChangedCount != 0 {
			return false
		}
	}
	for _, result := range probe.Results {
		if result.Status != domain.StatusStable || result.MutationBlocked || result.Evidence.LifecycleBlocked || result.Evidence.JSONLPendingLine || result.Evidence.SQLiteBusy {
			return false
		}
	}
	return true
}

func (s *Server) observePending(ctx context.Context, operation pendingpkg.Operation) (pendingObservation, error) {
	if strings.TrimSpace(operation.SourceRoot) == "" || strings.TrimSpace(operation.ConversationID) == "" {
		return pendingObservation{}, fmt.Errorf("暂存操作缺少源根或对话 ID")
	}
	report, err := s.scanner.Scan(ctx, operation.SourceRoot)
	if err != nil {
		return pendingObservation{}, err
	}
	conversation, assets, owners, found := conversationAssets(report, operation.ConversationID)
	if !found {
		return pendingObservation{}, fmt.Errorf("未找到暂存操作关联的对话")
	}
	baseConversation := conversation
	requiredWindows := operation.RequiredWindowCount
	if requiredWindows <= 0 {
		requiredWindows = pendingRequiredWindows
	}
	conversation, assets, probe, err := s.probeConversationActivityWindows(ctx, report, conversation, assets, requiredWindows)
	if err != nil {
		return pendingObservation{}, err
	}
	fingerprints, fingerprintErr := lifecycle.CaptureAssetFingerprints(ctx, report.Root, assets)
	currentAssets := pendingOperationAssets(fingerprints, assets)
	conversationHash := conversation.AssetSetHash
	if strings.TrimSpace(conversationHash) == "" {
		conversationHash = pendingpkg.HashAssetSet(currentAssets)
	}
	observation := pendingObservation{
		report: report, conversation: conversation, assets: assets, assetOwners: owners,
		activity: probe, fingerprints: fingerprints, fingerprintErr: fingerprintErr,
		idle:    fingerprintErr == nil && pendingActivityIdle(probe, requiredWindows),
		reasons: pendingBlockReasons(probe, fingerprintErr),
	}
	if baseConversation.Status != domain.StatusStable || baseConversation.Orphan {
		observation.structuralReview = true
		observation.reasons = append(observation.reasons, "对话关系或兼容状态不是 stable，需要人工复核")
	}
	if len(currentAssets) == 0 {
		observation.reasons = append(observation.reasons, "没有可保存的源资产")
	}
	_ = conversationHash
	return observation, nil
}

func pendingAssetsChanged(previous []pendingpkg.SourceAsset, current []pendingpkg.SourceAsset) bool {
	if len(previous) != len(current) {
		return true
	}
	for index := range previous {
		left, right := previous[index], current[index]
		if left.RelativePath != right.RelativePath || left.Kind != right.Kind || left.SizeBytes != right.SizeBytes || !left.ModTime.Equal(right.ModTime) || left.SHA256 != right.SHA256 || left.ControlPlaneOnly != right.ControlPlaneOnly {
			return true
		}
	}
	return false
}

func applyPendingObservation(operation *pendingpkg.Operation, observation pendingObservation, compareAssets bool) {
	if operation == nil {
		return
	}
	current := pendingOperationAssets(observation.fingerprints, observation.assets)
	changed := compareAssets && pendingAssetsChanged(operation.AssetSet, current)
	if operation.AssetSetHash != "" && observation.conversation.AssetSetHash != "" && operation.AssetSetHash != observation.conversation.AssetSetHash {
		changed = true
	}
	operation.AssetSet = current
	operation.AssetSetHash = pendingAssetHash(current, observation.conversation.AssetSetHash)
	operation.LastProbeAt = observation.activity.GeneratedAt
	operation.ProbeWindowCount = observation.activity.WindowCount
	operation.LastActivity = &observation.activity
	operation.BlockReasons = append([]string(nil), observation.reasons...)
	if observation.structuralReview {
		operation.State = pendingpkg.StateNeedsReview
		operation.Preview = nil
		return
	}
	if changed {
		operation.State = pendingpkg.StateRevalidationFailed
		operation.BlockReasons = append(operation.BlockReasons, "资产集合、文件身份或完整哈希与暂存基线不一致，需要重新生成预览")
		operation.Preview = nil
		return
	}
	if !observation.idle {
		operation.State = pendingpkg.StateWaitingForIdle
		operation.Preview = nil
		return
	}
	operation.State = pendingpkg.StateIdleDetected
	operation.BlockReasons = nil
	operation.Preview = nil
}

func pendingDescriptor(s *Server, report domain.InventoryReport) compat.Descriptor {
	if s.lifecycle != nil && s.lifecycle.Enabled && s.lifecycle.FixtureOnly {
		return s.lifecycleDescriptor()
	}
	return compat.ResolveInventory(report, s.registry).Descriptor
}

func pendingResponse(operation pendingpkg.Operation, err error, idempotent bool) pendingOperationResponse {
	response := pendingOperationResponse{ReadOnly: true, SourceReadOnly: true, Expired: operation.IsExpired(time.Now().UTC()), RequiresReview: operation.State == pendingpkg.StateNeedsReview, Operation: operation, Idempotent: idempotent}
	if err != nil {
		response.Error = err.Error()
	}
	return response
}

// pendingOperationsHandler 管理暂存意图及其状态；所有接口都不直接修改真实 .codex。
func (s *Server) pendingOperationsHandler(writer http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.Path, "/api/v1/operations/pending")
	path = strings.Trim(path, "/")
	if path == "" {
		s.pendingCollectionHandler(writer, request)
		return
	}
	parts := strings.Split(path, "/")
	operationID, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(operationID) == "" {
		writeError(writer, http.StatusBadRequest, "pending operation ID 无效")
		return
	}
	if len(parts) == 1 {
		s.pendingDetailHandler(writer, request, operationID)
		return
	}
	if len(parts) != 2 {
		writeError(writer, http.StatusNotFound, "未知 pending 操作路径")
		return
	}
	switch parts[1] {
	case "refresh", "revalidate":
		s.pendingRefreshHandler(writer, request, operationID)
	case "preview", "repreview":
		s.pendingPreviewHandler(writer, request, operationID)
	case "confirm":
		s.pendingConfirmHandler(writer, request, operationID)
	case "cancel":
		s.pendingCancelHandler(writer, request, operationID)
	default:
		writeError(writer, http.StatusNotFound, "未知 pending 操作动作")
	}
}

func (s *Server) pendingCollectionHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		store, err := s.currentPendingStore()
		if err != nil {
			writeError(writer, http.StatusServiceUnavailable, err.Error())
			return
		}
		items, err := store.List(request.Context())
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err.Error())
			return
		}
		responseItems := make([]pendingOperationResponse, 0, len(items))
		for _, item := range items {
			responseItems = append(responseItems, pendingResponse(item, nil, false))
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "items": responseItems, "count": len(responseItems)})
		return
	}
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET 或 POST")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input pendingOperationCreateRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "暂存操作请求不是有效 JSON")
		return
	}
	strategy := pendingStrategy(input)
	if err := pendingStrategyValid(strategy, input.Kind); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	workspace := s.currentWorkspace()
	root := strings.TrimSpace(workspace.Definition.Root)
	if root == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置工作区源根")
		return
	}
	if strings.TrimSpace(input.ConversationID) == "" {
		writeError(writer, http.StatusBadRequest, "暂存操作必须提供 conversation_id")
		return
	}
	store, err := s.currentPendingStore()
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, err.Error())
		return
	}
	operation, err := pendingpkg.NewOperation(workspace.Definition.ID, root, input.ConversationID, strategy)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	operation.Kind = strings.TrimSpace(input.Kind)
	if operation.Kind == "" {
		operation.Kind = "lifecycle"
	}
	if err := store.Save(request.Context(), operation); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	observation, observeErr := s.observePending(request.Context(), operation)
	if observeErr != nil {
		operation.State = pendingpkg.StateNeedsReview
		operation.BlockReasons = []string{observeErr.Error()}
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusAccepted, pendingResponse(operation, observeErr, false))
		return
	}
	applyPendingObservation(&operation, observation, false)
	if operation.State == pendingpkg.StateIdleDetected {
		// 创建时若已经空闲，仍要求先生成一次新预览并人工确认，避免误把旧页面状态当成同意。
		operation.State = pendingpkg.StateConfirmationRequired
	}
	if err := store.Save(request.Context(), operation); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, pendingResponse(operation, nil, false))
}

func (s *Server) pendingDetailHandler(writer http.ResponseWriter, request *http.Request, operationID string) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	store, err := s.currentPendingStore()
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, err.Error())
		return
	}
	operation, err := store.Get(request.Context(), operationID)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, pendingResponse(operation, nil, false))
}

func (s *Server) loadPendingOperation(ctx context.Context, operationID string) (*pendingpkg.Store, pendingpkg.Operation, error) {
	store, err := s.currentPendingStore()
	if err != nil {
		return nil, pendingpkg.Operation{}, err
	}
	operation, err := store.Get(ctx, operationID)
	if err != nil {
		return nil, pendingpkg.Operation{}, err
	}
	if !samePath(operation.SourceRoot, s.currentRoot()) {
		return nil, operation, fmt.Errorf("暂存操作属于其他工作区，当前工作区不会替它执行复核")
	}
	return store, operation, nil
}

func (s *Server) pendingRefreshHandler(writer http.ResponseWriter, request *http.Request, operationID string) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	store, operation, err := s.loadPendingOperation(request.Context(), operationID)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if operation.IsTerminal() {
		writeJSON(writer, http.StatusOK, pendingResponse(operation, nil, true))
		return
	}
	observation, err := s.observePending(request.Context(), operation)
	if err != nil {
		operation.State = pendingpkg.StateNeedsReview
		operation.BlockReasons = []string{err.Error()}
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, err, false))
		return
	}
	applyPendingObservation(&operation, observation, true)
	if err := store.Save(request.Context(), operation); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	if operation.State == pendingpkg.StateWaitingForIdle || operation.State == pendingpkg.StateRevalidationFailed {
		status = http.StatusConflict
	}
	writeJSON(writer, status, pendingResponse(operation, nil, false))
}

func (s *Server) pendingPreviewHandler(writer http.ResponseWriter, request *http.Request, operationID string) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input operationPreviewRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "暂存预览请求不是有效 JSON")
		return
	}
	store, operation, err := s.loadPendingOperation(request.Context(), operationID)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if operation.IsTerminal() {
		writeJSON(writer, http.StatusOK, pendingResponse(operation, nil, true))
		return
	}
	observation, err := s.observePending(request.Context(), operation)
	if err != nil {
		operation.State = pendingpkg.StateNeedsReview
		operation.BlockReasons = []string{err.Error()}
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, err, false))
		return
	}
	applyPendingObservation(&operation, observation, true)
	if operation.State != pendingpkg.StateIdleDetected {
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, fmt.Errorf("当前尚未满足连续空闲条件"), false))
		return
	}
	if operation.Kind != "" && operation.Kind != "lifecycle" {
		operation.State = pendingpkg.StateNeedsReview
		operation.BlockReasons = []string{"暂存的" + operation.Kind + "意图已保存；当前 Goal 只提供生命周期预览，修复或清理 adapter 仍需后续接入"}
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, fmt.Errorf("当前尚未接入 %s adapter", operation.Kind), false))
		return
	}
	descriptor := pendingDescriptor(s, observation.report)
	plan, planErr := lifecycle.BuildPlan(request.Context(), observation.report.Root, observation.conversation, observation.assets, descriptor, lifecycle.PlanOptions{
		Archive: input.Archive || operation.Strategy.Archive, Encrypted: input.Encrypted || operation.Strategy.Encrypted,
		Trash: input.Trash || operation.Strategy.Trash, PermanentDelete: input.PermanentDelete || operation.Strategy.PermanentDelete,
		ConfirmationCode: input.ConfirmationCode, AssetOwners: observation.assetOwners,
	})
	if planErr != nil {
		operation.State = pendingpkg.StateNeedsReview
		operation.BlockReasons = []string{planErr.Error()}
		operation.Preview = &plan
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, planErr, false))
		return
	}
	operation.Preview = &plan
	operation.State = pendingpkg.StateConfirmationRequired
	operation.Strategy = pendingpkg.Strategy{Archive: planHasAction(plan, lifecycle.ActionArchive, lifecycle.ActionEncryptedArchive), Encrypted: planHasAction(plan, lifecycle.ActionEncryptedArchive), Trash: planHasAction(plan, lifecycle.ActionTrash), PermanentDelete: planHasAction(plan, lifecycle.ActionPermanentDelete)}
	operation.BlockReasons = nil
	if err := store.Save(request.Context(), operation); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, pendingResponse(operation, nil, false))
}

func planHasAction(plan lifecycle.OperationPlan, actions ...lifecycle.Action) bool {
	for _, current := range plan.Actions {
		for _, wanted := range actions {
			if current == wanted {
				return true
			}
		}
	}
	return false
}

func (s *Server) pendingConfirmHandler(writer http.ResponseWriter, request *http.Request, operationID string) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input pendingOperationConfirmRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "暂存确认请求不是有效 JSON")
		return
	}
	store, operation, err := s.loadPendingOperation(request.Context(), operationID)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if operation.State == pendingpkg.StateReadyForFixtureExecution || operation.State == pendingpkg.StateCompleted {
		writeJSON(writer, http.StatusOK, pendingResponse(operation, nil, true))
		return
	}
	if operation.State != pendingpkg.StateConfirmationRequired || operation.Preview == nil {
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, fmt.Errorf("暂存操作尚未生成可确认的最新预览"), false))
		return
	}
	if operation.IsExpired(time.Now().UTC()) {
		operation.State = pendingpkg.StateNeedsReview
		operation.BlockReasons = []string{"暂存操作已过期，需要重新创建审核意图"}
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, fmt.Errorf("暂存操作已过期"), false))
		return
	}
	preview := operation.Preview
	observation, err := s.observePending(request.Context(), operation)
	if err != nil {
		operation.State = pendingpkg.StateNeedsReview
		operation.BlockReasons = []string{err.Error()}
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, err, false))
		return
	}
	applyPendingObservation(&operation, observation, true)
	if operation.State != pendingpkg.StateIdleDetected {
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, fmt.Errorf("重新确认前检测到活动或资产变化"), false))
		return
	}
	// 重新探测保持原预览引用，随后由 RevalidatePlan 再次核对其完整指纹。
	operation.Preview = preview
	if operation.Preview == nil {
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, fmt.Errorf("暂存操作缺少可重新确认的预览"), false))
		return
	}
	if err := lifecycle.RevalidatePlan(request.Context(), *operation.Preview); err != nil {
		operation.State = pendingpkg.StateRevalidationFailed
		operation.BlockReasons = []string{err.Error()}
		operation.Preview = nil
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, err, false))
		return
	}
	if operation.Preview.RequiresConfirm && !lifecycle.VerifyConfirmation(*operation.Preview, strings.TrimSpace(input.ConfirmationCode)) {
		operation.State = pendingpkg.StateConfirmationRequired
		operation.BlockReasons = []string{"确认数字与本次预览不匹配"}
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, fmt.Errorf("确认数字不正确"), false))
		return
	}
	if s.lifecycle == nil || !s.lifecycle.Enabled || !s.lifecycle.FixtureOnly || pathHasCodexComponent(operation.SourceRoot) {
		operation.State = pendingpkg.StateNeedsReview
		operation.BlockReasons = []string{"当前 adapter 只读或源根是真实 .codex；暂存操作不能进入 fixture 执行阶段"}
		_ = store.Save(request.Context(), operation)
		writeJSON(writer, http.StatusConflict, pendingResponse(operation, fmt.Errorf("当前 adapter 不允许进入 fixture 执行阶段"), false))
		return
	}
	operation.Preview.Status = lifecycle.PlanConfirmed
	operation.State = pendingpkg.StateReadyForFixtureExecution
	operation.BlockReasons = nil
	operation.LastReconfirmedAt = time.Now().UTC()
	if err := store.Save(request.Context(), operation); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, pendingResponse(operation, nil, false))
}

func (s *Server) pendingCancelHandler(writer http.ResponseWriter, request *http.Request, operationID string) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	store, operation, err := s.loadPendingOperation(request.Context(), operationID)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if operation.State == pendingpkg.StateCancelled || operation.State == pendingpkg.StateCompleted {
		writeJSON(writer, http.StatusOK, pendingResponse(operation, nil, true))
		return
	}
	operation.State = pendingpkg.StateCancelled
	operation.BlockReasons = []string{"用户取消暂存操作；审核记录保留在独立 pending 存储中"}
	if err := store.Save(request.Context(), operation); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, pendingResponse(operation, nil, false))
}
