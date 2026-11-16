// 中文说明：验证 S3 provider 的 SigV4 请求、分片上传、列表、校验、恢复、重试和删除边界。
package archive

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	"codex-chronicle.local/internal/lifecycle"
)

type s3TestServer struct {
	server             *httptest.Server
	mu                 sync.Mutex
	objects            map[string][]byte
	failManifestPut    bool
	sawSignedRequest   bool
	manifestPutAttempt int
}

func newS3TestServer(t *testing.T) *s3TestServer {
	t.Helper()
	fixture := &s3TestServer{objects: make(map[string][]byte)}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (s *s3TestServer) URL() string { return s.server.URL }

func (s *s3TestServer) handle(writer http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	if strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") && request.Header.Get("x-amz-content-sha256") != "" && request.Header.Get("x-amz-date") != "" {
		s.sawSignedRequest = true
	}
	s.mu.Unlock()
	key := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
	keyParts := strings.SplitN(key, "/", 2)
	if len(keyParts) != 2 || keyParts[0] != "bucket" {
		if request.Method == http.MethodGet && request.URL.Query().Get("list-type") == "2" {
			s.writeList(writer, request, "")
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	key = keyParts[1]
	if request.Method == http.MethodGet && request.URL.Query().Get("list-type") == "2" {
		s.writeList(writer, request, key)
		return
	}
	switch request.Method {
	case http.MethodPut:
		content, _ := io.ReadAll(request.Body)
		_ = request.Body.Close()
		s.mu.Lock()
		if strings.HasSuffix(key, "/remote-manifest.json") {
			s.manifestPutAttempt++
			if s.failManifestPut {
				s.failManifestPut = false
				s.mu.Unlock()
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		s.objects[key] = append([]byte(nil), content...)
		s.mu.Unlock()
		writer.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.mu.Lock()
		content, ok := s.objects[key]
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
		_, ok := s.objects[key]
		delete(s.objects, key)
		s.mu.Unlock()
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *s3TestServer) writeList(writer http.ResponseWriter, request *http.Request, _ string) {
	prefix := request.URL.Query().Get("prefix")
	delimiter := request.URL.Query().Get("delimiter")
	type content struct {
		Key string `xml:"Key"`
	}
	type commonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	type result struct {
		XMLName      xml.Name       `xml:"ListBucketResult"`
		IsTruncated  bool           `xml:"IsTruncated"`
		Contents     []content      `xml:"Contents"`
		CommonPrefix []commonPrefix `xml:"CommonPrefixes"`
	}
	resultValue := result{}
	s.mu.Lock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	s.mu.Unlock()
	for _, key := range keys {
		if delimiter == "" {
			resultValue.Contents = append(resultValue.Contents, content{Key: key})
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		if index := strings.Index(rest, delimiter); index >= 0 {
			resultValue.CommonPrefix = append(resultValue.CommonPrefix, commonPrefix{Prefix: prefix + rest[:index+len(delimiter)]})
		} else {
			resultValue.Contents = append(resultValue.Contents, content{Key: key})
		}
	}
	// S3 保证 CommonPrefixes 去重；测试服务也保持相同语义。
	seen := make(map[string]struct{})
	unique := resultValue.CommonPrefix[:0]
	for _, item := range resultValue.CommonPrefix {
		if _, ok := seen[item.Prefix]; ok {
			continue
		}
		seen[item.Prefix] = struct{}{}
		unique = append(unique, item)
	}
	resultValue.CommonPrefix = unique
	writer.Header().Set("Content-Type", "application/xml")
	writer.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(writer).Encode(resultValue)
}

func (s *s3TestServer) tamperFirstChunk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range s.objects {
		if strings.Contains(key, "/files/") {
			s.objects[key] = append([]byte("tampered"), value...)
			return
		}
	}
}

func (s *s3TestServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func TestS3ProviderUploadsDownloadsVerifiesAndDeletes(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newS3TestServer(t)
	server.failManifestPut = true
	provider, err := NewS3Provider(S3Config{Endpoint: server.URL(), Region: "us-east-1", Bucket: "bucket", Prefix: "archives", AccessKeyID: "access", SecretAccessKey: "secret", PathStyle: true, ChunkBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "s3-001")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.FileCount != 2 || result.ChunkCount < 2 || server.count() == 0 {
		t.Fatalf("unexpected S3 upload result: %+v", result)
	}
	server.mu.Lock()
	signed := server.sawSignedRequest
	manifestAttempts := server.manifestPutAttempt
	server.mu.Unlock()
	if !signed || manifestAttempts < 2 {
		t.Fatalf("S3 request was not signed or retry did not occur: signed=%v attempts=%d", signed, manifestAttempts)
	}
	server.mu.Lock()
	server.objects["archives/s3-001/files/orphan.part-999"] = []byte("orphan")
	server.mu.Unlock()
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "s3-001"); err != nil {
		t.Fatalf("S3 same-source retry failed: %v", err)
	}
	server.mu.Lock()
	_, orphanExists := server.objects["archives/s3-001/files/orphan.part-999"]
	server.mu.Unlock()
	if orphanExists {
		t.Fatal("S3 same-source retry left an unreferenced chunk")
	}
	archives, err := provider.ListArchives(context.Background())
	if err != nil || len(archives) != 1 || archives[0].ArchiveID != "s3-001" {
		t.Fatalf("unexpected S3 archive list: %+v err=%v", archives, err)
	}
	if verified, verifyErr := provider.VerifyArchive(context.Background(), "s3-001"); verifyErr != nil || !verified.Verified {
		t.Fatalf("S3 verification failed: %+v err=%v", verified, verifyErr)
	}
	destination := t.TempDir()
	downloaded, err := provider.DownloadArchive(context.Background(), "s3-001", destination)
	if err != nil || !downloaded.Verified {
		t.Fatalf("S3 download failed: %+v err=%v", downloaded, err)
	}
	if _, err := VerifyLocal(context.Background(), downloadedLocalDirectory(destination, "s3-001")); err != nil {
		t.Fatalf("downloaded S3 archive failed local verification: %v", err)
	}
	if _, err := provider.DownloadArchive(context.Background(), "s3-001", destination); err == nil {
		t.Fatal("expected existing S3 download destination rejection")
	}
	if err := provider.DeleteArchive(context.Background(), "s3-001"); err != nil {
		t.Fatal(err)
	}
	if server.count() != 0 {
		t.Fatalf("S3 remote archive objects remained after deletion: %d", server.count())
	}
}

func TestS3ProviderRejectsTamperingAndDifferentSourceReuse(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := CreateLocal(context.Background(), plan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newS3TestServer(t)
	provider, err := NewS3Provider(S3Config{Endpoint: server.URL(), Region: "us-east-1", Bucket: "bucket", Prefix: "archives", AccessKeyID: "access", SecretAccessKey: "secret", PathStyle: true, ChunkBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), localArchive.Directory, "s3-002"); err != nil {
		t.Fatal(err)
	}
	server.tamperFirstChunk()
	if _, err := provider.VerifyArchive(context.Background(), "s3-002"); err == nil {
		t.Fatal("expected tampered S3 archive rejection")
	}
	secondPlan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	secondArchive, err := CreateLocal(context.Background(), secondPlan, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(context.Background(), secondArchive.Directory, "s3-002"); err == nil {
		t.Fatal("expected different source manifest reuse rejection")
	}
}

func TestS3SigV4SignatureCanBeRecomputed(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "examplebucket.s3.amazonaws.com"
	request.Header.Set("x-amz-date", "20130524T000000Z")
	request.Header.Set("x-amz-content-sha256", sha256Hex(nil))
	if err := signS3Request(request, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "s3", sha256Hex(nil)); err != nil {
		t.Fatal(err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=14f6a0997b2b70a86f4726658a6575b5109092ccb5fd328f51b369c44b4ac958"
	if got := request.Header.Get("Authorization"); got != want {
		t.Fatalf("unexpected SigV4 authorization: got=%s want=%s", got, want)
	}
}

func TestS3ProviderSupportsVirtualHostAndEndpointPath(t *testing.T) {
	provider, err := NewS3Provider(S3Config{Endpoint: "https://objects.example.com/api", Region: "cn-north-1", Bucket: "bucket", AccessKeyID: "access", SecretAccessKey: "secret", PathStyle: false})
	if err != nil {
		t.Fatal(err)
	}
	object, err := provider.objectURL("archives/a/remote-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if object.Host != "bucket.objects.example.com" || object.EscapedPath() != "/api/archives/a/remote-manifest.json" {
		t.Fatalf("unexpected virtual-host S3 URL: %s", object.String())
	}
}
