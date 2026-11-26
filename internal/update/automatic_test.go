// 中文说明：验证自动更新默认开启、配置持久化、空闲门禁、重复调度、失败退避与坏版本抑制。
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func automaticFixture(t *testing.T) (*Manager, *atomic.Int32, func(string), *atomic.Bool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := map[string][]byte{}
	for _, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		bundle := makeBundle(t, version, version)
		manifest, err := BuildManifest(t.Context(), bundle, version, "")
		if err != nil {
			t.Fatal(err)
		}
		manifest, err = SignManifest(manifest, private, "automatic-test")
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteManifest(t.Context(), bundle, manifest); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(bundle, ManifestName))
		if err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(t.TempDir(), "release.zip")
		if err := writeZIP(archive, map[string][]byte{ManifestName: data, "bin/codex-chronicle": []byte(version)}); err != nil {
			t.Fatal(err)
		}
		artifacts[version], err = os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
	}
	var selected atomic.Value
	selected.Store("2.0.0")
	calls, offline := &atomic.Int32{}, &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if offline.Load() {
			http.Error(w, "离线", 503)
			return
		}
		v := selected.Load().(string)
		mode, version, policy := strings.Cut(v, ":")
		if policy {
			v = version
		}
		if r.URL.Path == "/feed.json" {
			release := onlineTestRelease(v, artifacts[v], runtime.GOOS, runtime.GOARCH)
			release.BuildID, release.URL = "", "http://"+r.Host+"/release.zip"
			feed := OnlineFeed{FormatVersion: 1, Product: "codex-chronicle", Releases: []OnlineRelease{release}}
			if policy && mode == "paused" {
				feed.UpdatesPaused, feed.PauseReason = true, "正在检查发布质量"
			}
			if policy && mode == "withdrawn" {
				feed.Withdrawn = []WithdrawnVersion{{Version: v, Reason: "发现兼容问题"}}
			}
			if feed.UpdatesPaused || len(feed.Withdrawn) != 0 {
				feed.HeldReleases, feed.Releases = feed.Releases, []OnlineRelease{}
			}
			_ = json.NewEncoder(w).Encode(feed)
			return
		}
		_, _ = w.Write(artifacts[v])
	}))
	t.Cleanup(server.Close)
	m, err := NewManagerWithOptions(t.TempDir(), ManagerOptions{RequireSignature: true, TrustedPublicKeys: [][]byte{public}, Online: OnlineConfig{FeedURL: server.URL + "/feed.json"}})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "baseline.zip")
	if err := os.WriteFile(archive, artifacts["1.0.0"], 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PrepareArchive(t.Context(), archive, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	return m, calls, func(version string) { selected.Store(version) }, offline
}

func TestAutomaticDefaultsPersistsAndWaitsForIdle(t *testing.T) {
	m, calls, _, _ := automaticFixture(t)
	status, _ := m.Status(t.Context())
	if !status.Automatic.Enabled {
		t.Fatal("自动更新未默认开启")
	}
	if err := m.SetAutomatic(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	options := ManagerOptions{RequireSignature: true, TrustedPublicKeys: m.trustedPublicKeys, Online: m.online}
	m, _ = NewManagerWithOptions(m.root, options)
	now, idle, restarts := time.Now().UTC(), false, 0
	restart := func(context.Context, string) error { restarts++; return nil }
	step := func(at time.Time, running string) {
		t.Helper()
		if err := m.automaticStepAt(t.Context(), running, func() bool { return idle }, m.VerifyVersionRoot, restart, at); err != nil {
			t.Fatal(err)
		}
	}
	step(now, "1.0.0")
	if calls.Load() != 0 {
		t.Fatal("关闭后仍访问更新源")
	}
	if err := m.SetAutomatic(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	step(now, "1.0.0")
	status, _ = m.Status(t.Context())
	if status.Automatic.Phase != "waiting_idle" || status.Automatic.PlanID == "" || restarts != 0 || status.State.ActiveVersion != "1.0.0" {
		t.Fatalf("忙碌时仍切换版本：%+v", status)
	}
	requests := calls.Load()
	// 模拟计划已经落盘、调度器还没保存 plan_id 就断电。
	interrupted := status.Automatic
	interrupted.PlanID, interrupted.Phase = "", "downloading"
	if err := m.saveAutomatic(t.Context(), interrupted); err != nil {
		t.Fatal(err)
	}
	m, _ = NewManagerWithOptions(m.root, options)
	idle = true
	step(now.Add(time.Minute), "1.0.0")
	step(now.Add(2*time.Minute), "1.0.0")
	if restarts != 1 || calls.Load() != requests+1 {
		t.Fatal("未执行一次安装前复核，或重复下载和重复安排重启")
	}
	step(now.Add(3*time.Minute), "2.0.0")
	status, _ = m.Status(t.Context())
	if status.Automatic.Phase != "up_to_date" || status.Automatic.PlanID != "" {
		t.Fatalf("新进程未完成自动周期：%+v", status.Automatic)
	}
}

func TestAutomaticResumesCommittedIntentWithoutAdoptingManualPlan(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "automatic"}[automatic], func(t *testing.T) {
			m, calls, _, _ := automaticFixture(t)
			plan, _, err := m.prepareOnline(t.Context(), false, automatic)
			if err != nil {
				t.Fatal(err)
			}
			requests, restarts := calls.Load(), 0
			if automatic {
				if err := m.saveAutomatic(t.Context(), AutomaticStatus{Phase: "activating", PlanID: plan.ID, TargetVersion: plan.Version}); err != nil {
					t.Fatal(err)
				}
				if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
					t.Fatal(err)
				}
			}
			err = m.automaticStepAt(t.Context(), "1.0.0", func() bool { return true }, m.VerifyVersionRoot, func(context.Context, string) error { restarts++; return nil }, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			expected := requests
			if automatic {
				expected++
			}
			if calls.Load() != expected || restarts != map[bool]int{false: 0, true: 1}[automatic] {
				t.Fatal("提交中断未继续，或自动接管了人工计划")
			}
		})
	}
}

