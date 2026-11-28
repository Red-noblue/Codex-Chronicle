// 中文说明：通过真实HTTP传输和持久化计划验证更新模式、旧开关迁移、人工安装边界及暂停后的断点续传。
package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdateModesMigrateLegacyAndRemainSafeForOldClients(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.automaticStatus()
	if s.Mode != ModeAutomatic || !s.Enabled {
		t.Fatal(s)
	}
	path := filepath.Join(m.root, "automatic-settings.json")
	if err = os.WriteFile(path, []byte(`{"enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, _ = m.automaticStatus()
	if s.Mode != ModeManual || s.Enabled {
		t.Fatal(s)
	}
	for _, mode := range []UpdateMode{ModeDownload, ModeNotify, ModeAutomatic, ModeManual} {
		if err = m.SetMode(t.Context(), mode); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewManager(m.root)
		if err != nil {
			t.Fatal(err)
		}
		s, err = reopened.automaticStatus()
		if err != nil || s.Mode != mode {
			t.Fatalf("模式未持久化：%+v %v", s, err)
		}
		var old struct {
			Enabled bool `json:"enabled"`
		}
		body, _ := os.ReadFile(path)
		_ = json.Unmarshal(body, &old)
		if old.Enabled != (mode == ModeAutomatic) {
			t.Fatalf("旧客户端会误自动安装：%s", body)
		}
	}
	before, _ := os.ReadFile(path)
	if err = m.SetMode(t.Context(), "unknown"); !errors.Is(err, ErrInvalidUpdateMode) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("无效模式改写了设置")
	}
}

func TestNotifyOnlyChecksAndDownloadModeNeverInstalls(t *testing.T) {
	for _, mode := range []UpdateMode{ModeManual, ModeNotify, ModeDownload} {
		t.Run(string(mode), func(t *testing.T) {
			m, calls, _, _ := automaticFixture(t)
			if err := m.SetMode(t.Context(), mode); err != nil {
				t.Fatal(err)
			}
			restarts := 0
			now := time.Now().UTC()
			step := func() {
				t.Helper()
				if err := m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, func(context.Context, string) error { restarts++; return nil }, now); err != nil {
					t.Fatal(err)
				}
			}
			step()
			status, _ := m.Status(t.Context())
			if restarts != 0 || status.State.ActiveVersion != "1.0.0" {
				t.Fatal("非自动安装模式切换了当前版本")
			}
			switch mode {
			case ModeManual:
				if calls.Load() != 0 {
					t.Fatal("手动模式访问了网络")
				}
			case ModeNotify:
				if calls.Load() != 1 || status.Automatic.Phase != "available" || len(status.PendingPlanIDs) != 0 {
					t.Fatalf("仅提醒仍下载：%+v", status)
				}
			case ModeDownload:
				if status.Automatic.Phase != "waiting_install" || status.Automatic.PlanID == "" {
					t.Fatal(status.Automatic)
				}
				requests := calls.Load()
				step()
				if calls.Load() != requests {
					t.Fatal("重复下载已准备版本")
				}
				if err := m.SetMode(t.Context(), ModeAutomatic); err != nil {
					t.Fatal(err)
				}
				step()
				if restarts != 1 {
					t.Fatal("改为自动模式后未继续更新")
				}
			}
		})
	}
}

func TestExplicitBackgroundDownloadRemainsManualAcrossRestart(t *testing.T) {
	for _, mode := range []UpdateMode{ModeAutomatic, ModeNotify, ModeManual} {
		t.Run(string(mode), func(t *testing.T) {
			m, calls, _, _ := automaticFixture(t)
			_ = m.SetMode(t.Context(), mode)
			if err := m.ControlDownload(t.Context(), "download"); err != nil {
				t.Fatal(err)
			}
			restarts := 0
			step := func(at time.Time) {
				t.Helper()
				if err := m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, func(context.Context, string) error { restarts++; return nil }, at); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			step(now)
			s, _ := m.Status(t.Context())
			if restarts != 0 || s.Automatic.ManualPlanID == "" || s.Automatic.PlanID != "" || s.Automatic.Phase != "waiting_install" {
				t.Fatalf("人工下载被自动安装：%+v", s)
			}
			plan, err := m.LoadPlan(t.Context(), s.Automatic.ManualPlanID)
			if err != nil || plan.Automatic {
				t.Fatalf("人工意图未保留：%+v %v", plan, err)
			}
			requests := calls.Load()
			m, err = NewManagerWithOptions(m.root, ManagerOptions{RequireSignature: true, TrustedPublicKeys: m.trustedPublicKeys, Online: m.online})
			if err != nil {
				t.Fatal(err)
			}
			step(now.Add(7 * time.Hour))
			if calls.Load() != requests || restarts != 0 {
				t.Fatal("重启或经过检查期限后接管了人工计划")
			}
		})
	}
}

func TestPausedDownloadCancelsTransferPersistsAndResumesRange(t *testing.T) {
	data := bytes.Repeat([]byte("可续传的更新内容"), 8192)
	started := make(chan struct{})
	var requests atomic.Int32
	var resumed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"same-package"`)
		if value := r.Header.Get("Range"); value != "" {
			offset, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(value, "bytes="), "-"))
			if err != nil || offset <= 0 || offset >= len(data) {
				t.Error("续传范围无效", value)
				w.WriteHeader(400)
				return
			}
			resumed.Store(true)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-offset))
			w.WriteHeader(206)
			_, _ = w.Write(data[offset:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	options := ManagerOptions{Online: OnlineConfig{FeedURL: server.URL + "/feed.json"}}
	m, err := NewManagerWithOptions(t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	release := onlineTestRelease("2.0.0", data, runtime.GOOS, runtime.GOARCH)
	release.URL = server.URL + "/release.zip"
	done := make(chan error, 1)
	go func() { _, err := m.DownloadOnline(t.Context(), release); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("传输没有开始")
	}
	for i := 0; i < 100; i++ {
		s, _ := m.Status(t.Context())
		if s.Download != nil && s.Download.ReceivedBytes > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = m.ControlDownload(t.Context(), "pause"); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, ErrDownloadPaused) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("暂停没有取消网络传输")
	}
	m, err = NewManagerWithOptions(m.root, options)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Status(t.Context())
	if !s.Automatic.DownloadPaused {
		t.Fatal("暂停设置未跨重启保留")
	}
	if _, err = m.DownloadOnline(t.Context(), release); !errors.Is(err, ErrDownloadPaused) {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("暂停时仍产生新请求")
	}
	if err = m.ControlDownload(t.Context(), "resume"); err != nil {
		t.Fatal(err)
	}
	downloaded, err := m.DownloadOnline(t.Context(), release)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(downloaded.Path)
	if err != nil || !bytes.Equal(body, data) || !resumed.Load() {
		t.Fatalf("断点续传未保留原内容：%v", err)
	}
}

func TestChangingModeCancelsAutomaticTransferWithoutRejectingVersion(t *testing.T) {
	m, _, _, _ := automaticFixture(t)
	data := bytes.Repeat([]byte("自动下载中的片段"), 4096)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed.json" {
			release := onlineTestRelease("2.0.0", data, runtime.GOOS, runtime.GOARCH)
			release.URL = "http://" + r.Host + "/release.zip"
			_ = json.NewEncoder(w).Encode(OnlineFeed{FormatVersion: 1, Product: "codex-chronicle", Releases: []OnlineRelease{release}})
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	m.online.FeedURL = server.URL + "/feed.json"
	done := make(chan error, 1)
	go func() {
		done <- m.AutomaticStep(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, func(context.Context, string) error { t.Error("切换模式后仍安排安装"); return nil })
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("没有开始传输")
	}
	if err := m.SetMode(t.Context(), ModeNotify); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("改变模式未停止自动下载")
	}
	status, err := m.Status(t.Context())
	if err != nil || status.Automatic.Mode != ModeNotify || status.Automatic.FailedVersion != "" || status.State.ActiveVersion != "1.0.0" {
		t.Fatal(status, err)
	}
	if status.LastDiagnostic != nil {
		t.Fatalf("用户改变模式被记录为故障：%+v", status.LastDiagnostic)
	}
}
func TestDownloadedPlanManualInstallClearsIntentAfterRestart(t *testing.T) {
	m, _, _, _ := automaticFixture(t)
	_ = m.SetMode(t.Context(), ModeDownload)
	now := time.Now().UTC()
	restart := func(context.Context, string) error { return nil }
	if err := m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, restart, now); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Status(t.Context())
	id := status.Automatic.PlanID
	if err := m.SetMode(t.Context(), ModeManual); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ActivateByID(t.Context(), id, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	if err := m.automaticStepAt(t.Context(), "2.0.0", func() bool { return true }, m.VerifyVersionRoot, restart, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status(t.Context())
	if status.Automatic.PlanID != "" || status.Automatic.TargetVersion != "" || len(status.PendingPlanIDs) != 0 {
		t.Fatalf("安装完成后仍展示失效计划：%+v", status)
	}
}

func TestAutomaticDoesNotInstallOverAnotherManualPlan(t *testing.T) {
	m, _, selectVersion, _ := automaticFixture(t)
	_ = m.SetMode(t.Context(), ModeDownload)
	restart := func(context.Context, string) error { t.Error("覆盖了人工待确认计划"); return nil }
	now := time.Now().UTC()
	if err := m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, restart, now); err != nil {
		t.Fatal(err)
	}
	selectVersion("3.0.0")
	manual, _, err := m.prepareOnline(t.Context(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.SetMode(t.Context(), ModeAutomatic)
	if err = m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, restart, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = m.LoadPlan(t.Context(), manual.ID); err != nil {
		t.Fatal("人工计划被丢弃", err)
	}
	status, _ := m.Status(t.Context())
	if status.State.ActiveVersion != "1.0.0" {
		t.Fatal(status.State)
	}
}

// 中文说明：模拟人工后台下载已登记计划、尚未保存UI状态就中断，恢复后仍只等待人工安装。
func TestManualDownloadRecoversPlanRegistrationCrash(t *testing.T) {
	m, calls, _, _ := automaticFixture(t)
	_ = m.SetMode(t.Context(), ModeManual)
	_ = m.ControlDownload(t.Context(), "download")
	now := time.Now().UTC()
	restart := func(context.Context, string) error { t.Error("恢复后自动安装了人工计划"); return nil }
	if err := m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, restart, now); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Status(t.Context())
	id := status.Automatic.ManualPlanID
	interrupted := status.Automatic
	interrupted.ManualPlanID = ""
	interrupted.ManualDownload = true
	interrupted.Phase = "downloading"
	if err := m.saveAutomatic(t.Context(), interrupted); err != nil {
		t.Fatal(err)
	}
	requests := calls.Load()
	if err := m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, restart, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status(t.Context())
	if status.Automatic.ManualPlanID != id || status.Automatic.Phase != "waiting_install" || calls.Load() != requests {
		t.Fatalf("未恢复已登记的人工下载计划：%+v", status.Automatic)
	}
}
