// 中文说明：提供生成物清理 registry 的只读候选、预览、执行、恢复和 retention API。
// 文件清理与恢复仍要求显式 fixture 模式；管理开关和保留设置仅写独立登记区，真实 .codex 和未知对象只报告。
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

	"codex-chronicle.local/internal/maintenance"
)

type cleanupSelectionRequest struct {
	ArtifactIDs []string `json:"artifact_ids"`
}

type cleanupOperationRequest struct {
	OperationID string `json:"operation_id"`
}

type cleanupRetentionRequest struct {
	Policy maintenance.RetentionPolicy `json:"policy"`
}

type cleanupRegisterRequest struct {
	Manifest maintenance.ArtifactManifest `json:"manifest"`
}

func (s *Server) cleanupRoot() string {
	if s == nil || s.lifecycle == nil {
		return ""
	}
	if strings.TrimSpace(s.lifecycle.CleanupRoot) != "" {
		return s.lifecycle.CleanupRoot
	}
	if strings.TrimSpace(s.lifecycle.JournalRoot) != "" {
		return filepath.Join(s.lifecycle.JournalRoot, "cleanup-registry")
	}
	return ""
}

func (s *Server) cleanupRegistry() (*maintenance.Registry, error) {
	root := s.cleanupRoot()
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("尚未配置独立 cleanup registry 根目录")
	}
	return maintenance.OpenRegistry(root)
}

func (s *Server) cleanupFixtureEnabled() bool {
	return s != nil && s.lifecycle != nil && s.lifecycle.Enabled && s.lifecycle.FixtureOnly && strings.TrimSpace(s.cleanupRoot()) != "" && !pathHasCodexComponent(s.cleanupRoot())
}

// cleanupArtifactRegistrar 为启用管理后的新任务登记中间产物；已开始任务保留其回调以完成登记，不扩大源目录写权限。
func (s *Server) cleanupArtifactRegistrar() func(context.Context, maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error) {
	registry, err := s.cleanupSettingsRegistry()
	if err != nil {
		return nil
	}
	settings, err := registry.LoadManagementSettings(s.cleanupFixtureEnabled())
	if err != nil || !settings.Enabled {
		return nil
	}
	return func(ctx context.Context, manifest maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error) {
		registry, err := s.cleanupSettingsRegistry()
		if err != nil {
			return maintenance.ArtifactManifest{}, err
		}
		stored, _, err := registry.RegisterArtifact(ctx, manifest)
		return stored, err
	}
}

type cleanupManagementStatus struct {
	Enabled             bool   `json:"enabled"`
	SettingsWritable    bool   `json:"settings_writable"`
	PolicyWriteEnabled  bool   `json:"policy_write_enabled"`
	RegistrationEnabled bool   `json:"registration_enabled"`
	AutomaticCleanup    bool   `json:"automatic_cleanup_enabled"`
	BlockReason         string `json:"block_reason,omitempty"`
}

// cleanupSettingsRegistry 只允许配置服务端指定、位于所有扫描根之外的登记区。
func (s *Server) cleanupSettingsRegistry() (*maintenance.Registry, error) {
	registry, err := s.cleanupRegistry()
	if err != nil {
		return nil, err
	}
	// 解析已存在的父目录，避免登记区经符号链接落入 .codex 或其他工作区。
	existing, suffix := registry.Root, ""
	for {
		_, err := os.Lstat(existing)
		if !os.IsNotExist(err) {
			if err != nil {
				return nil, err
			}
			break
		}
		suffix = filepath.Join(filepath.Base(existing), suffix)
		existing = filepath.Dir(existing)
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return nil, err
	}
	registry, err = maintenance.OpenRegistry(filepath.Join(resolved, suffix))
	if err != nil {
		return nil, err
	}
	for _, root := range s.cleanupRoots() {
		if resolvedRoot, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
			root = resolvedRoot
		}
		if pathWithinOrEqual(root, registry.Root) {
			return nil, fmt.Errorf("中间产物登记区必须位于工作区源目录之外")
		}
	}
	return registry, nil
}

