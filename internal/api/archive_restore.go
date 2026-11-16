// 中文说明：提供本地归档恢复的两阶段 HTTP 入口；预览不写文件，执行密码不进入内存计划或 journal。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/operation"
)

type localArchiveRestorePlan struct {
	Plan      operation.ArchiveRestorePlan
	CreatedAt time.Time
	ExpiresAt time.Time
	Executing bool
}

type localArchiveRestoreRequest struct {
	ArchivePath        string `json:"archive_path,omitempty"`
	TargetRoot         string `json:"target_root,omitempty"`
	SourceOperationID  string `json:"source_operation_id,omitempty"`
	RestoreOperationID string `json:"restore_operation_id,omitempty"`
	ArchivePassword    string `json:"archive_password,omitempty"`
}

func (s *Server) localArchiveRestoreHandler(writer http.ResponseWriter, request *http.Request) {
	suffix := strings.TrimPrefix(request.URL.Path, "/api/v1/archives/local/restore/")
	switch strings.Trim(suffix, "/") {
	case "preview":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.localArchiveRestorePreview(writer, request)
	case "execute":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.localArchiveRestoreExecute(writer, request)
	case "recovery":
		s.localArchiveRestoreRecovery(writer, request)
	default:
		writeError(writer, http.StatusBadRequest, "本地归档恢复路径无效")
	}
}

func (s *Server) localArchiveRestoreRecovery(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		s.localArchiveRestoreRecoveryList(writer, request)
		return
	}
	if request.Method == http.MethodPost {
		s.localArchiveRestoreRecoveryExecute(writer, request)
		return
	}
	writeError(writer, http.StatusMethodNotAllowed, "只支持 GET 或 POST")
}

func (s *Server) localArchiveRestoreRecoveryList(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.lifecycle == nil || strings.TrimSpace(s.lifecycle.JournalRoot) == "" {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": false, "items": []any{}})
		return
	}
	root, err := safeRecoveryRoot(s.lifecycle.JournalRoot)
	if err != nil {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": false, "items": []any{}, "errors": []string{err.Error()}})
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": true, "items": []any{}})
			return
		}
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"read_only": true, "available": true, "error": "读取恢复 journal 目录失败: " + err.Error()})
		return
	}
	items := make([]map[string]any, 0)
	errors := make([]string, 0)
	for _, item := range entries {
		if item.IsDir() || !strings.HasPrefix(item.Name(), "restore-") || filepath.Ext(item.Name()) != ".json" {
			continue
		}
		path := filepath.Join(root, item.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			errors = append(errors, "跳过不安全恢复 journal: "+item.Name())
			continue
		}
		entry, loadErr := lifecycle.LoadRestoreJournal(request.Context(), path)
		if loadErr != nil {
			errors = append(errors, fmt.Sprintf("解析 %s 失败: %v", item.Name(), loadErr))
			continue
		}
		if !lifecycle.RestoreNeedsRecovery(entry) {
			continue
		}
		items = append(items, map[string]any{
			"restore_operation_id": entry.RestoreOperationID, "source_operation_id": entry.SourceOperationID,
			"archive_path": entry.ArchivePath, "target_root": entry.TargetRoot, "phase": entry.Phase,
			"created_at": entry.CreatedAt, "updated_at": entry.UpdatedAt, "asset_count": len(entry.Assets),
			"error_code": entry.ErrorCode, "error_message": entry.ErrorMessage, "recovery_note": entry.RecoveryNote,
			"has_control_plane_snapshot": entry.Mutation != nil,
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": true, "items": items, "errors": errors})
}

func (s *Server) localArchiveRestoreRecoveryExecute(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.lifecycle == nil || !s.lifecycleWriteEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "当前 daemon 未启用生命周期写执行；默认只读"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input localArchiveRestoreRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "恢复 journal 续接请求不是有效 JSON")
		return
	}
	operationID := strings.TrimSpace(input.RestoreOperationID)
	if !safeRestoreOperationID(operationID) {
		writeError(writer, http.StatusBadRequest, "恢复操作 ID 无效")
		return
	}
	root, err := safeRecoveryRoot(s.lifecycle.JournalRoot)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	journalPath := filepath.Join(root, "restore-"+operationID+".json")
	if !pathWithinOrEqual(root, journalPath) {
		writeError(writer, http.StatusBadRequest, "恢复 journal 路径越出 JournalRoot")
		return
	}
	info, statErr := os.Lstat(journalPath)
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		writeError(writer, http.StatusNotFound, "未找到安全的待恢复 journal")
		return
	}
	entry, err := lifecycle.LoadRestoreJournal(request.Context(), journalPath)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	if _, err := s.validateLocalArchivePath(entry.ArchivePath); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "恢复 journal 的归档路径不安全: " + err.Error()})
		return
	}
	if entry.SourceJournalPath != "" {
		sourceJournal, sourceErr := s.resolveLocalRestoreSourceJournal(entry.SourceOperationID)
		if sourceErr != nil || !samePath(sourceJournal, entry.SourceJournalPath) {
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "恢复 journal 的来源 journal 不安全或不匹配"})
			return
		}
	}
	if entry.SourceJournalPath != "" && !samePath(entry.Plan.Root, s.currentRoot()) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "恢复 journal 所属源根与当前工作区不一致"})
		return
	}
	result, recoverErr := operation.RecoverArchiveRestore(request.Context(), journalPath, operation.RestoreExecutionOptions{ArchivePassword: input.ArchivePassword, ControlPlaneMutation: s.lifecycle.ControlPlaneMutation})
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

