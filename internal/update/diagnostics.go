// 中文说明：独立保存有界、脱敏的更新事件和故障编号；诊断写入失败不会改变更新事务的成败。
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const diagnosticEventsName = "diagnostic-events.json"
const diagnosticEventLimit = 192
const diagnosticReadLimit = 512 << 10

// DiagnosticEvent 仅记录更新元数据，不接收请求正文、会话内容或任意配置对象。
type DiagnosticEvent struct {
	ID             string    `json:"id"`
	At             time.Time `json:"at"`
	Stage          string    `json:"stage"`
	Result         string    `json:"result"`
	TargetVersion  string    `json:"target_version,omitempty"`
	Code           string    `json:"code,omitempty"`
	Message        string    `json:"message,omitempty"`
	RequiredBytes  uint64    `json:"required_bytes,omitempty"`
	AvailableBytes uint64    `json:"available_bytes,omitempty"`
}

// BeginDiagnostic 同一阶段的开始和结果共享编号；root 必须是本应用独立的 updates 目录。
func BeginDiagnostic(root, stage, target string) func(error) {
	id, err := newID("diag")
	if err != nil {
		return func(error) {}
	}
	event := DiagnosticEvent{ID: id, At: time.Now().UTC(), Stage: stage, Result: "started", TargetVersion: target}
	recordDiagnostic(root, event)
	return func(failure error) {
		event.At, event.Result = time.Now().UTC(), "succeeded"
		if failure != nil {
			event.Result, event.Code, event.Message = "failed", diagnosticErrorCode(stage, failure), failure.Error()
			var space *InsufficientSpaceError
			if errors.As(failure, &space) {
				event.RequiredBytes, event.AvailableBytes = space.RequiredBytes, space.AvailableBytes
			}
		}
		recordDiagnostic(root, event)
	}
}

func recordDiagnostic(root string, event DiagnosticEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := appendDiagnostic(ctx, root, event); err != nil {
		log.Printf("更新诊断记录暂不可用（%s）：%v", event.Stage, err)
	}
}

func appendDiagnostic(ctx context.Context, root string, event DiagnosticEvent) error {
	if _, err := safeExistingDirectory(root); err != nil {
		return err
	}
	var lock *os.File
	var err error
	for {
		lock, err = acquireUpdateLock(filepath.Join(root, ".diagnostics.lock"))
		if !errors.Is(err, errUpdateBusy) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	var events []DiagnosticEvent
	if err := readDiagnosticJSON(root, diagnosticEventsName, &events); err != nil && !os.IsNotExist(err) {
		// 损坏的诊断记录留给离线导出报告，不覆盖未知内容。
		return err
	}
	if len(events) >= diagnosticEventLimit {
		events = events[len(events)-diagnosticEventLimit+1:]
	}
	event = sanitizeDiagnosticEvent(event)
	events = append(events, event)
	return writeAtomicJSON(ctx, filepath.Join(root, diagnosticEventsName), events)
}

func lastDiagnosticFailure(root string) *DiagnosticEvent {
	var events []DiagnosticEvent
	if readDiagnosticJSON(root, diagnosticEventsName, &events) != nil {
		return nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Result == "failed" {
			event := sanitizeDiagnosticEvent(events[i])
			return &event
		}
	}
	return nil
}

func diagnosticErrorCode(stage string, err error) string {
	var timeout net.Error
	var space *InsufficientSpaceError
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "network_timeout"
	case errors.As(err, &space):
		return "insufficient_disk_space"
	case errors.Is(err, ErrDiskSpaceUnavailable):
		return "disk_space_unavailable"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, ErrOnlineUpdatesPaused):
		return "updates_paused"
	case errors.Is(err, ErrOnlineReleaseWithdrawn):
		return "release_withdrawn"
	case errors.Is(err, ErrOnlinePolicyUnavailable):
		return "policy_unavailable"
	case errors.Is(err, errUpdateBusy):
		return "update_busy"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "签名"), strings.Contains(message, "signature"):
		return "signature_invalid"
	case strings.Contains(message, "摘要"), strings.Contains(message, "sha-256"), strings.Contains(message, "校验不一致"):
		return "checksum_mismatch"
	case strings.Contains(message, "http "):
		return "http_error"
	case strings.Contains(message, "迁移"), strings.Contains(message, "schema"):
		return "database_migration_failed"
	case strings.Contains(message, "启动"), strings.Contains(message, "健康检查"):
		return "startup_check_failed"
	default:
		return stage + "_failed"
	}
}

var diagnosticURL = regexp.MustCompile(`(?i)(?:https?|socks5h?)://[^\s<>"']+`)
var diagnosticCredential = regexp.MustCompile(`(?i)(?:bearer\s+[a-z0-9._~+/-]+=*|gh[pousr]_[a-z0-9_]+|github_pat_[a-z0-9_]+|sk-[a-z0-9_-]+)`)
var diagnosticSecret = regexp.MustCompile(`(?i)(authorization|proxy-authorization|access_token|refresh_token|token|password|passwd|secret|api[_-]?key)["']?\s*[:=]\s*["']?[^\s,;"'<>]+`)
var diagnosticPath = regexp.MustCompile(`(?m)(?:[a-zA-Z]:[\\/]|/)[^\r\n<>"']*`)

func redactDiagnosticText(value string) string {
	value = diagnosticURL.ReplaceAllString(value, "[网址已隐藏]")
	value = diagnosticCredential.ReplaceAllString(value, "[凭据已隐藏]")
	value = diagnosticSecret.ReplaceAllString(value, "$1=[已隐藏]")
	value = diagnosticPath.ReplaceAllString(value, "[路径已隐藏]")
	value = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	runes := []rune(value)
	if len(runes) > 500 {
		value = string(runes[:500]) + "…"
	}
	return value
}

func sanitizeDiagnosticEvent(event DiagnosticEvent) DiagnosticEvent {
	event.ID = redactDiagnosticText(event.ID)
	event.Stage = redactDiagnosticText(event.Stage)
	event.Result = redactDiagnosticText(event.Result)
	event.TargetVersion = redactDiagnosticText(event.TargetVersion)
	event.Code = redactDiagnosticText(event.Code)
	event.Message = redactDiagnosticText(event.Message)
	return event
}

// readDiagnosticJSON 限制读取大小并拒绝链接、设备和越界路径；不打开数据库或触发更新恢复。
func readDiagnosticJSON(root, name string, value any) error {
	if _, err := safeExistingDirectory(root); err != nil {
		return err
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := 1; i < len(parts); i++ {
		parent, err := dir.Lstat(strings.Join(parts[:i], "/"))
		if err != nil {
			return err
		}
		if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
			return errors.New("诊断来源目录不是普通目录")
		}
	}
	info, err := dir.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > diagnosticReadLimit {
		return errors.New("诊断来源不是有界普通文件")
	}
	file, err := dir.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return errors.New("诊断来源在读取期间发生变化")
	}
	data, err := io.ReadAll(io.LimitReader(file, diagnosticReadLimit+1))
	if err != nil || len(data) > diagnosticReadLimit {
		return errors.New("诊断来源无法在大小限制内读取")
	}
	if err := diagnosticUnmarshal(data, value); err != nil {
		return fmt.Errorf("诊断来源 JSON 无法读取：%w", err)
	}
	return nil
}
