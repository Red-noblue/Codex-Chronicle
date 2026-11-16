// 中文说明：以合成源与本机模拟 GitHub 验证备份确认、暂停续传、重启、仓库绑定和隔离下载；不上传真实对话。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/compat/authorizedlive"
)

func cloudAPIFixture(t *testing.T) (*Server, string, string, []byte) {
	t.Helper()
	id := "11111111-2222-4333-8444-555555555555"
	root := t.TempDir()
	relative := "rollout-" + id + ".jsonl"
	body := []byte(fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", id) + strings.Repeat("{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"完整会话测试\"}}\n", 10000))
	if err := os.WriteFile(filepath.Join(root, relative), body, 0600); err != nil {
		t.Fatal(err)
	}
	remote := newSettingsGitHubServer(t)
	options := &LifecycleOptions{AuthorizedLive: &authorizedlive.Config{Python: "synthetic-read-only"}, JournalRoot: filepath.Join(t.TempDir(), "journal"), RemoteRecoveryRoot: t.TempDir()}
	server := NewServerWithOptions(root, nil, options)
	t.Cleanup(server.Close)
	selection, _ := archive.NewFileGitHubArchiveSelectionStore(filepath.Join(t.TempDir(), "selection.json"))
	if err := server.SetGitHubSettings(filepath.Join(t.TempDir(), "settings.json"), GitHubSettings{}, archive.NewMemoryGitHubTokenStore(), "test", selection); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/settings", GitHubSettings{ClientID: "test-client", APIBaseURL: remote.URL, OAuthBaseURL: remote.URL, ChunkMiB: 1}, 200)
	if err := server.githubSettings.auth.SaveCredential(t.Context(), "runtime-token"); err != nil {
		t.Fatal(err)
	}
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/archive-target", map[string]string{"owner": "owner", "repository": "repo"}, 200)
	snapshot := map[string]any{"kind": "chronicle-cloud-snapshot-v1", "ids": []string{id}, "requested_ids": []string{id},
		"assets":    []map[string]any{{"relative": relative, "id": id, "bytes": len(body), "sha256": fmt.Sprintf("%x", sha256.Sum256(body))}},
		"databases": map[string]any{"state_5.sqlite": map[string]any{"schema_sha256": strings.Repeat("a", 64), "rows": []map[string]any{{"table": "threads", "owner": "id", "columns": []string{"id", "title"}, "values": [][]string{{id, "会话测试"}}}}}}, "indexes": map[string]any{}}
	data, _ := json.Marshal(snapshot)
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	state, err := server.cloudBackups()
	if err != nil {
		t.Fatal(err)
	}
	state.run = func(ctx context.Context, action, work string, extra ...string) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if action == "backup-export" {
			if err := os.MkdirAll(filepath.Join(work, "files"), 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(work, "files", relative), body, 0600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(work, "snapshot.json"), data, 0600); err != nil {
				return nil, err
			}
		}
		return json.Marshal(map[string]any{"digest": digest, "ids": []string{id}, "requested_ids": []string{id}, "assets": snapshot["assets"], "source_bytes": len(body), "database_rows": []map[string]any{{"database": "state_5.sqlite", "table": "threads", "rows": 1}}, "index_lines": map[string]int{}, "source_read_only": true})
	}
	return server, compat.StableConversationID(id), relative, body
}

func inspectCloudAPI(t *testing.T, server *Server, id string) cloudPreview {
	t.Helper()
	response := githubSettingsRequest(t, server.Handler(), "POST", "/api/v1/backups/preview", map[string]any{"conversation_ids": []string{id}}, 200)
	var preview cloudPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	return preview
}