func (s *Server) localArchiveRestorePreview(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.lifecycle == nil || strings.TrimSpace(s.lifecycle.ArchiveRoot) == "" || strings.TrimSpace(s.lifecycle.JournalRoot) == "" {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "本地归档恢复必须配置独立 ArchiveRoot 和 JournalRoot"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input localArchiveRestoreRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "本地归档恢复预览请求不是有效 JSON")
		return
	}
	archivePath, err := s.validateLocalArchivePath(input.ArchivePath)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	sourceJournal, err := s.resolveLocalRestoreSourceJournal(input.SourceOperationID)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	target, err := validateLocalRestoreTarget(input.TargetRoot)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	plan, err := operation.PreviewArchiveRestore(request.Context(), archivePath, target, sourceJournal, input.ArchivePassword)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	if sourceJournal != "" && strings.TrimSpace(s.currentRoot()) != "" && !samePath(plan.SourcePlan.Root, s.currentRoot()) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "来源 journal 不属于当前工作区"})
		return
	}
	if !plan.Preview.Ready {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "plan": plan, "conflicts": plan.Preview.Conflicts})
		return
	}
	now := time.Now().UTC()
	stored := &localArchiveRestorePlan{Plan: plan, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	s.localRestoreMu.Lock()
	s.pruneLocalArchiveRestorePlansLocked(now)
	s.localRestorePlans[plan.RestoreOperationID] = stored
	s.localRestoreMu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "blocked": false, "restore_operation_id": plan.RestoreOperationID, "expires_at": stored.ExpiresAt, "plan": plan})
}

