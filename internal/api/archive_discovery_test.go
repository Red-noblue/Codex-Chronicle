// 中文说明：使用隔离归档和内存远端验证清单重建、断网重启、仓库隔离、缓存失效和副本身份，不访问真实 Codex。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-chronicle.local/internal/archive"
)

type discoveryRemote struct {
	apiGitHubArchiveSelector
	mu        sync.Mutex
	entries   []archive.GitHubArchiveSummary
	manifests map[string]archive.Manifest
	err       error
	calls     int
	reads     int
	verified  int
}

func (p *discoveryRemote) ListArchives(context.Context) ([]archive.GitHubArchiveSummary, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.entries, p.err
}

func (p *discoveryRemote) ReadArchiveManifest(_ context.Context, id, _ string) (archive.Manifest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	manifest, ok := p.manifests[id]
	if !ok {
		return archive.Manifest{}, fmt.Errorf("没有源清单")
	}
	return manifest, nil
}

func (p *discoveryRemote) VerifyArchive(context.Context, string) (archive.RemoteArchiveResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verified++
	return archive.RemoteArchiveResult{}, fmt.Errorf("发现清单不应下载全部分片")
}

type discoveryResponse struct {
	Items         []archiveCatalogItem `json:"items"`
	RemoteStale   bool                 `json:"remote_stale"`
	RemoteError   string               `json:"remote_error"`
	RemoteTarget  string               `json:"remote_target"`
	RemoteChecked time.Time            `json:"remote_checked_at"`
	Errors        []string             `json:"catalog_errors"`
}

func discoveryFixture(t *testing.T) (*Server, *discoveryRemote, []localArchiveCatalogItem) {
	t.Helper()
	root, archiveRoot := t.TempDir(), t.TempDir()
	provider := &discoveryRemote{apiGitHubArchiveSelector: apiGitHubArchiveSelector{
		target: archive.GitHubRepositorySummary{Owner: "owner", Name: "archive", FullName: "owner/archive"}, configured: true,
	}, manifests: map[string]archive.Manifest{}}
	server := NewServerWithOptions(root, nil, &LifecycleOptions{
		ArchiveRoot: archiveRoot, JournalRoot: filepath.Join(t.TempDir(), "lifecycle"), RemoteArchive: provider,
	})
	t.Cleanup(server.Close)
	_, sample := archiveCenterLocalTask(t, root, t.TempDir())
	for i := range 3 {
		id := fmt.Sprintf("backup-%024x", i+1)
		destination := filepath.Join(archiveRoot, id)
		if err := copyAPIArchiveTree(sample, destination); err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(destination, "manifest.json")
		data, _ := os.ReadFile(manifestPath)
		var manifest archive.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.OperationID = id
		manifest.CreatedAt = time.Date(2026, 11, i+1, 0, 0, 0, 0, time.UTC)
		data, _ = json.Marshal(manifest)
		if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if i != 1 {
			provider.entries = append(provider.entries, archive.GitHubArchiveSummary{ArchiveID: id, CreatedAt: manifest.CreatedAt, SourceManifestSHA256: fmt.Sprintf("%x", sha256.Sum256(data))})
			provider.manifests[id] = manifest
		}
	}
	locals, issues := discoverLocalArchives(t.Context(), archiveRoot)
	if len(locals) != 3 || len(issues) != 0 {
		t.Fatalf("样本不完整: %+v %v", locals, issues)
	}
	return server, provider, locals
}

