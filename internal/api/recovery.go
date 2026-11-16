// 中文说明：提供生命周期 journal 的只读清单和受限恢复入口；恢复只接受 daemon 自己的 journal ID。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/operation"
	"codex-chronicle.local/internal/platform"
)

type operationRecoveryItem struct {
	OperationID          string                        `json:"operation_id"`
	ConversationID       string                        `json:"conversation_id,omitempty"`
	SourceID             string                        `json:"source_id,omitempty"`
	Phase                lifecycle.JournalPhase        `json:"phase"`
	CreatedAt            time.Time                     `json:"created_at"`
	UpdatedAt            time.Time                     `json:"updated_at"`
	AssetCount           int                           `json:"asset_count"`
	Bytes                int64                         `json:"bytes"`
	ReceiptCount         int                           `json:"receipt_count"`
	ErrorCode            string                        `json:"error_code,omitempty"`
	ErrorMessage         string                        `json:"error_message,omitempty"`
	RecoveryNote         string                        `json:"recovery_note,omitempty"`
	HasMutation          bool                          `json:"has_control_plane_snapshot"`
	Disposition          operation.RecoveryDisposition `json:"disposition"`
	CanRetry             bool                          `json:"can_retry"`
	CanRollback          bool                          `json:"can_rollback"`
	ExternalStateUnknown bool                          `json:"external_state_unknown"`
	Reason               string                        `json:"reason,omitempty"`
}

type operationRecoveryListResponse struct {
	ReadOnly  bool                    `json:"read_only"`
	Available bool                    `json:"available"`
	Items     []operationRecoveryItem `json:"items"`
	Terminal  []operationRecoveryItem `json:"terminal_items,omitempty"`
	ScannedAt time.Time               `json:"scanned_at,omitempty"`
	Errors    []string                `json:"errors,omitempty"`
}

type operationRecoveryRequest struct {
	OperationID string `json:"operation_id"`
}

func (s *Server) operationRecoveryListHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	response := operationRecoveryListResponse{ReadOnly: true, Available: false, Items: make([]operationRecoveryItem, 0), Terminal: make([]operationRecoveryItem, 0)}
	if s == nil || s.lifecycle == nil || strings.TrimSpace(s.lifecycle.JournalRoot) == "" {
		writeJSON(writer, http.StatusOK, response)
		return
	}
	s.refreshRecoveryCatalog(request.Context())
	s.recoveryMu.RLock()
	candidates := append([]operation.RecoveryCandidate(nil), s.recoveryCandidates...)
	response.Errors = append(response.Errors, s.recoveryErrors...)
	response.ScannedAt = s.recoveryScannedAt
	s.recoveryMu.RUnlock()
	response.Available = true
	includeTerminal := request.URL.Query().Get("include_terminal") == "true"
	for _, candidate := range candidates {
		item := recoveryItem(candidate)
		if lifecycle.NeedsRecovery(candidate.Entry) {
			response.Items = append(response.Items, item)
		} else if includeTerminal {
			response.Terminal = append(response.Terminal, item)
		}
	}
	// 专用执行器保留逐行恢复入口，不交给 fixture saga 猜测处理。
	s.executionMu.Lock()
	response.Errors = append(response.Errors, s.executionLoadErrors...)
	for _, task := range s.executionTasks {
		if !task.Restricted || task.SourceRoot != s.currentWorkspace().Definition.Root || task.RecoveryPath == "" {
			continue
		}
		item := operationRecoveryItem{OperationID: task.ID, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
			Phase: lifecycle.PhaseNeedsReview, ErrorMessage: task.Error, RecoveryNote: task.RecoveryCommand,
			Reason: "受限真实执行恢复包：" + task.RecoveryPath + "；使用专用执行器逐行恢复，禁止整库覆盖或只放回正文。"}
		if task.State == "needs_review" {
			response.Items = append(response.Items, item)
		} else if includeTerminal && task.State == "completed" {
			item.Phase = lifecycle.PhaseCommitted
			response.Terminal = append(response.Terminal, item)
		}
	}
	s.executionMu.Unlock()
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) refreshRecoveryCatalog(ctx context.Context) {
	if s == nil || s.lifecycle == nil || strings.TrimSpace(s.lifecycle.JournalRoot) == "" {
		return
	}
	candidates, errorsFound := operation.DiscoverJournals(ctx, s.lifecycle.JournalRoot)
	s.recoveryMu.Lock()
	s.recoveryCandidates = candidates
	s.recoveryErrors = errorsFound
	s.recoveryScannedAt = time.Now().UTC()
	s.recoveryMu.Unlock()
}