func (s *Server) cleanupManagement() cleanupManagementStatus {
	registry, err := s.cleanupSettingsRegistry()
	if err != nil {
		return cleanupManagementStatus{BlockReason: err.Error()}
	}
	settings, err := registry.LoadManagementSettings(s.cleanupFixtureEnabled())
	if err != nil {
		return cleanupManagementStatus{BlockReason: err.Error()}
	}
	return cleanupManagementStatus{Enabled: settings.Enabled, SettingsWritable: true, PolicyWriteEnabled: settings.Enabled, RegistrationEnabled: settings.Enabled}
}

// cleanupSettingsHandler 只保存登记和保留规则的开关；启用不会自动删除任何文件。
func (s *Server) cleanupSettingsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		writeJSON(writer, http.StatusOK, s.cleanupManagement())
		return
	}
	if request.Method != http.MethodPut {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET 或 PUT")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 4096)
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Enabled == nil {
		writeError(writer, http.StatusBadRequest, "请明确选择是否启用中间产物管理")
		return
	}
	s.cleanupSettingsMu.Lock()
	defer s.cleanupSettingsMu.Unlock()
	registry, err := s.cleanupSettingsRegistry()
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	if _, err := registry.LoadManagementSettings(s.cleanupFixtureEnabled()); err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	if err := registry.SaveManagementSettings(request.Context(), *input.Enabled); err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	if s.updater != nil {
		s.updater.SetArtifactRegistrar(s.cleanupArtifactRegistrar())
	}
	s.refreshCleanupCatalog(request.Context())
	writeJSON(writer, http.StatusOK, s.cleanupManagement())
}

func (s *Server) cleanupRoots() []string {
	roots := make([]string, 0)
	if current := strings.TrimSpace(s.currentRoot()); current != "" {
		roots = append(roots, current)
	}
	if registry, persisted := s.workspaceDefinitions(); persisted {
		for _, definition := range registry.Workspaces {
			if strings.TrimSpace(definition.Root) != "" {
				roots = append(roots, definition.Root)
			}
		}
	}
	return uniqueCleanupPaths(roots)
}

func uniqueCleanupPaths(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		absolute, err := filepath.Abs(filepath.Clean(value))
		if err != nil || strings.TrimSpace(value) == "" {
			continue
		}
		if _, exists := seen[absolute]; exists {
			continue
		}
		seen[absolute] = struct{}{}
		result = append(result, absolute)
	}
	return result
}

func (s *Server) cleanupScanOptions() maintenance.CleanupOptions {
	roots := s.cleanupRoots()
	options := maintenance.CleanupOptions{ProtectedRoots: roots, ActiveWorkspaceRoots: roots, ProbeOpenFiles: true}
	if s.lifecycle != nil {
		options.OpenFileProbe = s.lifecycle.CleanupOpenFileProbe
	}
	return options
}

func emptyCleanupReport() maintenance.CleanupReport {
	return maintenance.CleanupReport{ReadOnly: true, Policy: maintenance.DefaultRetentionPolicy(), Candidates: []maintenance.CleanupCandidate{}, Retained: []maintenance.CleanupCandidate{}, Quarantined: []maintenance.CleanupCandidate{}, Unknown: []maintenance.UnknownArtifact{}, CrashResidues: []maintenance.UnknownArtifact{}, Recoveries: []maintenance.CleanupRecovery{}}
}

// refreshCleanupCatalog 在 daemon 启动和每次执行后刷新只读 crash residue/候选快照。
func (s *Server) refreshCleanupCatalog(ctx context.Context) {
	result := emptyCleanupReport()
	registry, err := s.cleanupRegistry()
	if err == nil {
		result, err = registry.Discover(ctx, s.cleanupScanOptions())
	}
	s.cleanupReportMu.Lock()
	s.cleanupReport = result
	s.cleanupReportError = ""
	if err != nil {
		s.cleanupReportError = err.Error()
	}
	s.cleanupScannedAt = time.Now().UTC()
	s.cleanupReportMu.Unlock()
}

