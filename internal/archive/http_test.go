// 中文说明：验证自定义 HTTP 对象归档 provider 的协议、校验、恢复和删除边界。
package archive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	"codex-chronicle.local/internal/lifecycle"
)

type httpArchiveTestServer struct {
	server          *httptest.Server
	mu              sync.Mutex
	objects         map[string][]byte
	failManifestPut bool
	putCount        int
}

func newHTTPArchiveTestServer(t *testing.T) *httpArchiveTestServer {
	t.Helper()
	fixture := &httpArchiveTestServer{objects: make(map[string][]byte)}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (s *httpArchiveTestServer) URL() string { return s.server.URL }

func (s *httpArchiveTestServer) handle(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer runtime-token" {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	key := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
	if key == "." || key == "" {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch request.Method {
	case http.MethodPut:
		if strings.HasSuffix(key, "/remote-manifest.json") {
			s.putCount++
			if s.failManifestPut {
				s.failManifestPut = false
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		content, _ := io.ReadAll(request.Body)
		_ = request.Body.Close()
		s.objects[key] = append([]byte(nil), content...)
		writer.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		content, ok := s.objects[key]
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(content)
	case http.MethodDelete:
		if _, ok := s.objects[key]; !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		delete(s.objects, key)
		writer.WriteHeader(http.StatusNoContent)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *httpArchiveTestServer) tamperFirstChunk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, content := range s.objects {
		if strings.Contains(key, "/files/") {
			s.objects[key] = append([]byte("tampered"), content...)
			return
		}
	}
}

func (s *httpArchiveTestServer) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func TestHTTPArchiveProviderUploadsVerifiesDownloadsAndDeletes(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newHTTPArchiveTestServer(t)
	server.failManifestPut = true
	provider, err := NewHTTPArchiveProvider(HTTPArchiveConfig{BaseURL: server.URL(), RootPath: "archives", Token: "runtime-token", ChunkBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "batch-http-001")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.FileCount != 2 || result.ChunkCount < 2 || server.putCount < 2 {
		t.Fatalf("unexpected HTTP upload result: %+v puts=%d", result, server.putCount)
	}
	archives, err := provider.ListArchives(context.Background())
	if err != nil || len(archives) != 1 || archives[0].ArchiveID != "batch-http-001" {
		t.Fatalf("unexpected HTTP archive list: archives=%+v err=%v", archives, err)
	}
	verified, err := provider.VerifyArchive(context.Background(), "batch-http-001")
	if err != nil || !verified.Verified {
		t.Fatalf("HTTP archive verification failed: result=%+v err=%v", verified, err)
	}
	downloadRoot := t.TempDir()
	downloaded, err := provider.DownloadArchive(context.Background(), "batch-http-001", downloadRoot)
	if err != nil || !downloaded.Verified {
		t.Fatalf("HTTP archive download failed: result=%+v err=%v", downloaded, err)
	}
	if _, err := VerifyLocal(context.Background(), downloadedLocalDirectory(downloadRoot, "batch-http-001")); err != nil {
		t.Fatalf("downloaded HTTP archive failed local verification: %v", err)
	}

	server.tamperFirstChunk()
	if _, err := provider.VerifyArchive(context.Background(), "batch-http-001"); err == nil {
		t.Fatal("expected tampered HTTP archive verification rejection")
	}
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "batch-http-001"); err != nil {
		t.Fatalf("same-source HTTP retry should repair tampered objects: %v", err)
	}
	if err := provider.DeleteArchive(context.Background(), "batch-http-001"); err != nil {
		t.Fatal(err)
	}
	if server.objectCount() != 1 {
		t.Fatalf("HTTP archive deletion should retain only the empty index: objects=%d", server.objectCount())
	}
}