func readDiscovery(t *testing.T, server *Server, force bool) discoveryResponse {
	t.Helper()
	path := "/api/v1/archive-center"
	if force {
		path += "?refresh=1"
	}
	data := archiveCenterRequest(t, server.Handler(), http.MethodGet, path, nil, http.StatusOK)
	var result discoveryResponse
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestArchiveDiscoveryRebuildsThreeLocalTwoRemoteWithoutTasks(t *testing.T) {
	server, provider, locals := discoveryFixture(t)
	result := readDiscovery(t, server, false)
	paired := 0
	for _, item := range result.Items {
		if item.Manageable || !item.Discovered || !item.LocalAvailable || len(item.Task.SourceIDs) != 1 || item.Task.Metadata == nil {
			t.Fatalf("无历史记录的归档不应被授权执行或丢失元数据: %+v", item)
		}
		if item.RemoteAvailable {
			paired++
		}
		archiveCenterRequest(t, server.Handler(), http.MethodPost, "/api/v1/archive-center/"+item.Task.ID+"/preview", map[string]string{"action": "restore"}, http.StatusConflict)
	}
	if len(result.Items) != 3 || paired != 2 || provider.calls != 1 || provider.reads != 2 || provider.verified != 0 || result.RemoteStale {
		t.Fatalf("清单重建结果不正确: %+v calls=%d reads=%d", result, provider.calls, provider.reads)
	}
	after, issues := discoverLocalArchives(t.Context(), server.lifecycle.ArchiveRoot)
	for i := range locals {
		if locals[i].ManifestSHA256 != after[i].ManifestSHA256 {
			t.Fatal("发现清单修改了归档 manifest")
		}
	}
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	readDiscovery(t, server, false)
	if provider.calls != 1 {
		t.Fatal("页面状态轮询重复请求远端清单")
	}
	readDiscovery(t, server, true)
	if provider.calls != 2 || provider.reads != 2 {
		t.Fatal("手动刷新没有重新列出远端，或重复读取了内容相同的源清单")
	}
}

func TestArchiveDiscoveryKeepsCloudCatalogOfflineAfterRestartAndIsolatesRepositories(t *testing.T) {
	server, provider, _ := discoveryFixture(t)
	readDiscovery(t, server, true)
	provider.err = fmt.Errorf("模拟断网")
	// 新建 Server 模拟重启，内存清单为空，只能读取独立磁盘缓存。
	restarted := NewServerWithOptions(server.currentRoot(), nil, server.lifecycle)
	t.Cleanup(restarted.Close)
	result := readDiscovery(t, restarted, false)
	remote := 0
	for _, item := range result.Items {
		if item.RemoteAvailable {
			remote++
			if !item.RemoteStale {
				t.Fatal("断网后把历史清单误报为本次核验成功")
			}
		}
	}
	if remote != 2 || !result.RemoteStale || result.RemoteChecked.IsZero() || len(result.Items) != 3 {
		t.Fatalf("断网重启后没有保留 3 本地 2 云端: %+v", result)
	}
	provider.target = archive.GitHubRepositorySummary{FullName: "owner/other"}
	other := readDiscovery(t, restarted, false)
	for _, item := range other.Items {
		if item.RemoteAvailable {
			t.Fatal("仓库切换后显示了上一个仓库的副本")
		}
	}
	if !other.RemoteChecked.IsZero() || other.RemoteTarget != "owner/other" {
		t.Fatal("新仓库借用了旧核验时间或身份")
	}
	provider.target = archive.GitHubRepositorySummary{FullName: "owner/archive"}
	provider.err, provider.entries = nil, nil
	empty := readDiscovery(t, restarted, true)
	for _, item := range empty.Items {
		if item.RemoteAvailable || item.RemoteStale {
			t.Fatal("远端成功返回空清单后，仍保留已经消失的云端副本")
		}
	}
}

func TestArchiveDiscoveryPreservesHistoryAndSeparatesChangedManifests(t *testing.T) {
	server, provider, locals := discoveryFixture(t)
	local := locals[0]
	restoredAt := time.Now().UTC()
	task := cloudTask{ID: local.OperationID, ArchiveID: local.OperationID, SourceRoot: server.currentRoot(),
		State: "completed", SourceIDs: local.Compression.ConversationIDs, CreatedAt: local.CreatedAt,
		PreviewDigest: local.Compression.SnapshotSHA256, LocalCopy: true, LocalPath: local.Path,
		Target: "owner/archive", TargetIdentity: "|owner/archive", RemoteCopy: true,
		RestoreStatus: "restored", RestoredAt: &restoredAt, Titles: []string{"保留的用户标题"}}
	server.cloud = &cloudBackupState{tasks: map[string]cloudTask{task.ID: task}}
	result := readDiscovery(t, server, false)
	if len(result.Items) != 3 {
		t.Fatalf("同一归档的任务和发现结果重复显示: %+v", result)
	}
	for _, item := range result.Items {
		if item.Task.ID == task.ID && (!item.Manageable || item.Task.RestoreStatus != "restored" || item.Task.Titles[0] != "保留的用户标题") {
			t.Fatal("合并副本丢失历史恢复记录或执行能力")
		}
	}
	provider.entries[0].SourceManifestSHA256 = strings.Repeat("f", 64)
	manifest := provider.manifests[task.ID]
	manifest.Encrypted = !manifest.Encrypted
	provider.manifests[task.ID] = manifest
	changed := readDiscovery(t, server, true)
	if len(changed.Items) != 4 {
		t.Fatalf("同名不同清单的副本未被分开: %+v", changed)
	}
	for _, item := range changed.Items {
		if item.Task.ID == task.ID && item.RemoteAvailable {
			t.Fatal("把清单不同的同名归档误算为双副本")
		}
	}
}

func TestArchiveDiscoveryReportsBrokenManifestWithoutHidingGoodArchives(t *testing.T) {
	server, _, _ := discoveryFixture(t)
	bad := filepath.Join(server.lifecycle.ArchiveRoot, "broken")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "manifest.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(server.lifecycle.ArchiveRoot, ".remote-restore-staging", "preview"), 0o700); err != nil {
		t.Fatal(err)
	}
	result := readDiscovery(t, server, false)
	if len(result.Items) != 3 || len(result.Errors) == 0 {
		t.Fatalf("坏清单使正常归档消失，或异常被隐瞒: %+v", result)
	}
}

