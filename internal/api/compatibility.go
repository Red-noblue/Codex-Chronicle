// 中文说明：提供工作区格式诊断与持久化访问策略；仅查看策略由服务端约束源数据修改，不影响派生索引和备份导出。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/compat"
)

type sourcePolicy struct {
	Mode      string `json:"mode"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// SetCompatibilityPath 只在监听前加载应用数据目录中的策略；损坏配置拒绝启动，避免意外解除只读限制。
func (s *Server) SetCompatibilityPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil || pathHasCodexComponent(abs) {
		return fmt.Errorf("兼容策略必须保存在源目录之外")
	}
	for _, workspace := range s.workspaceReg.Workspaces {
		if pathsOverlap(workspace.Root, abs) {
			return fmt.Errorf("兼容策略必须保存在所有源目录之外")
		}
	}
	policies := map[string]sourcePolicy{}
	info, err := os.Lstat(abs)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() > 128<<10 {
			return fmt.Errorf("兼容策略文件无效")
		}
		data, readErr := os.ReadFile(abs)
		if readErr != nil {
			return readErr
		}
		if json.Unmarshal(data, &policies) != nil || policies == nil {
			return fmt.Errorf("无法解析兼容策略")
		}
		for root, policy := range policies {
			if !filepath.IsAbs(root) || !validSourcePolicy(policy.Mode) {
				return fmt.Errorf("兼容策略内容无效")
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	s.compatibilityMu.Lock()
	defer s.compatibilityMu.Unlock()
	s.compatibilityPath, s.sourcePolicies = abs, policies
	return nil
}

func validSourcePolicy(mode string) bool { return mode == "automatic" || mode == "readonly" }

func (s *Server) sourcePolicyFor(root string) sourcePolicy {
	s.compatibilityMu.Lock()
	defer s.compatibilityMu.Unlock()
	return s.sourcePolicyLocked(root)
}

func (s *Server) sourcePolicyLocked(root string) sourcePolicy {
	if policy, ok := s.sourcePolicies[filepath.Clean(root)]; ok {
		return policy
	}
	return sourcePolicy{Mode: "automatic"}
}

func (s *Server) requireSourceChanges(root string) error {
	if s.sourcePolicyFor(root).Mode == "readonly" {
		return fmt.Errorf("当前工作区已设为“仅查看源数据”，请在数据格式兼容性中切回自动识别后重新预览")
	}
	return nil
}

// beginSourceChanges 与策略保存共用短锁；策略切换不打断已开始的源操作，也不能在切换后启动新的源操作。
func (s *Server) beginSourceChanges(root string) (func(), error) {
	root = filepath.Clean(root)
	s.compatibilityMu.Lock()
	defer s.compatibilityMu.Unlock()
	if s.sourcePolicyLocked(root).Mode == "readonly" {
		return nil, fmt.Errorf("当前工作区已设为“仅查看源数据”，源数据修改已阻止")
	}
	if s.sourceChanges == nil {
		s.sourceChanges = map[string]int{}
	}
	s.sourceChanges[root]++
	return func() { s.compatibilityMu.Lock(); s.sourceChanges[root]--; s.compatibilityMu.Unlock() }, nil
}

// sourcePolicyMiddleware 覆盖同步恢复与源目录维护入口；异步会话操作在提交时持有独立的操作计数。
func (s *Server) sourcePolicyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimRight(r.URL.Path, "/")
		mutation := r.Method == http.MethodPost && (path == "/api/v1/operations/recover" ||
			path == "/api/v1/maintenance/execute" || path == "/api/v1/maintenance/recover" ||
			strings.HasPrefix(path, "/api/v1/archives/local/restore/") && !strings.HasSuffix(path, "/preview") ||
			(strings.HasPrefix(path, "/api/v1/archives/") || strings.HasPrefix(path, "/api/v1/github/archives/")) && strings.HasSuffix(path, "/restore/execute"))
		if mutation {
			release, err := s.beginSourceChanges(s.currentRoot())
			if err != nil {
				writeError(w, http.StatusForbidden, err.Error())
				return
			}
			defer release()
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) compatibilityHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	detect := r.URL.Path == "/api/v1/compatibility/detect"
	if (!detect && r.Method != http.MethodGet && r.Method != http.MethodPut) || (detect && r.Method != http.MethodPost) {
		writeError(w, http.StatusMethodNotAllowed, "不支持此兼容性请求方法")
		return
	}
	workspace := s.currentWorkspace()
	root := workspace.Definition.Root
	if root == "" {
		writeError(w, http.StatusConflict, "尚未配置工作区")
		return
	}
	if r.Method != http.MethodGet {
		var input struct {
			Root   string `json:"root"`
			Policy string `json:"policy,omitempty"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
			writeError(w, 400, "兼容性请求无效")
			return
		}
		if input.Root != root {
			writeError(w, 409, "工作区已变化，请重新读取兼容状态")
			return
		}
		if detect {
			task := s.tasks.StartInventoryWithOptions(root, application.ScanOptions{})
			writeJSON(w, http.StatusAccepted, map[string]any{"root": root, "task": task})
			return
		}
		if !validSourcePolicy(input.Policy) {
			writeError(w, 400, "不支持的适配策略")
			return
		}
		s.compatibilityMu.Lock()
		if s.compatibilityPath == "" {
			s.compatibilityMu.Unlock()
			writeError(w, 409, "未配置独立策略存储，无法保存")
			return
		}
		if s.sourceChanges[filepath.Clean(root)] > 0 {
			s.compatibilityMu.Unlock()
			writeError(w, 409, "源数据操作正在执行，请等待完成后切换策略")
			return
		}
		policies := maps.Clone(s.sourcePolicies)
		if policies == nil {
			policies = map[string]sourcePolicy{}
		}
		if s.sourcePolicyLocked(root).Mode != input.Policy {
			policies[filepath.Clean(root)] = sourcePolicy{Mode: input.Policy, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			if err := saveSourcePolicies(s.compatibilityPath, policies); err != nil {
				s.compatibilityMu.Unlock()
				writeError(w, 500, "保存策略失败，原策略继续生效")
				return
			}
			s.sourcePolicies = policies
		}
		s.compatibilityMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"root": root, "policy": input.Policy, "persisted": true})
		return
	}
	snapshot, err := s.tasks.ReadReport(r.Context(), root, application.ScanOptions{}, false)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if root != s.currentRoot() {
		writeError(w, 409, "工作区已变化，请重新读取兼容状态")
		return
	}
	resolution := compat.ResolveInventory(snapshot.Report, s.registry)
	versions := []string{}
	for _, file := range snapshot.Report.Files {
		if file.Rollout != nil && strings.TrimSpace(file.Rollout.CLIVersion) != "" {
			versions = append(versions, file.Rollout.CLIVersion)
		}
		if file.SQLite != nil {
			for _, thread := range file.SQLite.Threads {
				if strings.TrimSpace(thread.CLIVersion) != "" {
					versions = append(versions, thread.CLIVersion)
				}
			}
		}
	}
	slices.Sort(versions)
	versions = slices.Compact(versions)
	mode, reason := "通用读取", "未匹配专用读取规则"
	if resolution.Matched {
		mode, reason = "已识别格式", "使用已匹配的读取规则"
	} else if len(versions) > 1 {
		reason = "检测到多个历史版本"
	} else if len(versions) == 0 {
		reason = "未找到版本信息"
	}
	policy := s.sourcePolicyFor(root)
	sourceStatus := "未启用源数据修改"
	if policy.Mode == "readonly" {
		sourceStatus = "已设为仅查看"
	} else if s.executionAllowed(root) {
		sourceStatus = "需逐项检查并确认"
	}
	s.compatibilityMu.Lock()
	persisted := s.compatibilityPath != ""
	s.compatibilityMu.Unlock()
	writeJSON(w, 200, map[string]any{"root": root, "policy": policy.Mode, "persisted": persisted, "mode": mode, "reason": reason,
		"source_status": sourceStatus, "checked_at": snapshot.Metadata.LastSuccessfulScanAt, "stale": snapshot.Metadata.Stale,
		"versions": versions, "resolution": resolution})
}

func saveSourcePolicies(path string, policies map[string]sourcePolicy) error {
	data, err := json.Marshal(policies)
	if err != nil || len(data) > 128<<10 {
		return fmt.Errorf("策略超过保存上限")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".compatibility-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
