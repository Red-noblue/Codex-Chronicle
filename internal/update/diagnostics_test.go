// 中文说明：验证更新失败留痕、导出白名单与脱敏、有界并发记录、损坏来源降级及离线导出不覆盖用户文件。
package update

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func diagnosticArchiveFiles(t *testing.T, archive DiagnosticsArchive) map[string][]byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(archive.Data), int64(len(archive.Data)))
	if err != nil {
		t.Fatal(err)
	}
	result := map[string][]byte{}
	for _, file := range reader.File {
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		result[file.Name] = data
	}
	if len(result) != 2 || result["report.json"] == nil || result["update-events.json"] == nil {
		t.Fatalf("诊断包出现非白名单文件：%v", result)
	}
	return result
}

func TestDiagnosticsObserveRealNetworkAndSignatureFailures(t *testing.T) {
	m, _, _, offline := automaticFixture(t)
	offline.Store(true)
	if _, err := m.CheckOnline(t.Context()); err == nil {
		t.Fatal("网络故障未触发失败")
	}
	if failure := lastDiagnosticFailure(m.root); failure == nil || failure.Stage != "check" || failure.Code != "http_error" || failure.ID == "" {
		t.Fatalf("检查失败未留下阶段、分类及故障编号：%+v", failure)
	}
	if _, err := m.Prepare(t.Context(), makeBundle(t, "4.0.0", "未签名"), false); err == nil {
		t.Fatal("未签名候选未被拒绝")
	}
	status, err := m.Status(t.Context())
	if err != nil || status.LastDiagnostic == nil || status.LastDiagnostic.Stage != "verify_signature" || status.LastDiagnostic.Code != "signature_invalid" {
		t.Fatalf("签名失败未保留诊断：%+v %v", status.LastDiagnostic, err)
	}
	archive, err := BuildDiagnostics(t.Context(), m.root, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	files := diagnosticArchiveFiles(t, archive)
	for _, want := range []string{"http_error", "signature_invalid", status.LastDiagnostic.ID} {
		if !bytes.Contains(files["update-events.json"], []byte(want)) {
			t.Fatalf("导出缺少实际失败证据 %s", want)
		}
	}
}

func TestDiagnosticsExportUsesWhitelistAndRedactsCredentialsAndPaths(t *testing.T) {
	data := t.TempDir()
	m, err := NewManager(filepath.Join(data, "updates"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		StateName:           map[string]any{"active_version": "1.0.0", "last_error": `Get https://alice:proxy-password@example.test/private?token=url-secret: token=field-secret Bearer bearer-secret at /Users/private-person/secret project/archive.db`, "token": "unknown-field-secret", "online": map[string]any{"url": "https://private.test/credential"}},
		"online-proxy.json": onlineProxySettings{ProxyURL: "http://proxy-user:proxy-password@127.0.0.1:7890"},
	} {
		if err := writeAtomicJSON(t.Context(), filepath.Join(m.root, name), value); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"auth.json": "AUTH_SECRET", "archive.db": "CONVERSATION_BODY", "daemon.log": "RAW_LOG_SECRET"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	finish := BeginDiagnostic(m.root, "restart", "1.0.0")
	finish(errors.New(`permission denied C:\Users\private-person\secret project\file token=windows-secret`))
	archive, err := BuildDiagnostics(t.Context(), m.root, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	files := diagnosticArchiveFiles(t, archive)
	all := string(files["report.json"]) + string(files["update-events.json"])
	for _, secret := range []string{"private-person", "proxy-user", "proxy-password", "url-secret", "field-secret", "bearer-secret", "unknown-field-secret", "windows-secret", "AUTH_SECRET", "CONVERSATION_BODY", "RAW_LOG_SECRET", "private.test", "secret project"} {
		if strings.Contains(all, secret) {
			t.Fatalf("诊断泄露禁止内容 %q", secret)
		}
	}
	if !strings.Contains(all, `"loopback": true`) || !strings.Contains(all, `"configured": true`) {
		t.Fatal("代理诊断丢失非敏感信息")
	}
	before, _ := os.ReadFile(filepath.Join(m.root, StateName))
	if _, err := BuildDiagnostics(t.Context(), m.root, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(m.root, StateName))
	if !bytes.Equal(before, after) {
		t.Fatal("只读导出改变了安装状态")
	}
}

func TestDiagnosticsRemainAvailableWithDamagedAndOversizedSources(t *testing.T) {
	root := filepath.Join(t.TempDir(), "updates")
	if _, err := NewManager(root); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{StateName: []byte("broken JSON"), diagnosticEventsName: []byte("[broken"), "automatic-state.json": bytes.Repeat([]byte("x"), diagnosticReadLimit+1)} {
		if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := BuildDiagnostics(t.Context(), root, "1.0.0")
	if err != nil {
		t.Fatal("状态损坏导致无法导出", err)
	}
	files := diagnosticArchiveFiles(t, archive)
	var report diagnosticReport
	if err := json.Unmarshal(files["report.json"], &report); err != nil || len(report.Warnings) < 3 {
		t.Fatalf("未说明来源损坏：%s %v", files["report.json"], err)
	}
	missing := filepath.Join(t.TempDir(), "not-created")
	if _, err := BuildDiagnostics(t.Context(), missing, "old-version"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("导出初始化了不存在的更新目录")
	}
}

func TestDiagnosticsRefuseSourceSymlinksWithoutBreakingUpdates(t *testing.T) {
	m, _, _, _ := automaticFixture(t)
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"last_error":"DO_NOT_EXPORT_SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.root, "last-cleanup.json")
	if err := os.Symlink(secret, path); err != nil {
		t.Skipf("本机无法创建测试链接：%v", err)
	}
	archive, err := BuildDiagnostics(t.Context(), m.root, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	files := diagnosticArchiveFiles(t, archive)
	if bytes.Contains(files["report.json"], []byte("DO_NOT_EXPORT_SECRET")) || !bytes.Contains(files["report.json"], []byte("cleanup")) {
		t.Fatal("读取了链接指向的秘密或未说明跳过来源")
	}
	// 只破坏本测试的诊断日志；更新检查必须继续保持原本的结果。
	if err := os.Remove(filepath.Join(m.root, diagnosticEventsName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(m.root, diagnosticEventsName)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CheckOnline(t.Context()); err != nil {
		t.Fatal("诊断不可写改变了更新行为", err)
	}
	actual, _ := os.ReadFile(secret)
	if string(actual) != `{"last_error":"DO_NOT_EXPORT_SECRET"}` {
		t.Fatal("诊断记录覆盖了链接目标")
	}
}

func TestDiagnosticsBoundConcurrentHistoryAndPreserveUserOutput(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for i := range 55 {
				event := DiagnosticEvent{ID: fmt.Sprintf("test-%d-%d", worker, i), At: time.Now().UTC(), Stage: "check", Result: "succeeded"}
				if err := appendDiagnostic(t.Context(), root, event); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	var events []DiagnosticEvent
	if err := readDiagnosticJSON(root, diagnosticEventsName, &events); err != nil || len(events) != diagnosticEventLimit {
		t.Fatalf("记录不是有界完整 JSON：%d %v", len(events), err)
	}
	ids := map[string]bool{}
	for _, event := range events {
		if ids[event.ID] {
			t.Fatal("并发写入产生重复或丢失更新")
		}
		ids[event.ID] = true
	}
	output := filepath.Join(t.TempDir(), "diagnostics.zip")
	if _, err := WriteDiagnosticsFile(t.Context(), root, "1.0.0", output); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(output)
	if _, err := WriteDiagnosticsFile(t.Context(), root, "1.0.0", output); !os.IsExist(err) {
		t.Fatal("导出覆盖现有文件")
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("导出改变现有文件")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := BuildDiagnostics(ctx, root, "1.0.0"); !errors.Is(err, context.Canceled) {
		t.Fatal("取消后仍成功导出")
	}
}

func TestDiagnosticFailureCodes(t *testing.T) {
	for _, item := range []struct {
		err  error
		code string
	}{
		{&InsufficientSpaceError{AvailableBytes: 10, RequiredBytes: 20}, "insufficient_disk_space"},
		{os.ErrPermission, "permission_denied"},
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{ErrOnlineReleaseWithdrawn, "release_withdrawn"},
		{ErrOnlineUpdatesPaused, "updates_paused"},
		{fmt.Errorf("%w：SHA-256 不匹配", ErrInvalidOnlinePackage), "checksum_mismatch"},
		{errors.New("archive.db 迁移失败"), "database_migration_failed"},
	} {
		if code := diagnosticErrorCode("download", item.err); code != item.code {
			t.Fatalf("分类错误：%s，期望 %s", code, item.code)
		}
	}
}

func TestDiagnosticsLongErrorsStayWithinReadableByteLimit(t *testing.T) {
	root := t.TempDir()
	for i := range diagnosticEventLimit + 4 {
		event := DiagnosticEvent{ID: fmt.Sprint(i), At: time.Now().UTC(), Stage: "restart", Result: "failed", TargetVersion: strings.Repeat("版本", 300), Message: strings.Repeat("错误", 300)}
		if err := appendDiagnostic(t.Context(), root, event); err != nil {
			t.Fatal("长错误令后续诊断无法写入", err)
		}
	}
	info, err := os.Stat(filepath.Join(root, diagnosticEventsName))
	if err != nil || info.Size() > diagnosticReadLimit {
		t.Fatal("日志超过自身读取上限", err)
	}
	var events []DiagnosticEvent
	if err := readDiagnosticJSON(root, diagnosticEventsName, &events); err != nil || len(events) == 0 || len(events) > diagnosticEventLimit || events[len(events)-1].ID != fmt.Sprint(diagnosticEventLimit+3) {
		t.Fatal("无法读取或丢失最近的故障记录", err)
	}
}

func TestDiagnosticPreservesErrorAfterTimestampAndRedactsAbsolutePaths(t *testing.T) {
	message := "候选启动器失败：2026/11/27 11:00:00 archive.db 迁移失败，日志：/Users/private-person/secret project/log"
	redacted := redactDiagnosticText(message)
	if !strings.Contains(redacted, "2026/11/27") || !strings.Contains(redacted, "archive.db 迁移失败") || strings.Contains(redacted, "private-person") || strings.Contains(redacted, "secret project") {
		t.Fatal("时间戳被误当成路径，或私人路径未过滤", redacted)
	}
	for _, path := range []string{`open \\private-server\private-share\file: permission denied`, `open "C:\Users\private-person\file": permission denied`} {
		redacted := redactDiagnosticText(path)
		if strings.Contains(redacted, "private-") {
			t.Fatal("Windows 路径未过滤", redacted)
		}
	}
}