func TestAutomaticBackoffRejectsBadVersionAndAllowsNewRelease(t *testing.T) {
	m, calls, selectVersion, offline := automaticFixture(t)
	now, restarts := time.Now().UTC(), 0
	idle := func() bool { return true }
	restart := func(context.Context, string) error { restarts++; return nil }
	offline.Store(true)
	if err := m.automaticStepAt(t.Context(), "1.0.0", idle, m.VerifyVersionRoot, restart, now); err == nil {
		t.Fatal("离线没有报告错误")
	}
	requests := calls.Load()
	if err := m.automaticStepAt(t.Context(), "1.0.0", idle, m.VerifyVersionRoot, restart, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != requests {
		t.Fatal("失败后没有退避")
	}
	offline.Store(false)
	bad := func(context.Context, string) error { return errors.New("候选无法启动") }
	if err := m.automaticStepAt(t.Context(), "1.0.0", idle, bad, restart, now.Add(time.Hour)); err == nil {
		t.Fatal("接受了坏版本")
	}
	status, _ := m.Status(t.Context())
	if status.State.ActiveVersion != "1.0.0" || status.Automatic.FailedVersion != "2.0.0" || len(status.PendingPlanIDs) != 0 || restarts != 0 {
		t.Fatalf("坏版本污染活动状态：%+v", status)
	}
	if err := m.automaticStepAt(t.Context(), "1.0.0", idle, bad, restart, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status(t.Context())
	if status.Automatic.Phase != "blocked_version" {
		t.Fatal("再次尝试安装失败版本")
	}
	selectVersion("3.0.0")
	if err := m.automaticStepAt(t.Context(), "1.0.0", idle, m.VerifyVersionRoot, restart, now.Add(9*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatal("失败计划阻断了后续新版本")
	}
}

func TestFailStartupRestoresOnlyExpectedVersion(t *testing.T) {
	m, _, _, _ := automaticFixture(t)
	plan, _, err := m.PrepareOnline(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	if err := m.FailStartup(t.Context(), "过期版本", "1.0.0", "错误"); err == nil {
		t.Fatal("过期版本撤销了其他更新")
	}
	if err := m.FailStartup(t.Context(), "2.0.0", "1.0.0", "真实启动失败"); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Status(t.Context())
	if status.State.ActiveVersion != "1.0.0" || status.State.RejectedVersion != "2.0.0" || status.State.LastStatus != "startup_rolled_back" {
		t.Fatalf("未恢复旧版本：%+v", status)
	}
	if err := m.FailStartup(t.Context(), "2.0.0", "1.0.0", "重复恢复"); err != nil {
		t.Fatal(err)
	}
}