func (s *Server) cleanupSnapshot() (maintenance.CleanupReport, string, time.Time) {
	s.cleanupReportMu.RLock()
	defer s.cleanupReportMu.RUnlock()
	return s.cleanupReport, s.cleanupReportError, s.cleanupScannedAt
}

// cleanupHandler 返回候选、保留对象、未知对象和 crash residue；GET 不会创建 registry。
func (s *Server) cleanupHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	s.refreshCleanupCatalog(request.Context())
	report, scanError, scannedAt := s.cleanupSnapshot()
	response := map[string]any{
		"available":     strings.TrimSpace(s.cleanupRoot()) != "" && scanError == "",
		"write_enabled": s.cleanupFixtureEnabled(),
		"read_only":     true,
		"scanned_at":    scannedAt,
		"cleanup":       report,
		"management":    s.cleanupManagement(),
	}
	if scanError != "" {
		response["error"] = scanError
	}
	writeJSON(writer, http.StatusOK, response)
}

// cleanupRegisterHandler 登记明确归属的项目生成物；只允许 fixture registry，未知对象不会因此自动获得清理资格。
func (s *Server) cleanupRegisterHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if !s.cleanupFixtureEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "生成物登记仅允许显式 fixture registry；真实 .codex 和默认 daemon 始终只读"})
		return
	}
	registry, err := s.cleanupRegistry()
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 128<<10)
	var input cleanupRegisterRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "生成物登记请求不是有效 JSON")
		return
	}
	manifest, manifestPath, err := registry.RegisterArtifact(request.Context(), input.Manifest)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": false, "blocked": true, "error": err.Error()})
		return
	}
	s.refreshCleanupCatalog(request.Context())
	writeJSON(writer, http.StatusCreated, map[string]any{"read_only": false, "blocked": false, "manifest": manifest, "manifest_path": manifestPath})
}

// cleanupRetentionHandler 读取或修改独立登记区的保留规则；只要求用户启用管理，不要求源目录清理权限。
func (s *Server) cleanupRetentionHandler(writer http.ResponseWriter, request *http.Request) {
	registry, err := s.cleanupRegistry()
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"read_only": true, "error": err.Error(), "policy": maintenance.DefaultRetentionPolicy()})
		return
	}
	switch request.Method {
	case http.MethodGet:
		management := s.cleanupManagement()
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "write_enabled": management.PolicyWriteEnabled, "policy": registry.Policy, "management": management})
	case http.MethodPut, http.MethodPost:
		s.cleanupSettingsMu.Lock()
		defer s.cleanupSettingsMu.Unlock()
		if !s.cleanupManagement().PolicyWriteEnabled {
			writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "请先启用中间产物管理，再设置保留规则"})
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
		var input cleanupRetentionRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			writeError(writer, http.StatusBadRequest, "retention 请求不是有效 JSON")
			return
		}
		if input.Policy.Version != 1 || !input.Policy.UnknownReportOnly {
			writeError(writer, http.StatusBadRequest, "保留规则版本不兼容，未登记对象必须保持仅报告")
			return
		}
		for _, seconds := range []int64{input.Policy.DefaultTTLSeconds, input.Policy.StagingTTLSeconds, input.Policy.FailedTTLSeconds, input.Policy.ExpiredCopyTTLSeconds, input.Policy.RuntimeCacheTTLSeconds, input.Policy.PurgeTTLSeconds} {
			if seconds <= 0 || seconds > 3650*86400 {
				writeError(writer, http.StatusBadRequest, "保留时间必须大于 0 且不超过 3650 天")
				return
			}
		}
		if err := registry.SavePolicy(request.Context(), input.Policy); err != nil {
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": false, "blocked": true, "error": err.Error()})
			return
		}
		s.refreshCleanupCatalog(request.Context())
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "blocked": false, "policy": registry.Policy})
	default:
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET、PUT 或 POST")
	}
}

