// 中文说明：验证归档发现只读源清单、拒绝扫描期间变化和清单损坏，并不会下载正文分片。
package archive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubReadArchiveManifestLeavesPayloadUnread(t *testing.T) {
	exported, _, _ := cloudExportFixture(t)
	local := filepath.Join(t.TempDir(), "package")
	metadata, err := CreateCloudBundle(t.Context(), exported, local, "manifest-discovery")
	if err != nil {
		t.Fatal(err)
	}
	remote := newGitHubContentsTestServer()
	defer remote.Close()
	provider, err := NewGitHubProvider(GitHubConfig{
		BaseURL: remote.URL(), Owner: "owner", Repository: "chronicle", Token: "runtime-token", ChunkBytes: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UploadDirectory(t.Context(), local, "manifest-discovery"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(local, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	sourceHash := sha256Hex(data)
	var transport GitHubRemoteManifest
	remote.mu.Lock()
	if err := json.Unmarshal(remote.files[provider.remotePath("manifest-discovery", "remote-manifest.json")], &transport); err != nil {
		remote.mu.Unlock()
		t.Fatal(err)
	}
	// 删除服务端所有正文分片。发现清单仍应成功，完整校验则必须失败。
	for _, file := range transport.Files {
		if file.RelativePath != manifestName {
			for _, chunk := range file.Chunks {
				delete(remote.files, chunk.Path)
			}
		}
	}
	puts := remote.puts
	remote.mu.Unlock()
	manifest, err := provider.ReadArchiveManifest(t.Context(), "manifest-discovery", sourceHash)
	if err != nil || manifest.CloudBackup == nil || manifest.CloudBackup.SnapshotSHA256 != metadata.SnapshotSHA256 {
		t.Fatalf("只读源清单失败: %+v %v", manifest, err)
	}
	if _, err := provider.VerifyArchive(t.Context(), "manifest-discovery"); err == nil {
		t.Fatal("正文缺失却通过了完整校验")
	}
	if _, err := provider.ReadArchiveManifest(t.Context(), "manifest-discovery", strings.Repeat("f", 64)); err == nil {
		t.Fatal("清单在扫描期间变化后仍被接受")
	}
	remote.mu.Lock()
	for _, file := range transport.Files {
		if file.RelativePath == manifestName {
			remote.files[file.Chunks[0].Path] = []byte("损坏的源清单分片")
		}
	}
	remote.mu.Unlock()
	if _, err := provider.ReadArchiveManifest(t.Context(), "manifest-discovery", sourceHash); err == nil {
		t.Fatal("源清单分片损坏后仍被接受")
	}
	if remote.putCount() != puts {
		t.Fatal("发现清单向远端写入了数据")
	}
}
