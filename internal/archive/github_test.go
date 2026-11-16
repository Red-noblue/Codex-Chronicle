// 中文说明：验证 GitHub Contents 分片归档的上传、断点重试、下载校验和删除边界。
package archive

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-chronicle.local/internal/lifecycle"
)

func TestGitHubProviderUploadsDownloadsVerifiesAndDeletesChunks(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newGitHubContentsTestServer()
	defer server.Close()
	provider, err := NewGitHubProvider(GitHubConfig{
		BaseURL: server.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", RootPath: "archives", ChunkBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "batch-001")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.FileCount != 2 || result.ChunkCount < 2 {
		t.Fatalf("unexpected GitHub upload result: %+v", result)
	}
	if !server.has("archives/batch-001/remote-manifest.json") {
		t.Fatal("remote manifest was not published")
	}
	server.mu.Lock()
	server.files["archives/batch-001/files/orphan.part-999999"] = []byte("orphan")
	server.mu.Unlock()

	// 同一批次重试跳过相同分片，仅修补缺失或损坏对象。
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "batch-001"); err != nil {
		t.Fatalf("resumable re-upload failed: %v", err)
	}
	if server.has("archives/batch-001/files/orphan.part-999999") {
		t.Fatal("resumable upload left an unreferenced orphan chunk")
	}
	if server.putCount() != result.ChunkCount+1 {
		t.Fatalf("相同分片不应重复 PUT: puts=%d", server.putCount())
	}

	downloadRoot := t.TempDir()
	downloaded, err := provider.DownloadArchive(context.Background(), "batch-001", downloadRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !downloaded.Verified || downloaded.FileCount != result.FileCount {
		t.Fatalf("unexpected GitHub download result: %+v", downloaded)
	}
	if _, err := VerifyLocal(context.Background(), downloadedLocalDirectory(downloadRoot, "batch-001")); err != nil {
		t.Fatalf("downloaded local archive failed verification: %v", err)
	}
	verified, err := provider.VerifyArchive(context.Background(), "batch-001")
	if err != nil || !verified.Verified || verified.ChunkCount != result.ChunkCount {
		t.Fatalf("remote archive verification failed: result=%+v err=%v", verified, err)
	}

	// 篡改远端分片必须在下载阶段被拒绝。
	server.tamperFirstChunk()
	if _, err := provider.DownloadArchive(context.Background(), "batch-001", t.TempDir()); err == nil {
		t.Fatal("expected tampered remote chunk rejection")
	}
	if _, err := provider.VerifyArchive(context.Background(), "batch-001"); err == nil {
		t.Fatal("expected tampered remote archive verification rejection")
	}
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "batch-001"); err != nil {
		t.Fatal(err)
	}
	if err := provider.DeleteArchive(context.Background(), "batch-001"); err != nil {
		t.Fatal(err)
	}
	if server.count() != 0 {
		t.Fatalf("remote archive objects remained after deletion: %d", server.count())
	}
}

func TestGiteeProviderUsesIndependentIdentityAndContentsProtocol(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newGitHubContentsTestServer()
	defer server.Close()
	provider, err := NewGiteeProvider(GiteeConfig{
		BaseURL: server.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", RootPath: "archives", ChunkBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	info := provider.ProviderInfo()
	if info.ID != "gitee" || info.Name != "Gitee" || info.TargetRequired {
		t.Fatalf("unexpected Gitee provider identity: %+v", info)
	}
	result, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "gitee-001")
	if err != nil || !result.Verified {
		t.Fatalf("Gitee upload failed: result=%+v err=%v", result, err)
	}
	archives, err := provider.ListArchives(context.Background())
	if err != nil || len(archives) != 1 || archives[0].ArchiveID != "gitee-001" {
		t.Fatalf("Gitee list failed: archives=%+v err=%v", archives, err)
	}
	if _, err := provider.VerifyArchive(context.Background(), "gitee-001"); err != nil {
		t.Fatalf("Gitee verification failed: %v", err)
	}
	download, err := provider.DownloadArchive(context.Background(), "gitee-001", t.TempDir())
	if err != nil || !download.Verified {
		t.Fatalf("Gitee download failed: result=%+v err=%v", download, err)
	}
	if err := provider.DeleteArchive(context.Background(), "gitee-001"); err != nil {
		t.Fatalf("Gitee deletion failed: %v", err)
	}
}

func TestNewGitHubProviderRejectsUnsafeAndUnsupportedProxyConfig(t *testing.T) {
	if _, err := NewGitHubProvider(GitHubConfig{Owner: "owner", Repository: "repo", Token: "token", RootPath: "../outside"}); err == nil {
		t.Fatal("expected unsafe root path rejection")
	}
	if _, err := NewGitHubProvider(GitHubConfig{Owner: "owner", Repository: "repo", Token: "token", ProxyURL: "ftp://127.0.0.1:1080"}); err == nil {
		t.Fatal("expected unsupported proxy rejection")
	}
	if _, err := NewGitHubProvider(GitHubConfig{Owner: "owner", Repository: "repo", Token: "token", ProxyURL: "http://127.0.0.1:8080"}); err != nil {
		t.Fatalf("valid HTTP proxy was rejected: %v", err)
	}
	if _, err := NewGitHubProvider(GitHubConfig{Owner: "owner", Repository: "repo", Token: "token", ProxyURL: "socks5://127.0.0.1:1080"}); err != nil {
		t.Fatalf("valid SOCKS5 proxy was rejected: %v", err)
	}
}

func TestNewGitHubArchiveIDIsShortAndSafe(t *testing.T) {
	first, err := NewGitHubArchiveID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewGitHubArchiveID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) > 32 || !strings.HasPrefix(first, "cc-") {
		t.Fatalf("unexpected generated archive IDs: %q %q", first, second)
	}
	if err := validateArchiveID(first); err != nil {
		t.Fatalf("generated archive ID is unsafe: %v", err)
	}
}

