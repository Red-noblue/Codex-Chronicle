// 中文说明：将主题、阅读密度和已保存筛选持久化在应用数据目录，换端口后仍可恢复；不接受凭证或会话正文。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const uiPreferencesLimit = 128 << 10

// SetUIPreferencesPath 在启动时注入独立设置路径，禁止写入源数据目录。
func (s *Server) SetUIPreferencesPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil || pathHasCodexComponent(abs) || s.currentRoot() != "" && pathsOverlap(s.currentRoot(), abs) {
		return fmt.Errorf("界面设置必须位于源目录与 .codex 之外")
	}
	s.uiPreferencesPath = abs
	return nil
}

func validateUIPreferences(values map[string]string) error {
	for key, value := range values {
		switch key {
		case "codex-chronicle.theme.v1":
			if value != "light" && value != "dark" {
				return fmt.Errorf("主题设置无效")
			}
		case "codex-chronicle.density.v1":
			if value != "comfortable" && value != "compact" && value != "reading" {
				return fmt.Errorf("阅读密度无效")
			}
		case "codex-chronicle.saved-filters.v1":
			var filters map[string][]json.RawMessage
			if json.Unmarshal([]byte(value), &filters) != nil || filters == nil {
				return fmt.Errorf("已保存筛选无效")
			}
		default:
			return fmt.Errorf("不支持的界面设置")
		}
	}
	return nil
}

func readUIPreferences(path string) (map[string]string, error) {
	values := make(map[string]string)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > uiPreferencesLimit {
		return nil, fmt.Errorf("界面设置不是有效的普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &values); err != nil || values == nil {
		return nil, fmt.Errorf("无法解析界面设置")
	}
	return values, validateUIPreferences(values)
}

func saveUIPreferences(path string, values map[string]string) error {
	data, err := json.Marshal(values)
	if err != nil || len(data) > uiPreferencesLimit {
		return fmt.Errorf("界面设置超过保存上限")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".ui-preferences-*.tmp")
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

func (s *Server) uiPreferencesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "只支持 GET 和 PATCH")
		return
	}
	s.uiPreferencesMu.Lock()
	defer s.uiPreferencesMu.Unlock()
	if s.uiPreferencesPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "preferences": map[string]string{}})
		return
	}
	values, err := readUIPreferences(s.uiPreferencesPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取界面设置失败")
		return
	}
	if r.Method == http.MethodPatch {
		var patch map[string]string
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, uiPreferencesLimit))
		if decoder.Decode(&patch) != nil || patch == nil || validateUIPreferences(patch) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
			writeError(w, http.StatusBadRequest, "界面设置无效或超过保存上限")
			return
		}
		for key, value := range patch {
			values[key] = value
		}
		if err := saveUIPreferences(s.uiPreferencesPath, values); err != nil {
			writeError(w, http.StatusInternalServerError, "保存界面设置失败，原设置继续保留")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "preferences": values})
}
