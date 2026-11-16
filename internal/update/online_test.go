// 中文说明：验证在线版本 feed 的平台筛选、流式下载摘要校验和失败清理边界。
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

func TestCheckOnlineFeedSelectsNewestCompatibleRelease(t *testing.T) {
	artifact := []byte("online-release")
	feed := OnlineFeed{
		FormatVersion: OnlineFeedFormatVersion,
		Product:       "codex-chronicle",
		GeneratedAt:   time.Now().UTC(),
		Releases: []OnlineRelease{
			onlineTestRelease("0.9.0", artifact, runtime.GOOS, runtime.GOARCH),
			onlineTestRelease("9.0.0", artifact, "other-os", runtime.GOARCH),
			onlineTestRelease("1.2.0", artifact, runtime.GOOS, runtime.GOARCH),
			onlineTestRelease("1.1.0", artifact, runtime.GOOS, runtime.GOARCH),
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/feed.json" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(feed)
	}))
	defer server.Close()

	result, err := CheckOnlineFeed(context.Background(), OnlineConfig{FeedURL: server.URL + "/feed.json"}, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Available || result.Release == nil || result.Release.Version != "1.2.0" {
		t.Fatalf("unexpected online check result: %+v", result)
	}
	result, err = CheckOnlineFeed(context.Background(), OnlineConfig{FeedURL: server.URL + "/feed.json"}, "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if result.Available || result.Release != nil {
		t.Fatalf("current version should have no update: %+v", result)
	}
}

func TestOnlineConfigAcceptsSupportedProxySchemes(t *testing.T) {
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		if _, err := normalizeOnlineConfig(OnlineConfig{FeedURL: "https://updates.example.invalid/feed.json", ProxyURL: scheme + "://127.0.0.1:1080"}); err != nil {
			t.Fatalf("proxy scheme %s was rejected: %v", scheme, err)
		}
	}
	if _, err := normalizeOnlineConfig(OnlineConfig{FeedURL: "https://updates.example.invalid/feed.json", ProxyURL: "ftp://127.0.0.1:21"}); err == nil {
		t.Fatal("unsupported proxy scheme was accepted")
	}
}

func TestDownloadOnlineVerifiesSizeHashAndCleansFailures(t *testing.T) {
	artifact := []byte("online-release-content")
	digest := sha256.Sum256(artifact)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/artifact" {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(artifact)
	}))
	defer server.Close()

	manager, err := NewManagerWithOptions(filepath.Join(t.TempDir(), "install"), ManagerOptions{Online: OnlineConfig{FeedURL: server.URL + "/feed.json"}})
	if err != nil {
		t.Fatal(err)
	}
	release := onlineTestRelease("1.0.0", artifact, runtime.GOOS, runtime.GOARCH)
	release.URL = server.URL + "/artifact"
	downloaded, err := manager.DownloadOnline(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	if downloaded.SizeBytes != int64(len(artifact)) || downloaded.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("unexpected downloaded result: %+v", downloaded)
	}
	data, err := os.ReadFile(downloaded.Path)
	if err != nil || string(data) != string(artifact) {
		t.Fatalf("downloaded file mismatch: err=%v content=%q", err, string(data))
	}

	bad := release
	bad.SHA256 = strings.Repeat("0", sha256.Size*2)
	if _, err := manager.DownloadOnline(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("tampered digest was accepted: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(manager.Root(), BundlesDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("failed download left temporary file: %s", entry.Name())
		}
	}
}

func onlineTestRelease(version string, artifact []byte, operatingSystem, architecture string) OnlineRelease {
	digest := sha256.Sum256(artifact)
	return OnlineRelease{
		Version: version, BuildID: "test-" + version, OS: operatingSystem, Arch: architecture,
		URL: "https://updates.example.invalid/" + version + ".zip", SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(artifact)),
	}
}