func TestGitHubProviderUsesSOCKS5Proxy(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newGitHubContentsTestServer()
	defer server.Close()
	proxy := newSOCKS5TestProxy(t)
	defer proxy.Close()
	for _, scheme := range []string{"socks5", "socks5h"} {
		baseURL := server.URL()
		if scheme == "socks5h" {
			baseURL = strings.Replace(baseURL, "127.0.0.1", "localhost", 1)
		}
		provider, err := NewGitHubProvider(GitHubConfig{
			BaseURL: baseURL, Owner: "owner", Repository: "chronicle", Token: "runtime-token", RootPath: "archives", ChunkBytes: 4,
			ProxyURL: scheme + "://" + proxy.Addr(),
		})
		if err != nil {
			t.Fatal(err)
		}
		archiveID := "batch-" + scheme
		if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, archiveID); err != nil {
			t.Fatalf("%s proxied upload failed: %v", scheme, err)
		}
		if !server.has("archives/" + archiveID + "/remote-manifest.json") {
			t.Fatalf("%s proxied upload did not publish manifest", scheme)
		}
	}
}

func TestGitHubProviderUsesHTTPProxy(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newGitHubContentsTestServer()
	defer server.Close()
	proxy := httptest.NewServer(http.HandlerFunc(httpForwardProxyHandler))
	defer proxy.Close()
	provider, err := NewGitHubProvider(GitHubConfig{
		BaseURL: server.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", RootPath: "archives", ChunkBytes: 4,
		ProxyURL: proxy.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "batch-http-proxy"); err != nil {
		t.Fatalf("HTTP proxied upload failed: %v", err)
	}
	if !server.has("archives/batch-http-proxy/remote-manifest.json") {
		t.Fatal("HTTP proxied upload did not publish manifest")
	}
}

func TestGitHubProviderListsValidArchivesAndSkipsOtherDirectories(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	firstPlan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	firstArchive, err := CreateLocal(context.Background(), firstPlan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	secondArchive, err := CreateLocal(context.Background(), secondPlan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newGitHubContentsTestServer()
	defer server.Close()
	provider, err := NewGitHubProvider(GitHubConfig{
		BaseURL: server.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", RootPath: "archives", ChunkBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), firstArchive.Directory, "batch-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), secondArchive.Directory, "batch-new"); err != nil {
		t.Fatal(err)
	}
	server.replaceRemoteManifest(t, "archives/batch-old/remote-manifest.json", func(manifest *GitHubRemoteManifest) {
		manifest.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	})
	server.replaceRemoteManifest(t, "archives/batch-new/remote-manifest.json", func(manifest *GitHubRemoteManifest) {
		manifest.CreatedAt = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	})
	server.mu.Lock()
	server.files["archives/notes/readme.txt"] = []byte("other repository data")
	server.mu.Unlock()
	archives, err := provider.ListArchives(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 2 || archives[0].ArchiveID != "batch-new" || archives[1].ArchiveID != "batch-old" {
		t.Fatalf("unexpected archive list: %+v", archives)
	}
	if archives[0].FileCount != 2 || archives[0].ChunkCount < 2 || archives[0].SourceManifestSHA256 == "" {
		t.Fatalf("archive summary is incomplete: %+v", archives[0])
	}
}

func TestGitHubProviderRejectsManifestReferencesOutsideArchive(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newGitHubContentsTestServer()
	defer server.Close()
	provider, err := NewGitHubProvider(GitHubConfig{
		BaseURL: server.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", RootPath: "archives", ChunkBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "batch-unsafe"); err != nil {
		t.Fatal(err)
	}
	server.replaceRemoteManifest(t, "archives/batch-unsafe/remote-manifest.json", func(manifest *GitHubRemoteManifest) {
		manifest.Files[0].Chunks[0].Path = "archives/another-archive/files/foreign.part-000000"
	})
	if _, err := provider.DownloadArchive(context.Background(), "batch-unsafe", t.TempDir()); err == nil {
		t.Fatal("expected cross-archive manifest reference rejection")
	}
}

func TestGitHubProviderPreservesEncryptedLocalArchive(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true, Encrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocalEncrypted(context.Background(), plan, t.TempDir(), "归档密码")
	if err != nil {
		t.Fatal(err)
	}
	server := newGitHubContentsTestServer()
	defer server.Close()
	provider, err := NewGitHubProvider(GitHubConfig{
		BaseURL: server.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", RootPath: "archives", ChunkBytes: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "batch-encrypted"); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if _, err := provider.DownloadArchive(context.Background(), "batch-encrypted", destination); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLocalEncrypted(context.Background(), filepath.Join(destination, "batch-encrypted"), "归档密码"); err != nil {
		t.Fatalf("downloaded encrypted archive failed verification: %v", err)
	}
}

func downloadedLocalDirectory(root, archiveID string) string {
	return filepath.Join(root, archiveID)
}

type socks5TestProxy struct {
	listener net.Listener
	wait     sync.WaitGroup
}

func newSOCKS5TestProxy(t *testing.T) *socks5TestProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &socks5TestProxy{listener: listener}
	proxy.wait.Add(1)
	go proxy.acceptLoop()
	return proxy
}

func (p *socks5TestProxy) Addr() string { return p.listener.Addr().String() }

func (p *socks5TestProxy) Close() {
	_ = p.listener.Close()
	p.wait.Wait()
}

func (p *socks5TestProxy) acceptLoop() {
	defer p.wait.Done()
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.wait.Add(1)
		go func() {
			defer p.wait.Done()
			p.handle(connection)
		}()
	}
}

func (p *socks5TestProxy) handle(connection net.Conn) {
	defer connection.Close()
	methodHeader := make([]byte, 2)
	if _, err := io.ReadFull(connection, methodHeader); err != nil {
		return
	}
	methods := make([]byte, int(methodHeader[1]))
	if _, err := io.ReadFull(connection, methods); err != nil {
		return
	}
	if _, err := connection.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	requestHeader := make([]byte, 4)
	if _, err := io.ReadFull(connection, requestHeader); err != nil || requestHeader[0] != 0x05 || requestHeader[1] != 0x01 {
		return
	}
	host, ok := readSOCKS5TestHost(connection, requestHeader[3])
	if !ok {
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(connection, portBytes); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)
	upstream, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		_, _ = connection.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := connection.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	copyDone := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, connection); copyDone <- struct{}{} }()
	go func() { _, _ = io.Copy(connection, upstream); copyDone <- struct{}{} }()
	<-copyDone
}

func readSOCKS5TestHost(reader io.Reader, addressType byte) (string, bool) {
	switch addressType {
	case 0x01:
		bytes := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, bytes); err != nil {
			return "", false
		}
		return net.IP(bytes).String(), true
	case 0x04:
		bytes := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, bytes); err != nil {
			return "", false
		}
		return net.IP(bytes).String(), true
	case 0x03:
		length := []byte{0}
		if _, err := io.ReadFull(reader, length); err != nil {
			return "", false
		}
		bytes := make([]byte, int(length[0]))
		if _, err := io.ReadFull(reader, bytes); err != nil {
			return "", false
		}
		return string(bytes), true
	default:
		return "", false
	}
}