func awaitCloud(t *testing.T, server *Server, id string, ready func(cloudTask) bool) cloudTask {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state, err := server.cloudBackups()
		if err != nil {
			t.Fatal(err)
		}
		state.mu.Lock()
		task := state.tasks[id]
		active := state.active
		state.mu.Unlock()
		if active == "" && ready(task) {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("云备份任务超时")
	return cloudTask{}
}

func TestCloudBackupAPIConfirmationUploadDownloadAndSourceUnchanged(t *testing.T) {
	server, id, relative, original := cloudAPIFixture(t)
	handler := server.Handler()
	preview := inspectCloudAPI(t, server, id)
	githubSettingsRequest(t, handler, "POST", "/api/v1/backups/start", map[string]any{"preview_id": preview.ID, "confirm_upload": false}, 409)
	response := githubSettingsRequest(t, handler, "POST", "/api/v1/backups/start", map[string]any{"preview_id": preview.ID, "confirm_upload": true}, 202)
	var task cloudTask
	json.Unmarshal(response.Body.Bytes(), &task)
	finished := awaitCloud(t, server, task.ID, func(task cloudTask) bool { return task.State == "completed" || task.State == "failed" })
	if finished.State != "completed" || finished.CacheRetained || finished.Metadata == nil {
		t.Fatalf("备份失败: %+v", finished)
	}
	if _, err := os.Stat(filepath.Join(server.cloud.root, task.ID)); !os.IsNotExist(err) {
		t.Fatal("成功后仍遗留上传缓存")
	}
	githubSettingsRequest(t, handler, "POST", "/api/v1/backups/start", map[string]any{"preview_id": preview.ID, "confirm_upload": true}, 409)
	githubSettingsRequest(t, handler, "POST", "/api/v1/backups/"+task.ID+"/download", nil, 202)
	downloaded := awaitCloud(t, server, task.ID, func(task cloudTask) bool { return task.DownloadState == "completed" || task.DownloadState == "failed" })
	if downloaded.DownloadState != "completed" || downloaded.RecoveryPath == "" {
		t.Fatalf("下载失败: %+v", downloaded)
	}
	result, err := os.ReadFile(filepath.Join(downloaded.RecoveryPath, "files", relative))
	if err != nil || string(result) != string(original) {
		t.Fatal("隔离恢复正文不一致", err)
	}
	result, err = os.ReadFile(filepath.Join(server.root, relative))
	if err != nil || string(result) != string(original) {
		t.Fatal("备份修改了源正文", err)
	}
	if _, err := os.Stat(filepath.Join(downloaded.RecoveryPath, "snapshot.json")); err != nil {
		t.Fatal("缺少目标数据库行快照")
	}
	githubSettingsRequest(t, handler, "GET", "/api/v1/backups", nil, 200)
}

func TestCloudBackupPauseRestartAndChangedTargetGuard(t *testing.T) {
	server, id, _, _ := cloudAPIFixture(t)
	handler := server.Handler()
	preview := inspectCloudAPI(t, server, id)
	state := server.cloud
	normal := state.run
	entered := make(chan struct{})
	state.run = func(ctx context.Context, action, work string, extra ...string) (json.RawMessage, error) {
		if action == "backup-export" {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return normal(ctx, action, work, extra...)
	}
	response := githubSettingsRequest(t, handler, "POST", "/api/v1/backups/start", map[string]any{"preview_id": preview.ID, "confirm_upload": true}, 202)
	var task cloudTask
	json.Unmarshal(response.Body.Bytes(), &task)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("未进入导出任务")
	}
	githubSettingsRequest(t, handler, "POST", "/api/v1/backups/"+task.ID+"/pause", nil, 202)
	awaitCloud(t, server, task.ID, func(task cloudTask) bool { return task.State == "paused" })
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/archive-target", map[string]string{"owner": "owner", "repository": "other"}, 200)
	githubSettingsRequest(t, handler, "POST", "/api/v1/backups/"+task.ID+"/resume", nil, 409)
	githubSettingsRequest(t, handler, "PUT", "/api/v1/github/archive-target", map[string]string{"owner": "owner", "repository": "repo"}, 200)
	state.run = normal
	// 模拟进程重启前持久记录仍停留在 uploading；新服务不自动上传或删除。
	state.mu.Lock()
	task = state.tasks[task.ID]
	task.State = "uploading"
	state.save(task)
	state.mu.Unlock()
	server.cloudMu.Lock()
	server.cloud = nil
	server.cloudMu.Unlock()
	reloaded, err := server.cloudBackups()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.tasks[task.ID].State != "interrupted" {
		t.Fatal("重启没有将运行态标记中断")
	}
	reloaded.run = normal
	githubSettingsRequest(t, handler, "POST", "/api/v1/backups/"+task.ID+"/resume", nil, 202)
	finished := awaitCloud(t, server, task.ID, func(task cloudTask) bool { return task.State == "completed" || task.State == "failed" })
	if finished.State != "completed" {
		t.Fatalf("续传失败: %+v", finished)
	}
}

func TestCloudBackupNoAdapterRemainsUnavailable(t *testing.T) {
	server := NewServer(t.TempDir())
	t.Cleanup(server.Close)
	result := githubSettingsRequest(t, server.Handler(), http.MethodGet, "/api/v1/backups", nil, 200)
	if !strings.Contains(result.Body.String(), `"available":false`) {
		t.Fatal("缺少兼容层却宣称支持完整备份")
	}
	githubSettingsRequest(t, server.Handler(), http.MethodPost, "/api/v1/backups/start", nil, 409)
}

func TestCloudBackupDownloadRecoversOwnedInterruptedStaging(t *testing.T) {
	server, id, relative, original := cloudAPIFixture(t)
	handler := server.Handler()
	preview := inspectCloudAPI(t, server, id)
	response := githubSettingsRequest(t, handler, "POST", "/api/v1/backups/start", map[string]any{"preview_id": preview.ID, "confirm_upload": true}, 202)
	var task cloudTask
	json.Unmarshal(response.Body.Bytes(), &task)
	task = awaitCloud(t, server, task.ID, func(task cloudTask) bool { return task.State == "completed" || task.State == "failed" })
	if task.State != "completed" {
		t.Fatalf("备份未完成: %+v", task)
	}
	root := filepath.Join(server.lifecycle.RemoteRecoveryRoot, "cloud-"+task.ID)
	if err := prepareExecutionDirectory(root); err != nil {
		t.Fatal(err)
	}
	staging, err := prepareCloudRecoveryStaging(root, task)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟强杀后同时留下的未完成下载与半份解压，均不得被当作完整结果。
	for _, name := range []string{filepath.Join("."+task.ArchiveID+".tmp", "partial"), "recovered/snapshot.json"} {
		path := filepath.Join(staging, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("中断材料"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	task.DownloadState = "running"
	if err := server.cloud.save(task); err != nil {
		t.Fatal(err)
	}
	server.cloudMu.Lock()
	server.cloud = nil
	server.cloudMu.Unlock()
	reloaded, err := server.cloudBackups()
	if err != nil || reloaded.tasks[task.ID].DownloadState != "interrupted" {
		t.Fatal("下载中断记录未恢复", err)
	}
	githubSettingsRequest(t, handler, "POST", "/api/v1/backups/"+task.ID+"/download", nil, 202)
	task = awaitCloud(t, server, task.ID, func(task cloudTask) bool { return task.DownloadState == "completed" || task.DownloadState == "failed" })
	if task.DownloadState != "completed" {
		t.Fatalf("中断后下载失败: %+v", task)
	}
	data, err := os.ReadFile(filepath.Join(task.RecoveryPath, "files", relative))
	if err != nil || string(data) != string(original) {
		t.Fatal("中断后恢复正文不一致", err)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatal("中断临时目录未清理", err)
	}
	// 用户修改了完整输出后，重新下载不得覆盖它。
	modified := []byte("用户手动修改的隔离副本")
	recoveryPath := task.RecoveryPath
	if err := os.WriteFile(filepath.Join(recoveryPath, "files", relative), modified, 0600); err != nil {
		t.Fatal(err)
	}
	githubSettingsRequest(t, handler, "POST", "/api/v1/backups/"+task.ID+"/download", nil, 202)
	task = awaitCloud(t, server, task.ID, func(task cloudTask) bool { return task.DownloadState == "failed" })
	data, err = os.ReadFile(filepath.Join(recoveryPath, "files", relative))
	if err != nil || string(data) != string(modified) || !strings.Contains(task.DownloadError, "未覆盖") || task.RecoveryPath != "" {
		t.Fatal("已有输出被覆盖或没有报告校验失败", err)
	}
}

func TestCloudRecoveryStagingPreservesUnownedContents(t *testing.T) {
	for _, marker := range []string{"", "another-task\n"} {
		t.Run(fmt.Sprintf("marker-%d", len(marker)), func(t *testing.T) {
			root := t.TempDir()
			staging := filepath.Join(root, ".recovery-staging")
			if err := os.Mkdir(staging, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(staging, "user-file"), []byte("保留"), 0600); err != nil {
				t.Fatal(err)
			}
			if marker != "" {
				if err := os.WriteFile(filepath.Join(staging, ".chronicle-owner"), []byte(marker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepareCloudRecoveryStaging(root, cloudTask{ID: "backup-" + strings.Repeat("a", 24), PreviewDigest: strings.Repeat("b", 64)}); err == nil {
				t.Fatal("归属未知的中断目录未阻断")
			}
			if data, err := os.ReadFile(filepath.Join(staging, "user-file")); err != nil || string(data) != "保留" {
				t.Fatal("清理触及非任务文件", err)
			}
		})
	}
}