func TestPrepareOnlineSignedBundleActivationAndFailureIsolation(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"valid", "wrong-version", "wrong-build", "unsigned", "unknown-key", "damaged-signature"} {
		t.Run(scenario, func(t *testing.T) {
			bundle := makeBundle(t, "2.0.0", "在线候选版本")
			manifest, err := BuildManifest(t.Context(), bundle, "2.0.0", "online-build")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "unsigned" {
				key := private
				if scenario == "unknown-key" {
					_, key, err = ed25519.GenerateKey(rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
				}
				manifest, err = SignManifest(manifest, key, "isolated-test-key")
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "damaged-signature" {
					manifest.BuildID = "signature-was-tampered"
				}
			}
			if err := WriteManifest(t.Context(), bundle, manifest); err != nil {
				t.Fatal(err)
			}
			files := make(map[string][]byte)
			if err := filepath.WalkDir(bundle, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				relative, _ := filepath.Rel(bundle, path)
				files[filepath.ToSlash(relative)], err = os.ReadFile(path)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "release.zip")
			if err := writeZIP(archive, files); err != nil {
				t.Fatal(err)
			}
			artifact, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			release := onlineTestRelease("2.0.0", artifact, runtime.GOOS, runtime.GOARCH)
			release.BuildID = "online-build"
			if scenario == "wrong-version" {
				release.Version = "9.0.0"
			}
			if scenario == "wrong-build" {
				release.BuildID = "another-build"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/feed.json" {
					current := release
					current.URL = "http://" + r.Host + "/release.zip"
					_ = json.NewEncoder(w).Encode(OnlineFeed{FormatVersion: 1, Product: "codex-chronicle", Releases: []OnlineRelease{current}})
					return
				}
				_, _ = w.Write(artifact)
			}))
			defer server.Close()
			manager, err := NewManagerWithOptions(filepath.Join(t.TempDir(), "updates"), ManagerOptions{RequireSignature: true, TrustedPublicKeys: [][]byte{public}, Online: OnlineConfig{FeedURL: server.URL + "/feed.json"}})
			if err != nil {
				t.Fatal(err)
			}
			plan, _, err := manager.PrepareOnline(t.Context(), false)
			if scenario != "valid" {
				if err == nil {
					t.Fatal("错误在线包生成了可执行更新计划")
				}
				status, statusErr := manager.Status(t.Context())
				if statusErr != nil || status.State.ActiveVersion != "" || len(status.PendingPlanIDs) != 0 {
					t.Fatalf("失败污染了更新状态：%+v %v", status, statusErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.ActivateByID(t.Context(), plan.ID, manager.VerifyVersionRoot); err != nil {
				t.Fatal(err)
			}
			check, err := manager.CheckOnline(t.Context())
			if err != nil || check.Available {
				t.Fatalf("更新后仍提示相同更新：%+v %v", check, err)
			}
		})
	}
}

func TestDownloadOnlineNetworkFailureCancellationAndRetry(t *testing.T) {
	artifact := []byte(strings.Repeat("流式下载", 1024))
	var mode atomic.Int32
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0:
			w.Header().Set("Content-Length", fmt.Sprint(len(artifact)))
			_, _ = w.Write(artifact[:10])
		case 1:
			_, _ = w.Write(artifact[:10])
			w.(http.Flusher).Flush()
			started <- struct{}{}
			<-r.Context().Done()
		default:
			_, _ = w.Write(artifact)
		}
	}))
	defer server.Close()
	manager, err := NewManager(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	release := onlineTestRelease("2.0.0", artifact, runtime.GOOS, runtime.GOARCH)
	release.URL = server.URL + "/release.zip"
	if _, err := manager.DownloadOnline(t.Context(), release); err == nil {
		t.Fatal("截断的下载被接纳")
	}
	mode.Store(1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := manager.DownloadOnline(ctx, release); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("取消实验未开始下载")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("取消未中断下载：%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(manager.Root(), BundlesDirName))
	if err != nil || len(entries) != 0 {
		t.Fatalf("失败或取消留下半成品：%v %v", entries, err)
	}
	mode.Store(2)
	if _, err := manager.DownloadOnline(t.Context(), release); err != nil {
		t.Fatalf("网络恢复后重试失败：%v", err)
	}
}

func TestOnlineFeedAndDownloadUseConfiguredHTTPProxy(t *testing.T) {
	artifact := []byte("代理返回的更新内容")
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "updates.example.invalid" || r.URL.Scheme != "http" {
			t.Errorf("请求没有按预期进入 HTTP 代理：%s", r.URL)
		}
		requests.Add(1)
		if r.URL.Path == "/feed.json" {
			release := onlineTestRelease("2.0.0", artifact, runtime.GOOS, runtime.GOARCH)
			release.URL = "http://updates.example.invalid/release.zip"
			_ = json.NewEncoder(w).Encode(OnlineFeed{FormatVersion: 1, Product: "codex-chronicle", Releases: []OnlineRelease{release}})
			return
		}
		_, _ = w.Write(artifact)
	}))
	defer proxy.Close()
	manager, err := NewManagerWithOptions(filepath.Join(t.TempDir(), "updates"), ManagerOptions{Online: OnlineConfig{FeedURL: "http://updates.example.invalid/feed.json", ProxyURL: proxy.URL}})
	if err != nil {
		t.Fatal(err)
	}
	check, err := manager.CheckOnline(t.Context())
	if err != nil || check.Release == nil {
		t.Fatalf("通过代理检查失败：%+v %v", check, err)
	}
	if _, err := manager.DownloadOnline(t.Context(), *check.Release); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("feed 和安装包未全部通过代理：%d", requests.Load())
	}
}
