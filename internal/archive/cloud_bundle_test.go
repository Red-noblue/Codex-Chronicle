// 中文说明：隔离样本验证流式压缩、完整下载、数据库归属过滤与解压安全，不读取真实 .codex。
package archive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cloudExportFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	id := "11111111-2222-4333-8444-555555555555"
	root := t.TempDir()
	relative := "sessions/rollout-" + id + ".jsonl"
	body := []byte(strings.Repeat("{\"type\":\"event_msg\",\"payload\":{\"text\":\"完整会话压缩测试\"}}\n", 30000))
	if err := os.MkdirAll(filepath.Join(root, "files", "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "files", relative), body, 0600); err != nil {
		t.Fatal(err)
	}
	index := []byte("{\"id\":\"" + id + "\"}\n")
	snapshot := map[string]any{"kind": "chronicle-cloud-snapshot-v1", "ids": []string{id}, "requested_ids": []string{id},
		"assets":    []any{map[string]any{"relative": relative, "id": id, "bytes": len(body), "sha256": sha256Hex(body)}},
		"databases": map[string]any{"state_5.sqlite": map[string]any{"schema_sha256": strings.Repeat("a", 64), "rows": []any{map[string]any{"table": "threads", "owner": "id", "columns": []string{"id", "title"}, "values": [][]string{{id, "目标标题"}}}}}},
		"indexes":   map[string]any{"session_index.jsonl": map[string]any{"owner": "id", "content": base64.StdEncoding.EncodeToString(index), "sha256": sha256Hex(index), "lines": 1}}}
	data, _ := json.Marshal(snapshot)
	if err := os.WriteFile(filepath.Join(root, "snapshot.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return root, relative, body
}

func TestCloudBundleCompressedGitHubRoundTrip(t *testing.T) {
	exported, relative, body := cloudExportFixture(t)
	local := filepath.Join(t.TempDir(), "package")
	metadata, err := CreateCloudBundle(t.Context(), exported, local, "cloud-test")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.CompressedBytes >= metadata.OriginalBytes/5 {
		t.Fatalf("可压缩正文未显著减小: %+v", metadata)
	}
	legacyTarget := filepath.Join(t.TempDir(), "legacy-restore")
	if _, err := PreviewLocalRestore(t.Context(), local, legacyTarget, ""); err == nil || !strings.Contains(err.Error(), "下载并校验") {
		t.Fatal("云备份被旧入口当作普通会话文件恢复", err)
	}
	if _, err := os.Stat(legacyTarget); !os.IsNotExist(err) {
		t.Fatal("格式不兼容的恢复仍写入了目标", err)
	}
	remote := newGitHubContentsTestServer()
	defer remote.Close()
	provider, err := NewGitHubProvider(GitHubConfig{BaseURL: remote.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", ChunkBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(t.Context(), local, "cloud-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.VerifyArchive(t.Context(), "cloud-test"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := provider.DownloadArchive(t.Context(), "cloud-test", root); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "recovered")
	if _, err := ExtractCloudBundle(t.Context(), filepath.Join(root, "cloud-test"), output, 10<<20); err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(filepath.Join(output, "files", relative))
	if err != nil || string(result) != string(body) {
		t.Fatal("恢复正文不同", err)
	}
	if _, err := os.Stat(filepath.Join(output, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractCloudBundle(t.Context(), local, output, 10<<20); err == nil {
		t.Fatal("不应覆盖已有目录")
	}
}

func TestEncryptedCloudBundleCopyAndExtract(t *testing.T) {
	exported, relative, body := cloudExportFixture(t)
	local := filepath.Join(t.TempDir(), "package")
	if _,err := CreateCloudBundle(t.Context(),exported,local,"encrypted-test"); err != nil { t.Fatal(err) }
	if err := EncryptCloudBundle(t.Context(),local,"测试密码"); err != nil { t.Fatal(err) }
	if _,err := VerifyLocalEncrypted(t.Context(),local,"wrong"); err == nil { t.Fatal("错误密码通过校验") }
	copy := filepath.Join(t.TempDir(),"copy")
	if err := CopyCloudBundle(t.Context(),local,copy,"测试密码"); err != nil { t.Fatal(err) }
	if err := CopyCloudBundle(t.Context(),local,copy,"测试密码"); err != nil { t.Fatal("一致副本应可复用",err) }
	output := filepath.Join(t.TempDir(),"recovered")
	if _,err := ExtractCloudBundle(t.Context(),copy,output,16<<20,"测试密码"); err != nil { t.Fatal(err) }
	data,err := os.ReadFile(filepath.Join(output,"files",relative)); if err != nil || string(data)!=string(body) { t.Fatal("加密压缩往返数据不一致",err) }
	if _,err := os.Stat(filepath.Join(output,".compressed-plaintext")); !os.IsNotExist(err) { t.Fatal("遗留临时压缩明文") }
}

func TestCloudBundleRejectsNonTargetRowsTamperAndUnsafeDestination(t *testing.T) {
	exported, _, _ := cloudExportFixture(t)
	data, _ := os.ReadFile(filepath.Join(exported, "snapshot.json"))
	var snapshot map[string]any
	json.Unmarshal(data, &snapshot)
	db := snapshot["databases"].(map[string]any)["state_5.sqlite"].(map[string]any)
	group := db["rows"].([]any)[0].(map[string]any)
	group["values"] = [][]string{{"99999999-2222-4333-8444-555555555555", "非目标记录"}}
	changed, _ := json.Marshal(snapshot)
	os.WriteFile(filepath.Join(exported, "snapshot.json"), changed, 0600)
	if _, err := CreateCloudBundle(t.Context(), exported, filepath.Join(t.TempDir(), "bad"), "bad"); err == nil {
		t.Fatal("非目标数据库行被接受")
	}
	os.WriteFile(filepath.Join(exported, "snapshot.json"), data, 0600)
	local := filepath.Join(t.TempDir(), "package")
	if _, err := CreateCloudBundle(t.Context(), exported, local, "test"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Join(local, "nested"), filepath.Join(t.TempDir(), ".codex", "nested")} {
		if _, err := ExtractCloudBundle(t.Context(), local, target, 10<<20); err == nil {
			t.Fatal("危险目标被接受", target)
		}
	}
	if _, err := ExtractCloudBundle(t.Context(), local, filepath.Join(t.TempDir(), "small"), 10); err == nil {
		t.Fatal("解压预算未生效")
	}
	file := filepath.Join(local, "payload", cloudPayload)
	os.WriteFile(file, []byte("篡改"), 0600)
	if _, err := ExtractCloudBundle(context.Background(), local, filepath.Join(t.TempDir(), "tampered"), 10<<20); err == nil {
		t.Fatal("篡改内容被接受")
	}
}