func (s *Server) localArchiveRestoreExecute(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.lifecycle == nil || !s.lifecycleWriteEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "当前 daemon 未启用生命周期写执行；默认只读"})
		return
	}
	if pathHasCodexComponent(s.currentRoot()) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "恢复器拒绝真实 .codex 根目录"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input localArchiveRestoreRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "本地归档恢复执行请求不是有效 JSON")
		return
	}
	operationID := strings.TrimSpace(input.RestoreOperationID)
	if !safeRestoreOperationID(operationID) {
		writeError(writer, http.StatusBadRequest, "恢复操作 ID 无效")
		return
	}
	now := time.Now().UTC()
	s.localRestoreMu.Lock()
	s.pruneLocalArchiveRestorePlansLocked(now)
	stored, exists := s.localRestorePlans[operationID]
	if !exists {
		s.localRestoreMu.Unlock()
		writeError(writer, http.StatusNotFound, "恢复预览不存在或已过期")
		return
	}
	if stored.Executing {
		s.localRestoreMu.Unlock()
		writeError(writer, http.StatusConflict, "恢复操作正在执行")
		return
	}
	stored.Executing = true
	plan := stored.Plan
	s.localRestoreMu.Unlock()
	if plan.SourceJournalPath != "" && !samePath(plan.SourcePlan.Root, s.currentRoot()) {
		s.localRestoreMu.Lock()
		stored.Executing = false
		s.localRestoreMu.Unlock()
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "执行时当前工作区已切换，拒绝恢复到错误控制面"})
		return
	}

	result, restoreErr := operation.ExecuteArchiveRestore(request.Context(), plan, s.lifecycle.JournalRoot, operation.RestoreExecutionOptions{ArchivePassword: input.ArchivePassword, ControlPlaneMutation: s.lifecycle.ControlPlaneMutation})
	s.localRestoreMu.Lock()
	delete(s.localRestorePlans, operationID)
	s.localRestoreMu.Unlock()
	if restoreErr != nil {
		status := http.StatusInternalServerError
		if result.NeedsReview {
			status = http.StatusConflict
		}
		writeJSON(writer, status, map[string]any{"read_only": false, "blocked": result.NeedsReview, "restore_operation_id": operationID, "error": restoreErr.Error(), "result": result})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "blocked": false, "restore_operation_id": operationID, "result": result})
}

func (s *Server) validateLocalArchivePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("归档路径不能为空")
	}
	archivePath, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("解析归档路径失败: %w", err)
	}
	archivePath = filepath.Clean(archivePath)
	archiveRoot, err := filepath.Abs(s.lifecycle.ArchiveRoot)
	if err != nil {
		return "", fmt.Errorf("解析 ArchiveRoot 失败: %w", err)
	}
	archiveRoot = filepath.Clean(archiveRoot)
	if pathHasCodexComponent(archivePath) || !pathWithinOrEqual(archiveRoot, archivePath) || archiveRoot == archivePath {
		return "", fmt.Errorf("归档路径必须位于独立 ArchiveRoot 的子目录内")
	}
	info, statErr := os.Lstat(archivePath)
	if statErr != nil {
		return "", fmt.Errorf("读取归档路径失败: %w", statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("归档路径必须是普通目录")
	}
	return archivePath, nil
}

func (s *Server) resolveLocalRestoreSourceJournal(operationID string) (string, error) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return "", nil
	}
	if !safeOperationID(operationID) {
		return "", fmt.Errorf("来源 operation ID 无效")
	}
	root, err := safeRecoveryRoot(s.lifecycle.JournalRoot)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, operationID+".json")
	if !pathWithinOrEqual(root, path) {
		return "", fmt.Errorf("来源 journal 路径越出 JournalRoot")
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("未找到安全的来源 journal")
	}
	return path, nil
}

func validateLocalRestoreTarget(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	target, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("解析恢复目标失败: %w", err)
	}
	target = filepath.Clean(target)
	if pathHasCodexComponent(target) {
		return "", fmt.Errorf("恢复目标不能位于 .codex 路径")
	}
	return target, nil
}

func (s *Server) pruneLocalArchiveRestorePlansLocked(now time.Time) {
	for id, plan := range s.localRestorePlans {
		if plan == nil || now.After(plan.ExpiresAt) {
			delete(s.localRestorePlans, id)
		}
	}
}

func safeRestoreOperationID(value string) bool {
	return safeOperationID(value) && strings.HasPrefix(value, "restore-")
}
