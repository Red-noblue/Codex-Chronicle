// 中文说明：通过独立更新进程和真实 HTTP 流验证强杀后的片段保留、跨进程续传及版本切换中断恢复。
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOnlineDownloadProcessInterruption(t *testing.T) {
	if root := os.Getenv("CHRONICLE_TEST_DOWNLOAD_ROOT"); root != "" {
		manager, err := NewManager(root)
		if err != nil {
			t.Fatal(err)
		}
		var release OnlineRelease
		content, _ := os.ReadFile(filepath.Join(root, "test-release.json"))
		if err := json.Unmarshal(content, &release); err != nil {
			t.Fatal(err)
		}
		_, err = manager.DownloadOnline(t.Context(), release)
		t.Fatalf("故障进程不应完成下载：%v", err)
	}
	root := filepath.Join(t.TempDir(), "updates")
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte(strings.Repeat("下载测试", 10000))
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(artifact)))
		if healthy.Load() {
			if r.Header.Get("Range") != "bytes=1024-" {
				t.Errorf("强杀恢复后没有请求续传：%s", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(artifact)-1024))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 1024-%d/%d", len(artifact)-1, len(artifact)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(artifact[1024:])
			return
		}
		_, _ = w.Write(artifact[:1024])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	release := onlineTestRelease("2.0.0", artifact, runtime.GOOS, runtime.GOARCH)
	release.URL = server.URL + "/release.zip"
	content, _ := json.Marshal(release)
	if err := os.WriteFile(filepath.Join(root, "test-release.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestOnlineDownloadProcessInterruption$")
	command.Env = append(os.Environ(), "CHRONICLE_TEST_DOWNLOAD_ROOT="+root)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	var temporary string
	waitUpdateTest(t, func() bool {
		paths, _ := filepath.Glob(filepath.Join(root, partialDownloadsDir, "*.part"))
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				temporary = path
				return true
			}
		}
		return false
	})
	if err := manager.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporary); err != nil {
		t.Fatal("恢复流程删除了仍在进行的下载")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if err := manager.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(temporary); err != nil || info.Size() != 1024 {
		t.Fatalf("强杀后没有保留可续传片段：%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, BundlesDirName))
	if err != nil || len(entries) != 0 {
		t.Fatalf("强杀后发布了未完成的安装包：%v %v", entries, err)
	}
	healthy.Store(true)
	if _, err := manager.DownloadOnline(t.Context(), release); err != nil {
		t.Fatalf("强杀恢复后无法重新下载：%v", err)
	}
}

func TestUpdateActivationProcessInterruption(t *testing.T) {
	if root := os.Getenv("CHRONICLE_TEST_ACTIVATION_ROOT"); root != "" {
		manager, err := NewManager(root)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := manager.Prepare(t.Context(), os.Getenv("CHRONICLE_TEST_ACTIVATION_BUNDLE"), false)
		if err != nil {
			t.Fatal(err)
		}
		_, err = manager.Activate(t.Context(), plan, func(context.Context, string) error {
			if err := os.WriteFile(filepath.Join(root, "test-paused"), []byte("ready"), 0o600); err != nil {
				return err
			}
			time.Sleep(time.Hour)
			return nil
		})
		t.Fatalf("故障进程不应完成切换：%v", err)
	}
	root := filepath.Join(t.TempDir(), "updates")
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	stable := makeBundle(t, "1.0.0", "旧版本仍可用")
	plan, err := manager.Prepare(t.Context(), stable, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(t.Context(), plan, manager.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	candidate := makeBundle(t, "2.0.0", "中断的更新")
	command := exec.Command(os.Args[0], "-test.run=^TestUpdateActivationProcessInterruption$")
	command.Env = append(os.Environ(), "CHRONICLE_TEST_ACTIVATION_ROOT="+root, "CHRONICLE_TEST_ACTIVATION_BUNDLE="+candidate)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	waitUpdateTest(t, func() bool { _, err := os.Stat(filepath.Join(root, "test-paused")); return err == nil })
	if err := manager.Recover(t.Context()); err == nil {
		t.Fatal("另一进程正在切换版本时，恢复流程没有阻断")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if err := manager.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(t.Context())
	if err != nil || status.RecoveryRequired || status.State.ActiveVersion != "1.0.0" {
		t.Fatalf("中断改变了旧版本或恢复未结束：%+v %v", status, err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, VersionDir, ".staging-*"))
	if len(paths) != 0 {
		t.Fatalf("切换半成品未清理：%v", paths)
	}
	if err := manager.VerifyVersionRoot(t.Context(), filepath.Join(root, VersionDir, "1.0.0")); err != nil {
		t.Fatal(err)
	}
}

func waitUpdateTest(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("等待故障实验检查点超时")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