func recoveryItem(candidate operation.RecoveryCandidate) operationRecoveryItem {
	entry := candidate.Entry
	item := operationRecoveryItem{
		OperationID: entry.OperationID, ConversationID: entry.Plan.ConversationID, SourceID: entry.Plan.SourceID,
		Phase: entry.Phase, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt,
		AssetCount: len(entry.Plan.Assets), ReceiptCount: len(entry.Receipts),
		ErrorCode: entry.ErrorCode, ErrorMessage: entry.ErrorMessage, RecoveryNote: entry.RecoveryNote,
		HasMutation: entry.Mutation != nil, Disposition: candidate.Disposition, CanRetry: candidate.CanRetry,
		CanRollback: candidate.CanRollback, ExternalStateUnknown: candidate.ExternalStateUnknown, Reason: candidate.Reason,
	}
	for _, asset := range entry.Plan.Assets {
		if asset.SizeBytes > 0 {
			item.Bytes += asset.SizeBytes
		}
	}
	return item
}

func (s *Server) operationRecoveryExecuteHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s == nil || s.lifecycle == nil || !s.lifecycleWriteEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "当前 daemon 未启用生命周期恢复；默认只读"})
		return
	}
	workspace := s.currentWorkspace()
	root := workspace.Definition.Root
	if pathHasCodexComponent(root) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "恢复器拒绝真实 .codex 根目录"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input operationRecoveryRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "恢复请求不是有效 JSON")
		return
	}
	operationID := strings.TrimSpace(input.OperationID)
	journalRoot, err := safeRecoveryRoot(s.lifecycle.JournalRoot)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	if !safeOperationID(operationID) {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"read_only": true, "blocked": true, "error": "恢复请求的 operation ID 无效"})
		return
	}
	journalPath := filepath.Join(journalRoot, operationID+".json")
	if !pathWithinOrEqual(journalRoot, journalPath) {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"read_only": true, "blocked": true, "error": "journal 路径越出允许目录"})
		return
	}
	info, statErr := os.Lstat(journalPath)
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "blocked": true, "error": "未找到安全的待恢复 journal"})
		return
	}
	entry, err := lifecycle.LoadJournal(request.Context(), journalPath)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	if !samePath(entry.Plan.Root, root) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "journal 所属源根与当前工作区不一致，拒绝恢复"})
		return
	}
	if !lifecycle.NeedsRecovery(entry) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "该 journal 已进入终态，无需恢复", "result": operation.SagaResult{OperationID: entry.OperationID, JournalPath: journalPath, Phase: entry.Phase}})
		return
	}
	candidate := operation.ClassifyJournal(request.Context(), journalPath, entry)
	if !candidate.CanRollback {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "当前 journal 不具备安全自动回滚条件", "disposition": candidate.Disposition, "reason": candidate.Reason})
		return
	}
	trash, err := s.recoveryTrashProvider(request.Context(), root)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	result, recoverErr := operation.RecoverLocalSagaWithOptions(request.Context(), journalPath, trash, s.lifecycle.ControlPlaneMutation)
	s.refreshRecoveryCatalog(request.Context())
	if recoverErr != nil {
		status := http.StatusInternalServerError
		if result.NeedsReview {
			status = http.StatusConflict
		}
		writeJSON(writer, status, map[string]any{"read_only": false, "blocked": result.NeedsReview, "error": recoverErr.Error(), "result": result})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "blocked": false, "result": result})
}

func (s *Server) recoveryTrashProvider(ctx context.Context, sourceRoot string) (platform.TrashProvider, error) {
	if s.lifecycle.TrashProvider != nil {
		return s.lifecycle.TrashProvider, nil
	}
	if s.lifecycle.UseSystemTrash {
		return s.systemTrashProvider(ctx, sourceRoot)
	}
	if strings.TrimSpace(s.lifecycle.TrashRoot) == "" {
		return nil, fmt.Errorf("恢复回收站操作必须配置独立 TrashRoot、系统回收站或注入 TrashProvider")
	}
	return platform.NewDirectoryTrash(s.lifecycle.TrashRoot)
}

func safeRecoveryRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("尚未配置独立 JournalRoot")
	}
	root, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("解析 JournalRoot 失败: %w", err)
	}
	root = filepath.Clean(root)
	if pathHasCodexComponent(root) {
		return "", fmt.Errorf("JournalRoot 不能位于 .codex 路径")
	}
	if info, statErr := os.Lstat(root); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("JournalRoot 不能是符号链接")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", fmt.Errorf("检查 JournalRoot 失败: %w", statErr)
	}
	return root, nil
}

func safeOperationID(value string) bool {
	if strings.TrimSpace(value) == "" || len(value) > 128 || filepath.Base(value) != value || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsAny(value, `/\\`) && !strings.ContainsRune(value, 0)
}
