// 中文说明：用真实 HTTP 截断和范围响应验证跨进程续传、完整响应重下、错误范围拒绝及空间不足保护。
package update

import (
	"errors"
	"fmt"
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

func TestDownloadResumesAcrossManagersAndHandlesIgnoredRange(t *testing.T) {
	for _, mode := range []string{"resume", "ignore-range", "wrong-range", "changed-validator"} {
		t.Run(mode, func(t *testing.T) {
			artifact := []byte(strings.Repeat("更新包", 1024))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"package-one"`)
				requestNumber := requests.Add(1)
				if requestNumber == 1 {
					w.Header().Set("Content-Length", fmt.Sprint(len(artifact)))
					_, _ = w.Write(artifact[:100])
					return
				}
				if requestNumber == 3 {
					if r.Header.Get("Range") != "" {
						t.Error("错误续传后没有从头下载")
					}
					_, _ = w.Write(artifact)
					return
				}
				if r.Header.Get("Range") != "bytes=100-" || r.Header.Get("If-Range") != `"package-one"` {
					t.Errorf("没有按片段身份续传：%v", r.Header)
				}
				if mode == "ignore-range" {
					_, _ = w.Write(artifact)
					return
				}
				start := 100
				if mode == "wrong-range" {
					start++
				}
				if mode == "changed-validator" {
					w.Header().Set("ETag", `"another-package"`)
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(artifact)-1, len(artifact)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(artifact[100:])
			}))
			defer server.Close()
			root := t.TempDir()
			m, err := NewManager(root)
			if err != nil {
				t.Fatal(err)
			}
			release := onlineTestRelease("2.0.0", artifact, runtime.GOOS, runtime.GOARCH)
			release.URL = server.URL + "/release.zip"
			if _, err := m.DownloadOnline(t.Context(), release); err == nil {
				t.Fatal("截断下载被当成完整包")
			}
			m, err = NewManager(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			downloaded, err := m.DownloadOnline(t.Context(), release)
			if mode == "wrong-range" || mode == "changed-validator" {
				if err == nil {
					t.Fatal("错误范围或标识被接纳")
				}
				partials, _ := filepath.Glob(filepath.Join(root, partialDownloadsDir, "*.part"))
				if len(partials) != 0 {
					t.Fatal("错误范围之后没有废弃片段")
				}
				if _, err := m.DownloadOnline(t.Context(), release); err != nil {
					t.Fatalf("错误续传之后无法从头下载：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(downloaded.Path)
			if err != nil || string(data) != string(artifact) {
				t.Fatalf("完整包不一致：%v", err)
			}
			partials, _ := filepath.Glob(filepath.Join(root, partialDownloadsDir, "*.part*"))
			if len(partials) != 0 {
				t.Fatal("成功后仍保留续传片段")
			}
		})
	}
}

func TestDownloadInsufficientSpaceDoesNotRequestOrRejectVersion(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.diskSpaceCheck = func(string, int64, string) error {
		return &InsufficientSpaceError{Phase: "下载", RequiredBytes: 100, AvailableBytes: 0}
	}
	release := onlineTestRelease("2.0.0", []byte("包"), runtime.GOOS, runtime.GOARCH)
	release.URL = server.URL
	_, err = m.DownloadOnline(t.Context(), release)
	var space *InsufficientSpaceError
	if !errors.As(err, &space) || calls.Load() != 0 {
		t.Fatalf("空间不足仍下载：%v %d", err, calls.Load())
	}
}

func TestDownloadCorruptedPartialRetriesFullPackage(t *testing.T) {
	artifact := []byte(strings.Repeat("完整包", 100))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Length", fmt.Sprint(len(artifact)))
			_, _ = w.Write([]byte("坏片段xx"))
		case 2:
			offset := len("坏片段xx")
			if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", offset) {
				t.Error("未尝试续传")
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(artifact)-1, len(artifact)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(artifact[offset:])
		default:
			if r.Header.Get("Range") != "" {
				t.Error("片段损坏后没有从头下载")
			}
			_, _ = w.Write(artifact)
		}
	}))
	defer server.Close()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := onlineTestRelease("2.0.0", artifact, runtime.GOOS, runtime.GOARCH)
	release.URL = server.URL
	if _, err := m.DownloadOnline(t.Context(), release); err == nil {
		t.Fatal("截断片段被接受")
	}
	if _, err := m.DownloadOnline(t.Context(), release); err == nil || errors.Is(err, ErrInvalidOnlinePackage) {
		t.Fatalf("损坏片段被接受或永久归咎于版本：%v", err)
	}
	if _, err := m.DownloadOnline(t.Context(), release); err != nil {
		t.Fatalf("无法重新完整下载：%v", err)
	}
}

func TestExpiredPartialCleanupPreservesRecentAndUnknownFiles(t *testing.T) {
	artifact := []byte(strings.Repeat("包", 100))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(artifact)))
		_, _ = w.Write(artifact[:10])
	}))
	defer server.Close()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := onlineTestRelease("2.0.0", artifact, runtime.GOOS, runtime.GOARCH)
	release.URL = server.URL
	if _, err := m.DownloadOnline(t.Context(), release); err == nil {
		t.Fatal("截断下载被接纳")
	}
	paths, _ := filepath.Glob(filepath.Join(m.root, partialDownloadsDir, "*.part"))
	if len(paths) != 1 {
		t.Fatal("没有留下可续传片段")
	}
	unknown := filepath.Join(m.root, partialDownloadsDir, "用户文件.txt")
	if err := os.WriteFile(unknown, []byte("保留"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal("近期片段被删除")
	}
	old := time.Now().Add(-partialDownloadRetention - time.Hour)
	for _, path := range []string{paths[0], paths[0] + ".json", unknown} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("过期片段未清理")
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("未知文件被清理")
	}
}
