// 中文说明：从固定字段白名单生成本地诊断 ZIP；后台不可用、状态损坏时仍可离线导出，不收集原始日志或用户内容。
package update

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DiagnosticsArchive 是可由网页下载或启动器保存的同一种脱敏诊断包。
type DiagnosticsArchive struct {
	ID       string
	Filename string
	Data     []byte
}

type diagnosticReport struct {
	FormatVersion int                       `json:"format_version"`
	ReportID      string                    `json:"report_id"`
	ExportedAt    time.Time                 `json:"exported_at"`
	Version       string                    `json:"exporter_version"`
	OS            string                    `json:"os"`
	Arch          string                    `json:"arch"`
	GoVersion     string                    `json:"go_version"`
	Sources       map[string]map[string]any `json:"sources"`
	Warnings      []string                  `json:"warnings"`
	Privacy       string                    `json:"privacy"`
}

// BuildDiagnostics 只读固定状态文件；不会初始化更新管理器、联网或访问对话源和数据库。
func BuildDiagnostics(ctx context.Context, updateRoot, version string) (DiagnosticsArchive, error) {
	if strings.TrimSpace(updateRoot) == "" || hasCodexComponent(updateRoot) {
		return DiagnosticsArchive{}, errors.New("诊断仅支持本应用独立更新目录")
	}
	id, err := newID("report")
	if err != nil {
		return DiagnosticsArchive{}, err
	}
	now := time.Now().UTC()
	report := diagnosticReport{
		FormatVersion: 1, ReportID: id, ExportedAt: now, Version: redactDiagnosticText(version),
		OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(),
		Sources: map[string]map[string]any{}, Warnings: []string{},
		Privacy: "仅包含更新阶段、故障编号及白名单状态字段；不含对话、数据库、恢复快照、认证文件、原始日志或完整网址。诊断包只在本地生成，不自动上传。",
	}
	collect := func(label, root, path, fields string) map[string]any {
		var raw map[string]any
		if err := readDiagnosticJSON(root, path, &raw); err != nil {
			status := "unavailable"
			if os.IsNotExist(err) {
				status = "not_found"
			} else {
				report.Warnings = append(report.Warnings, label+"：来源不可用，已跳过")
			}
			report.Sources[label] = map[string]any{"source_status": status}
			return nil
		}
		selected := map[string]any{"source_status": "read"}
		for _, key := range strings.Fields(fields) {
			if value, ok := diagnosticScalar(raw[key]); ok {
				selected[key] = value
			}
		}
		report.Sources[label] = selected
		return raw
	}
	state := collect("installation", updateRoot, StateName, "installed_version active_version previous_version last_operation_id last_status last_error confirmed_at cleanup_after cleanup_retention_minutes rejected_version deferred_version updated_at")
	collect("automatic", updateRoot, "automatic-state.json", "enabled mode download_paused phase plan_id manual_plan_id manual_download target_version failed_version last_error last_checked_at next_check_at")
	collect("automatic_settings", updateRoot, "automatic-settings.json", "enabled mode download_paused")
	collect("installation_journal", updateRoot, JournalName, "operation_id plan_id version previous_version phase updated_at error")
	collect("restart_journal", updateRoot, "restart-journal.json", "version previous phase")
	collect("cleanup", updateRoot, "last-cleanup.json", "running_version removed_items freed_bytes completed_at warnings")
	dataDir := filepath.Dir(updateRoot)
	if available, err := availableDiskSpace(dataDir); err == nil {
		report.Sources["disk"] = map[string]any{"available_bytes": available}
	}
	collect("startup", dataDir, "startup.json", "port auto_fallback")
	collect("runtime", dataDir, "runtime/instance.json", "version pid started_at")
	if active, ok := state["active_version"].(string); ok && safeVersion(active) {
		collect("active_package", updateRoot, filepath.ToSlash(filepath.Join(VersionDir, active, ManifestName)), "format_version version build_id os arch created_at")
	}
	if online, ok := state["online"].(map[string]any); ok {
		if feed, ok := online["feed_url"].(string); ok {
			if parsed, err := url.Parse(feed); err == nil {
				channel := "custom"
				if parsed.Hostname() == "raw.githubusercontent.com" && strings.HasSuffix(parsed.Path, "/update-feeds/preview.json") {
					channel = "preview"
				} else if parsed.Hostname() == "raw.githubusercontent.com" && strings.HasSuffix(parsed.Path, "/update-feeds/stable.json") {
					channel = "stable"
				}
				report.Sources["channel"] = map[string]any{"name": channel}
			}
		}
	}
	var proxy onlineProxySettings
	if err := readDiagnosticJSON(updateRoot, "online-proxy.json", &proxy); err == nil {
		parsed, _ := url.Parse(proxy.ProxyURL)
		value := map[string]any{"configured": proxy.ProxyURL != ""}
		if parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https" || parsed.Scheme == "socks5" || parsed.Scheme == "socks5h") {
			value["scheme"] = parsed.Scheme
			value["loopback"] = parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost" || parsed.Hostname() == "::1"
		}
		report.Sources["proxy"] = value
	} else if !os.IsNotExist(err) {
		report.Warnings = append(report.Warnings, "proxy：来源不可用，已跳过")
	}
	var events []DiagnosticEvent
	if err := readDiagnosticJSON(updateRoot, diagnosticEventsName, &events); err != nil && !os.IsNotExist(err) {
		report.Warnings = append(report.Warnings, "events：事件记录不可用，已跳过")
	}
	if len(events) > diagnosticEventLimit {
		events = events[len(events)-diagnosticEventLimit:]
	}
	for i := range events {
		events[i] = sanitizeDiagnosticEvent(events[i])
	}
	if events == nil {
		events = []DiagnosticEvent{}
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for _, entry := range []struct {
		name  string
		value any
	}{{"report.json", report}, {"update-events.json", events}} {
		if err := ctx.Err(); err != nil {
			return DiagnosticsArchive{}, err
		}
		writer, err := archive.Create(entry.name)
		if err != nil {
			return DiagnosticsArchive{}, err
		}
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(entry.value); err != nil {
			return DiagnosticsArchive{}, err
		}
	}
	if err := archive.Close(); err != nil {
		return DiagnosticsArchive{}, err
	}
	return DiagnosticsArchive{ID: id, Filename: "codex-chronicle-diagnostics-" + now.Format("20060102T150405Z") + "-" + id + ".zip", Data: buffer.Bytes()}, nil
}

// WriteDiagnosticsFile 创建新文件，绝不覆盖现有文件；失败只移除本次未完成的导出文件。
func WriteDiagnosticsFile(ctx context.Context, updateRoot, version, output string) (string, error) {
	archive, err := BuildDiagnostics(ctx, updateRoot, version)
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(archive.Data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		_ = os.Remove(output)
		return "", err
	}
	return archive.ID, nil
}

func diagnosticScalar(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		return redactDiagnosticText(v), true
	case bool, json.Number:
		return v, true
	case []any:
		result := []string{}
		for _, item := range v {
			if text, ok := item.(string); ok {
				result = append(result, redactDiagnosticText(text))
			}
			if len(result) >= 8 {
				break
			}
		}
		return result, true
	default:
		return nil, false
	}
}

func diagnosticUnmarshal(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return errors.New("诊断来源包含多余 JSON")
	}
	return nil
}