func httpForwardProxyHandler(writer http.ResponseWriter, request *http.Request) {
	outbound := request.Clone(request.Context())
	outbound.RequestURI = ""
	outbound.Header.Del("Proxy-Connection")
	if !outbound.URL.IsAbs() {
		outbound.URL.Scheme = "http"
		outbound.URL.Host = request.Host
	}
	response, err := http.DefaultTransport.RoundTrip(outbound)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

type githubContentsTestServer struct {
	server *httptest.Server
	mu     sync.Mutex
	files  map[string][]byte
	puts   int
}

func newGitHubContentsTestServer() *githubContentsTestServer {
	testServer := &githubContentsTestServer{files: make(map[string][]byte)}
	testServer.server = httptest.NewServer(http.HandlerFunc(testServer.handle))
	return testServer
}

func (s *githubContentsTestServer) Close()      { s.server.Close() }
func (s *githubContentsTestServer) URL() string { return s.server.URL }

func (s *githubContentsTestServer) handle(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Connection", "close")
	if request.Header.Get("Authorization") != "Bearer runtime-token" && request.Header.Get("Authorization") != "token runtime-token" {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	remotePath := strings.TrimPrefix(request.URL.Path, "/repos/owner/chronicle/contents")
	remotePath = strings.Trim(remotePath, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch request.Method {
	case http.MethodGet:
		content, ok := s.files[remotePath]
		if ok {
			if strings.Contains(request.Header.Get("Accept"), "raw") {
				writer.Write(content)
				return
			}
			writeGitHubContentJSON(writer, remotePath, content)
			return
		}
		entries := s.directoryEntries(remotePath)
		if len(entries) == 0 {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		// Contents 实际忽略分页参数，最多返回 1000 项。
		if len(entries) > 1000 {
			entries = entries[:1000]
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(entries)
	case http.MethodPut:
		var payload struct {
			Content string `json:"content"`
			SHA     string `json:"sha"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if previous, exists := s.files[remotePath]; exists && payload.SHA != githubTestSHA(previous) {
			writer.WriteHeader(http.StatusConflict)
			return
		}
		content, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(payload.Content), ""))
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		s.files[remotePath] = content
		s.puts++
		writeGitHubContentJSON(writer, remotePath, content)
	case http.MethodDelete:
		if _, exists := s.files[remotePath]; !exists {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		var payload struct {
			SHA string `json:"sha"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload.SHA != githubTestSHA(s.files[remotePath]) {
			writer.WriteHeader(http.StatusConflict)
			return
		}
		delete(s.files, remotePath)
		writer.WriteHeader(http.StatusOK)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *githubContentsTestServer) directoryEntries(remotePath string) []map[string]string {
	prefix := strings.Trim(remotePath, "/")
	if prefix != "" {
		prefix += "/"
	}
	entries := make(map[string]string)
	for filePath := range s.files {
		if !strings.HasPrefix(filePath, prefix) {
			continue
		}
		rest := strings.TrimPrefix(filePath, prefix)
		if rest == "" {
			continue
		}
		parts := strings.SplitN(rest, "/", 2)
		entryPath := prefix + parts[0]
		entryType := "file"
		if len(parts) == 2 {
			entryType = "dir"
		}
		entries[entryPath] = entryType
	}
	paths := make([]string, 0, len(entries))
	for entryPath := range entries {
		paths = append(paths, entryPath)
	}
	sort.Strings(paths)
	result := make([]map[string]string, 0, len(paths))
	for _, entryPath := range paths {
		result = append(result, map[string]string{"type": entries[entryPath], "path": entryPath})
	}
	return result
}

func (s *githubContentsTestServer) has(remotePath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.files[remotePath]
	return ok
}

func (s *githubContentsTestServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.files)
}

func (s *githubContentsTestServer) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func (s *githubContentsTestServer) tamperFirstChunk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, content := range s.files {
		if strings.Contains(name, ".part-") {
			s.files[name] = append([]byte("tampered"), content...)
			return
		}
	}
}

func (s *githubContentsTestServer) replaceRemoteManifest(t *testing.T, remotePath string, mutate func(*GitHubRemoteManifest)) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[remotePath]
	if !ok {
		t.Fatalf("remote manifest not found: %s", remotePath)
	}
	var manifest GitHubRemoteManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode remote manifest: %v", err)
	}
	mutate(&manifest)
	updated, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode remote manifest: %v", err)
	}
	s.files[remotePath] = updated
}

func writeGitHubContentJSON(writer http.ResponseWriter, remotePath string, content []byte) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	encoding, body := "base64", base64.StdEncoding.EncodeToString(content)
	if len(content) > 1<<20 {
		encoding, body = "none", ""
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "file", "encoding": encoding, "content": body, "sha": githubTestSHA(content), "path": remotePath,
	})
}

func githubTestSHA(content []byte) string {
	return gitBlobSHA(content)
}
