// 中文说明：验证 WebDAV 归档 provider 的上传、校验、恢复、列表、删除和认证边界。
package archive

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

type webDAVTestServer struct {
	server             *httptest.Server
	mu                 sync.Mutex
	files              map[string][]byte
	failManifestPut    bool
	username           string
	password           string
	bearer             string
	manifestPutAttempt int
}

func newWebDAVTestServer(t *testing.T) *webDAVTestServer {
	t.Helper()
	fixture := &webDAVTestServer{files: make(map[string][]byte)}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (s *webDAVTestServer) URL() string { return s.server.URL + "/dav" }

func (s *webDAVTestServer) handle(writer http.ResponseWriter, request *http.Request) {
	if s.username != "" || s.bearer != "" {
		basicUser, basicPassword, basicOK := request.BasicAuth()
		bearerOK := s.bearer != "" && request.Header.Get("Authorization") == "Bearer "+s.bearer
		if (!basicOK || basicUser != s.username || basicPassword != s.password) && !bearerOK {
			writer.Header().Set("WWW-Authenticate", `Basic realm="webdav-test"`)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	key := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
	if strings.HasSuffix(request.URL.Path, "/") {
		key = strings.TrimSuffix(key, "/")
	}
	switch request.Method {
	case http.MethodPut:
		content, _ := io.ReadAll(request.Body)
		_ = request.Body.Close()
		if strings.HasSuffix(key, "/remote-manifest.json") {
			s.mu.Lock()
			s.manifestPutAttempt++
			fail := s.failManifestPut
			s.failManifestPut = false
			s.mu.Unlock()
			if fail {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		s.mu.Lock()
		s.files[key] = content
		s.mu.Unlock()
		writer.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		s.mu.Lock()
		content, ok := s.files[key]
		content = append([]byte(nil), content...)
		s.mu.Unlock()
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(content)
	case http.MethodDelete:
		s.mu.Lock()
		_, ok := s.files[key]
		delete(s.files, key)
		s.mu.Unlock()
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	case webDAVMethodPropfind:
		s.writePropfind(writer, key)
	case "MKCOL":
		writer.WriteHeader(http.StatusCreated)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *webDAVTestServer) writePropfind(writer http.ResponseWriter, root string) {
	prefix := strings.Trim(root, "/")
	if prefix != "" {
		prefix += "/"
	}
	entries := map[string]struct{}{strings.TrimSuffix(prefix, "/"): {}}
	s.mu.Lock()
	for key := range s.files {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		child := strings.SplitN(rest, "/", 2)[0]
		if child != "" {
			entries[prefix+child] = struct{}{}
		}
	}
	s.mu.Unlock()

	type response struct {
		Href string `xml:"D:href"`
	}
	type multistatus struct {
		XMLName  xml.Name   `xml:"D:multistatus"`
		Response []response `xml:"D:response"`
	}
	result := multistatus{Response: make([]response, 0, len(entries))}
	for entry := range entries {
		href := "/" + entry
		if entry != strings.TrimSuffix(prefix, "/") {
			href += "/"
		}
		result.Response = append(result.Response, response{Href: href})
	}
	writer.Header().Set("Content-Type", "application/xml; charset=utf-8")
	writer.WriteHeader(http.StatusMultiStatus)
	_ = xml.NewEncoder(writer).Encode(result)
}

func (s *webDAVTestServer) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.files[key]
	return ok
}

func (s *webDAVTestServer) tamperFirstChunk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range s.files {
		if strings.Contains(key, "/files/") {
			s.files[key] = append([]byte("tampered"), value...)
			return
		}
	}
}

func TestWebDAVProviderUploadsListsDownloadsVerifiesAndDeletes(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecyclePlanForWebDAV(root, conversation, files)
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newWebDAVTestServer(t)
	server.username, server.password = "alice", "secret"
	provider, err := NewWebDAVProvider(WebDAVConfig{BaseURL: server.URL(), RootPath: "archives", Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "webdav-001")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.FileCount != 2 || result.ChunkCount < 2 || !server.has("dav/archives/webdav-001/remote-manifest.json") {
		t.Fatalf("unexpected WebDAV upload result: %+v", result)
	}
	archives, err := provider.ListArchives(context.Background())
	if err != nil || len(archives) != 1 || archives[0].ArchiveID != "webdav-001" {
		t.Fatalf("unexpected WebDAV archive list: %+v err=%v", archives, err)
	}
	verified, err := provider.VerifyArchive(context.Background(), "webdav-001")
	if err != nil || !verified.Verified {
		t.Fatalf("WebDAV verification failed: %+v err=%v", verified, err)
	}
	destination := t.TempDir()
	downloaded, err := provider.DownloadArchive(context.Background(), "webdav-001", destination)
	if err != nil || !downloaded.Verified {
		t.Fatalf("WebDAV download failed: %+v err=%v", downloaded, err)
	}
	if _, err := VerifyLocal(context.Background(), filepath.Join(destination, "webdav-001")); err != nil {
		t.Fatalf("downloaded WebDAV archive failed local verification: %v", err)
	}
	if _, err := provider.DownloadArchive(context.Background(), "webdav-001", destination); err == nil {
		t.Fatal("expected existing WebDAV download destination rejection")
	}
	if err := provider.DeleteArchive(context.Background(), "webdav-001"); err != nil {
		t.Fatal(err)
	}
	if server.has("dav/archives/webdav-001/remote-manifest.json") {
		t.Fatal("WebDAV remote manifest remained after deletion")
	}
}

func TestWebDAVProviderRejectsTamperingAndDifferentSourceReuse(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecyclePlanForWebDAV(root, conversation, files)
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newWebDAVTestServer(t)
	server.bearer = "bearer-secret"
	provider, err := NewWebDAVProvider(WebDAVConfig{BaseURL: server.URL(), RootPath: "archives", Token: "bearer-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "webdav-002"); err != nil {
		t.Fatal(err)
	}
	server.tamperFirstChunk()
	if _, err := provider.VerifyArchive(context.Background(), "webdav-002"); err == nil {
		t.Fatal("expected tampered WebDAV archive rejection")
	}
	secondPlan, err := lifecyclePlanForWebDAV(root, conversation, files)
	if err != nil {
		t.Fatal(err)
	}
	secondArchive, err := CreateLocal(context.Background(), secondPlan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), secondArchive.Directory, "webdav-002"); err == nil {
		t.Fatal("expected different source manifest reuse rejection")
	}
}

func TestWebDAVProviderRetriesSameArchiveIDAfterManifestFailure(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecyclePlanForWebDAV(root, conversation, files)
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newWebDAVTestServer(t)
	server.failManifestPut = true
	provider, err := NewWebDAVProvider(WebDAVConfig{BaseURL: server.URL(), RootPath: "archives", Token: "bearer-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "webdav-003"); err == nil {
		t.Fatal("expected first manifest publication failure")
	}
	if server.has("dav/archives/webdav-003/remote-manifest.json") {
		t.Fatal("failed manifest publication should not leave a manifest")
	}
	result, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "webdav-003")
	if err != nil || !result.Verified {
		t.Fatalf("same archive ID retry failed: %+v err=%v", result, err)
	}
}

func TestNewWebDAVProviderRejectsInvalidConfig(t *testing.T) {
	if _, err := NewWebDAVProvider(WebDAVConfig{BaseURL: "ftp://example.test"}); err == nil {
		t.Fatal("expected unsupported WebDAV scheme rejection")
	}
	if _, err := NewWebDAVProvider(WebDAVConfig{BaseURL: "https://example.test", RootPath: "../outside"}); err == nil {
		t.Fatal("expected unsafe WebDAV root rejection")
	}
}

func lifecyclePlanForWebDAV(root string, conversation domain.ConversationSummary, files []domain.FileInventory) (lifecycle.OperationPlan, error) {
	return lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
}

// 编译期检查 WebDAV provider 满足生命周期需要的三个远端能力接口。
var (
	_ RemoteArchiveProvider = (*WebDAVProvider)(nil)
	_ RemoteArchiveCatalog  = (*WebDAVProvider)(nil)
	_ RemoteArchiveDeletion = (*WebDAVProvider)(nil)
)
