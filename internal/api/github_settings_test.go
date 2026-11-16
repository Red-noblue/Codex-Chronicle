// 中文说明：使用本机模拟 GitHub 验证设置持久化、设备绑定、仓库选择及归档往返；不访问真实账号或 .codex。
package api

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

func githubSettingsRequest(t *testing.T, handler http.Handler, method, path string, value any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if value != nil {
		if err := json.NewEncoder(&body).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(method, path, &body))
	if result.Code != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, result.Code, status, result.Body.String())
	}
	for _, secret := range []string{"runtime-token", "device-secret"} {
		if strings.Contains(result.Body.String(), secret) {
			t.Fatal("API 响应泄露模拟凭证")
		}
	}
	return result
}

func TestGitHubSettingsEndToEndWithSimulatedGitHub(t *testing.T) {
	remote := newSettingsGitHubServer(t)
	path := filepath.Join(t.TempDir(), "github-settings.json")
	selection, err := archive.NewFileGitHubArchiveSelectionStore(filepath.Join(t.TempDir(), "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	tokens := archive.NewMemoryGitHubTokenStore()
	options := &LifecycleOptions{RemoteRecoveryRoot: t.TempDir()}
	server := NewServerWithOptions(t.TempDir(), nil, options)
	t.Cleanup(server.Close)
	if err := server.SetGitHubSettings(path, GitHubSettings{}, tokens, "test", selection); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	githubSettingsRequest(t, handler, "GET", "/api/v1/github/settings", nil, 200)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("读取配置不应创建配置文件")
	}
	settings := GitHubSettings{ClientID: "client-test", OAuthBaseURL: remote.URL, APIBaseURL: remote.URL, ChunkMiB: 1}
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/settings", settings, 200)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("配置权限错误: %v %v", info, err)
	}
	connection := githubSettingsRequest(t, handler, "POST", "/api/v1/github/connection-test", nil, 200)
	if strings.Count(connection.Body.String(), `"ok":true`) != 2 {
		t.Fatalf("网络检查失败: %s", connection.Body.String())
	}
	githubSettingsRequest(t, handler, "POST", "/api/v1/github/auth/start", nil, 202)
	deadline := time.Now().Add(3 * time.Second)
	for {
		status := githubSettingsRequest(t, handler, "GET", "/api/v1/github/auth/status", nil, 200)
		if strings.Contains(status.Body.String(), `"credential_available":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("模拟设备绑定未完成")
		}
		time.Sleep(10 * time.Millisecond)
	}
	githubSettingsRequest(t, handler, "GET", "/api/v1/github/repositories", nil, 200)
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/archive-target", map[string]string{"owner": "owner", "repository": "public"}, 409)
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/archive-target", map[string]string{"owner": "owner", "repository": "repo"}, 200)
	// 源文件是隔离样本；调用正式归档 provider，验证配置确实传入了上传、校验和下载链路。
	source := t.TempDir()
	payload := []byte("隔离 GitHub 归档样本\n")
	if err := os.WriteFile(filepath.Join(source, "rollout.jsonl"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	created, err := archive.CreateLocal(t.Context(), lifecycle.OperationPlan{
		OperationID: "settings-test", Root: source, ConversationID: "synthetic-test", Status: lifecycle.PlanPrepared,
		Assets: []lifecycle.AssetFingerprint{{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result, err := server.remoteArchiveProvider().UploadDirectory(t.Context(), created.Directory, "settings-batch"); err != nil || !result.Verified {
		t.Fatalf("模拟上传失败: %+v %v", result, err)
	}
	list := githubSettingsRequest(t, handler, "GET", "/api/v1/archives", nil, 200)
	if !strings.Contains(list.Body.String(), "settings-batch") {
		t.Fatal("上传的归档未出现在清单中")
	}
	destination := filepath.Join(options.RemoteRecoveryRoot, "download")
	preview := githubSettingsRequest(t, handler, "POST", "/api/v1/archives/settings-batch/download/preview", map[string]string{"destination": destination}, 200)
	var plan struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	githubSettingsRequest(t, handler, "POST", "/api/v1/archives/settings-batch/download/execute", map[string]string{"operation_id": plan.OperationID}, 200)
	if _, err := archive.VerifyLocal(t.Context(), filepath.Join(destination, "settings-batch")); err != nil {
		t.Fatal(err)
	}
	// 重启后保持连接、私有仓库选择与凭证，不需要重新填写设置。
	restarted := NewServerWithOptions(source, nil, options)
	t.Cleanup(restarted.Close)
	if err := restarted.SetGitHubSettings(path, GitHubSettings{}, tokens, "test", selection); err != nil {
		t.Fatal(err)
	}
	githubSettingsRequest(t, restarted.Handler(), "GET", "/api/v1/archives", nil, 200)
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "runtime-token") || strings.Contains(string(data), "access_token") {
		t.Fatal("配置文件中不应存在访问令牌")
	}
	// 切换目标后禁止复用旧仓库的删除预览。
	deletion := githubSettingsRequest(t, handler, "POST", "/api/v1/archives/settings-batch/delete/preview", nil, 200)
	_ = json.Unmarshal(deletion.Body.Bytes(), &plan)
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/archive-target", map[string]string{"owner": "owner", "repository": "other"}, 200)
	githubSettingsRequest(t, handler, "POST", "/api/v1/archives/settings-batch/delete/execute", map[string]string{"operation_id": plan.OperationID, "confirm_archive_id": "settings-batch"}, 404)
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/archive-target", map[string]string{"owner": "owner", "repository": "repo"}, 200)
	deletion = githubSettingsRequest(t, handler, "POST", "/api/v1/archives/settings-batch/delete/preview", nil, 200)
	_ = json.Unmarshal(deletion.Body.Bytes(), &plan)
	githubSettingsRequest(t, handler, "POST", "/api/v1/archives/settings-batch/delete/execute", map[string]string{"operation_id": plan.OperationID, "confirm_archive_id": "wrong"}, 409)
	githubSettingsRequest(t, handler, "POST", "/api/v1/archives/settings-batch/delete/execute", map[string]string{"operation_id": plan.OperationID, "confirm_archive_id": "settings-batch"}, 200)
	if content, err := os.ReadFile(filepath.Join(source, "rollout.jsonl")); err != nil || !bytes.Equal(content, payload) {
		t.Fatal("远端归档管理不应修改本地源样本")
	}
	// 切换 OAuth App 后不能复用原 App 的凭证。
	settings.ClientID = "different-client"
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/settings", settings, 200)
	status := githubSettingsRequest(t, handler, "GET", "/api/v1/github/auth/status", nil, 200)
	if strings.Contains(status.Body.String(), `"credential_available":true`) {
		t.Fatal("不同 OAuth App 不应共享凭证")
	}
}

func TestGitHubSettingsRejectUnsafeChangesAndKeepPreviousState(t *testing.T) {
	server := NewServer(t.TempDir())
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "github.json")
	if err := server.SetGitHubSettings(path, GitHubSettings{}, archive.NewMemoryGitHubTokenStore(), "test", nil); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/settings", GitHubSettings{ClientID: "valid-client"}, 200)
	for _, input := range []any{
		GitHubSettings{ClientID: "ghp_secret"}, GitHubSettings{APIBaseURL: "http://github.example"},
		GitHubSettings{APIBaseURL: "https://name:secret@github.example"}, GitHubSettings{ProxyURL: "http://name:secret@localhost:1080"},
		GitHubSettings{ProxyURL: "ftp://localhost:1080"}, GitHubSettings{ArchiveRoot: "../outside"}, GitHubSettings{ChunkMiB: 51},
		map[string]string{"client_id": "valid", "access_token": "must-not-save"},
	} {
		githubSettingsRequest(t, handler, "PUT", "/api/v1/github/settings", input, 400)
	}
	server.remoteConfigMu.RLock()
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/settings", GitHubSettings{}, 409)
	server.remoteConfigMu.RUnlock()
	if server.githubSettings.settings.ClientID != "valid-client" {
		t.Fatal("无效配置不应覆盖当前配置")
	}
	server.githubSettings.path = t.TempDir()
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/settings", GitHubSettings{ClientID: "other"}, 500)
	if server.githubSettings.settings.ClientID != "valid-client" {
		t.Fatal("保存失败不应破坏原运行状态")
	}
}

// 本地模拟服务覆盖设备授权、私有仓库列表和 Contents API；其数据只存于测试内存。
func newSettingsGitHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	files := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		encode := func(value any) { _ = json.NewEncoder(writer).Encode(value) }
		switch request.URL.Path {
		case "/", "/rate_limit":
			if request.Method != "GET" || request.Header.Get("Authorization") != "" {
				t.Error("网络探测只能使用无凭证 GET")
			}
			encode(map[string]any{"ok": true})
			return
		case "/login/device/code":
			encode(map[string]any{"device_code": "device-secret", "user_code": "ABCD-EFGH", "verification_uri": "https://github.com/login/device", "expires_in": 600, "interval": 1})
			return
		case "/login/oauth/access_token":
			encode(map[string]string{"access_token": "runtime-token"})
			return
		}
		if request.Header.Get("Authorization") != "Bearer runtime-token" {
			writer.WriteHeader(401)
			return
		}
		if request.URL.Path == "/user" {
			encode(map[string]string{"login": "owner", "name": "模拟归档用户"})
			return
		}
		if request.URL.Path == "/user/repos" {
			items := make([]map[string]any, 0)
			for _, name := range []string{"repo", "other", "public"} {
				items = append(items, map[string]any{"name": name, "full_name": "owner/" + name, "owner": map[string]string{"login": "owner"}, "private": name != "public", "default_branch": "main"})
			}
			encode(items)
			return
		}
		if request.URL.Path == "/repos/owner/repo" {
			encode(map[string]any{"private": true})
			return
		}
		prefix := "/repos/owner/repo/contents/"
		if !strings.HasPrefix(request.URL.Path, prefix) {
			writer.WriteHeader(404)
			return
		}
		path := strings.TrimPrefix(request.URL.Path, prefix)
		mu.Lock()
		defer mu.Unlock()
		sha := func(content []byte) string {
			h := sha1.New()
			fmt.Fprintf(h, "blob %d\x00", len(content))
			h.Write(content)
			return fmt.Sprintf("%x", h.Sum(nil))
		}
		switch request.Method {
		case "GET":
			if content, ok := files[path]; ok {
				encode(map[string]any{"type": "file", "path": path, "sha": sha(content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content)})
				return
			}
			entries := map[string]string{}
			for name := range files {
				if strings.HasPrefix(name, path+"/") {
					parts := strings.SplitN(strings.TrimPrefix(name, path+"/"), "/", 2)
					kind := "file"
					if len(parts) == 2 {
						kind = "dir"
					}
					entries[path+"/"+parts[0]] = kind
				}
			}
			if len(entries) == 0 {
				writer.WriteHeader(404)
				return
			}
			items := make([]map[string]string, 0, len(entries))
			for name, kind := range entries {
				items = append(items, map[string]string{"path": name, "type": kind})
			}
			encode(items)
		case "PUT", "DELETE":
			var input struct{ Content, SHA string }
			if json.NewDecoder(request.Body).Decode(&input) != nil {
				writer.WriteHeader(400)
				return
			}
			if previous, ok := files[path]; ok && input.SHA != sha(previous) {
				writer.WriteHeader(409)
				return
			}
			if request.Method == "DELETE" {
				delete(files, path)
				encode(map[string]any{})
				return
			}
			content, err := base64.StdEncoding.DecodeString(input.Content)
			if err != nil {
				writer.WriteHeader(400)
				return
			}
			files[path] = content
			encode(map[string]any{"content": map[string]string{"sha": sha(content)}})
		default:
			writer.WriteHeader(405)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