// cleanupPreviewHandler 生成清理计划，不移动或删除任何对象。
func (s *Server) cleanupPreviewHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	registry, err := s.cleanupRegistry()
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input cleanupSelectionRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "清理预览请求不是有效 JSON")
		return
	}
	report, err := registry.Discover(request.Context(), s.cleanupScanOptions())
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error(), "cleanup": report})
		return
	}
	plan, planErr := registry.BuildPlan(request.Context(), report, maintenance.CleanupPlanOptions{SelectedIDs: input.ArtifactIDs, FixtureOnly: s.cleanupFixtureEnabled(), ProtectedRoots: s.cleanupRoots(), ActiveWorkspaceRoots: s.cleanupRoots()})
	if planErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": planErr.Error(), "cleanup": report, "plan": plan})
		return
	}
	if !plan.WriteEnabled {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "blocked": true, "error": plan.BlockReason, "cleanup": report, "plan": plan})
		return
	}
	s.cleanupMu.Lock()
	s.cleanupPlans[plan.OperationID] = plan
	s.cleanupMu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "blocked": false, "cleanup": report, "plan": plan})
}

// cleanupExecuteHandler 只执行同一 daemon 已预览并缓存的 fixture 计划。
func (s *Server) cleanupExecuteHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if !s.cleanupFixtureEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "生成物清理写入仅允许显式 fixture registry；未知对象和真实 .codex 始终只读"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input cleanupOperationRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || !validCleanupOperationID(input.OperationID) {
		writeError(writer, http.StatusBadRequest, "清理执行必须提供有效 operation_id")
		return
	}
	s.cleanupMu.Lock()
	plan, ok := s.cleanupPlans[input.OperationID]
	s.cleanupMu.Unlock()
	if !ok {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "blocked": true, "error": "清理计划不存在或已过期，请重新预览"})
		return
	}
	registry, err := s.cleanupRegistry()
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	if filepath.Clean(plan.RegistryRoot) != filepath.Clean(registry.Root) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "清理 registry 已切换，请重新预览"})
		return
	}
	result, executeErr := registry.ExecutePlan(request.Context(), plan)
	if executeErr != nil {
		status := http.StatusConflict
		if !result.NeedsReview {
			status = http.StatusInternalServerError
		}
		writeJSON(writer, status, map[string]any{"read_only": false, "blocked": result.NeedsReview, "error": executeErr.Error(), "result": result, "plan": plan})
		return
	}
	s.cleanupMu.Lock()
	delete(s.cleanupPlans, input.OperationID)
	s.cleanupMu.Unlock()
	s.refreshCleanupCatalog(request.Context())
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "blocked": false, "result": result, "plan": plan})
}

// cleanupRecoverHandler 恢复中断的隔离操作；已提交操作保留 journal，不自动逆向撤销。
func (s *Server) cleanupRecoverHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if !s.cleanupFixtureEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "清理恢复仅允许 fixture registry"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input cleanupOperationRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || !validCleanupOperationID(input.OperationID) {
		writeError(writer, http.StatusBadRequest, "清理恢复必须提供有效 operation_id")
		return
	}
	registry, err := s.cleanupRegistry()
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	journalPath := filepath.Join(registry.Root, "journals", input.OperationID+".json")
	result, recoverErr := registry.RecoverPlan(request.Context(), journalPath)
	s.refreshCleanupCatalog(request.Context())
	if recoverErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": false, "blocked": result.NeedsReview, "error": recoverErr.Error(), "result": result})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "blocked": false, "result": result})
}

func validCleanupOperationID(value string) bool {
	return len(value) == len("cleanup-")+32 && strings.HasPrefix(value, "cleanup-") && !strings.ContainsAny(value, `/\\.`)
}