func TestArchiveDiscoveryRefreshesAfterOperationAndRejectsBrokenCache(t *testing.T) {
	server, provider, _ := discoveryFixture(t)
	first := readDiscovery(t, server, false)
	server.cloud = &cloudBackupState{tasks: map[string]cloudTask{"changed": {
		ID: "changed", SourceRoot: server.currentRoot(), State: "completed", ManagementState: "completed", UpdatedAt: time.Now().UTC(),
	}}}
	readDiscovery(t, server, false)
	if provider.calls != 2 {
		t.Fatal("操作完成后仍沿用操作前的缓存")
	}
	identity := server.archiveCatalogIdentity("owner/archive", "|owner/archive")
	path := server.archiveCatalogCachePath(identity)
	if data, err := os.ReadFile(path); err != nil || strings.Contains(string(data), "runtime-token") || strings.Contains(string(data), "完整会话测试") {
		t.Fatal("独立清单缓存缺失或包含凭证、正文", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := NewServerWithOptions(server.currentRoot(), nil, server.lifecycle)
	t.Cleanup(restarted.Close)
	result := readDiscovery(t, restarted, false)
	if len(result.Items) != len(first.Items) || len(result.Errors) == 0 {
		t.Fatalf("缓存损坏导致实际归档消失或被隐瞒: %+v", result)
	}
}

func TestArchiveDiscoveryCredentialProviderFindsRemoteWithoutTask(t *testing.T) {
	server, id, relative, original := cloudAPIFixture(t)
	preview := inspectCloudAPI(t, server, id)
	response := githubSettingsRequest(t, server.Handler(), http.MethodPost, "/api/v1/backups/start", map[string]any{"preview_id": preview.ID, "confirm_upload": true}, http.StatusAccepted)
	var task cloudTask
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	finished := awaitCloud(t, server, task.ID, func(task cloudTask) bool { return task.State == "completed" || task.State == "failed" })
	if finished.State != "completed" {
		t.Fatalf("隔离远端上传未完成: %+v", finished)
	}
	server.cloud.mu.Lock()
	delete(server.cloud.tasks, task.ID)
	server.cloud.mu.Unlock()
	result := readDiscovery(t, server, true)
	if len(result.Items) != 1 || !result.Items[0].Discovered || result.Items[0].Manageable || !result.Items[0].RemoteAvailable || result.Items[0].Task.Metadata == nil {
		t.Fatalf("带凭证的真实 provider 未从清单找回元数据: %+v", result)
	}
	if data, err := os.ReadFile(filepath.Join(server.currentRoot(), relative)); err != nil || string(data) != string(original) {
		t.Fatal("重建清单修改了隔离样本源正文", err)
	}
}

func TestArchiveDiscoveryConcurrentPollingCoalescesRemoteReads(t *testing.T) {
	server, provider, _ := discoveryFixture(t)
	var done sync.WaitGroup
	for range 8 {
		done.Add(1)
		go func() {
			defer done.Done()
			result := readDiscovery(t, server, false)
			if len(result.Items) != 3 {
				t.Error("并发请求缺少归档")
			}
		}()
	}
	done.Wait()
	if provider.calls != 1 || provider.reads != 2 {
		t.Fatalf("并发页面重复请求远端: calls=%d reads=%d", provider.calls, provider.reads)
	}
}
