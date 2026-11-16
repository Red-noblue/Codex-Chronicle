// 中文说明：将完整目标快照流式打包为 Zstd 压缩归档，并在隔离目录校验正文、归属行和索引；不恢复到真实 Codex。
package archive

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/durablefs"
	"codex-chronicle.local/internal/lifecycle"
	"github.com/klauspost/compress/zstd"
)

const cloudPayload = "conversation.tar.zst"
const maxCloudSnapshotBytes = 128 << 20

// CloudBackupMetadata 使用现有 v1 传输清单承载独立压缩包，兼容旧归档和其他存储 provider。
type CloudBackupMetadata struct {
	Format          string   `json:"format"`
	ConversationIDs []string `json:"conversation_ids"`
	OriginalBytes   int64    `json:"original_bytes"`
	CompressedBytes int64    `json:"compressed_bytes"`
	SnapshotSHA256  string   `json:"snapshot_sha256"`
}

type cloudSnapshot struct {
	Kind   string   `json:"kind"`
	IDs    []string `json:"ids"`
	Assets []struct {
		Relative string `json:"relative"`
		ID       string `json:"id"`
		Bytes    int64  `json:"bytes"`
		SHA256   string `json:"sha256"`
	} `json:"assets"`
	Databases map[string]struct {
		SchemaSHA256 string `json:"schema_sha256"`
		Rows         []struct {
			Table   string              `json:"table"`
			Columns []string            `json:"columns"`
			Owner   string              `json:"owner"`
			Values  [][]json.RawMessage `json:"values"`
		} `json:"rows"`
	} `json:"databases"`
	Indexes map[string]struct {
		Owner   string `json:"owner"`
		Content string `json:"content"`
		SHA256  string `json:"sha256"`
		Lines   int    `json:"lines"`
	} `json:"indexes"`
}

// CreateCloudBundle 只读取兼容层已导出的目标快照；destination 必须不存在。
func CreateCloudBundle(ctx context.Context, exported, destination, operationID string) (CloudBackupMetadata, error) {
	var metadata CloudBackupMetadata
	if _, err := validateCloudExport(ctx, exported, "", nil); err != nil {
		return metadata, err
	}
	files, err := regularArchiveFiles(exported)
	if err != nil {
		return metadata, err
	}
	snapshotBytes, err := os.ReadFile(filepath.Join(exported, "snapshot.json"))
	if err != nil {
		return metadata, err
	}
	var snapshot cloudSnapshot
	if err := json.Unmarshal(snapshotBytes, &snapshot); err != nil {
		return metadata, err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return metadata, err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(destination)
		}
	}()
	if err := os.Mkdir(filepath.Join(destination, "payload"), 0700); err != nil {
		return metadata, err
	}
	output, err := os.OpenFile(filepath.Join(destination, "payload", cloudPayload), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return metadata, err
	}
	defer output.Close()
	hash := sha256.New()
	encoder, err := zstd.NewWriter(io.MultiWriter(output, hash), zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return metadata, err
	}
	defer encoder.Close()
	tarWriter := tar.NewWriter(encoder)
	defer tarWriter.Close()
	var original int64
	for _, relative := range files {
		if err := ctx.Err(); err != nil {
			tarWriter.Close()
			encoder.Close()
			return metadata, err
		}
		file, err := os.Open(filepath.Join(exported, filepath.FromSlash(relative)))
		if err != nil {
			return metadata, err
		}
		info, err := file.Stat()
		if err == nil {
			err = tarWriter.WriteHeader(&tar.Header{Name: relative, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg})
		}
		if err == nil {
			_, err = io.Copy(tarWriter, cloudContextReader{ctx, file})
			original += info.Size()
		}
		file.Close()
		if err != nil {
			tarWriter.Close()
			encoder.Close()
			return metadata, err
		}
	}
	if err := tarWriter.Close(); err != nil {
		encoder.Close()
		return metadata, err
	}
	if err := encoder.Close(); err != nil {
		return metadata, err
	}
	if err := output.Sync(); err != nil {
		return metadata, err
	}
	info, err := output.Stat()
	if err != nil {
		return metadata, err
	}
	metadata = CloudBackupMetadata{Format: "tar+zstd-v1", ConversationIDs: snapshot.IDs, OriginalBytes: original, CompressedBytes: info.Size(), SnapshotSHA256: sha256Hex(snapshotBytes)}
	manifest := Manifest{Version: 1, OperationID: operationID, CreatedAt: time.Now().UTC(), ConversationID: snapshot.IDs[0], CloudBackup: &metadata,
		Assets: []lifecycle.AssetFingerprint{{RelativePath: cloudPayload, Kind: domain.AssetKindJSONL, SizeBytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))}}}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return metadata, err
	}
	if err := os.WriteFile(filepath.Join(destination, manifestName), data, 0600); err != nil {
		return metadata, err
	}
	file, err := os.OpenFile(filepath.Join(destination, manifestName), os.O_WRONLY, 0)
	if err != nil {
		return metadata, err
	}
	err = file.Sync()
	file.Close()
	if err != nil {
		return metadata, err
	}
	success = true
	return metadata, nil
}

// EncryptCloudBundle 在尚未发布的压缩包上复用分块 AES-GCM；压缩先于加密，密码不落盘。
func EncryptCloudBundle(ctx context.Context, directory, password string) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("加密归档必须提供密码")
	}
	manifest, err := VerifyLocal(ctx, directory)
	if err != nil || manifest.CloudBackup == nil {
		return fmt.Errorf("压缩归档未通过加密前校验: %v", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	manifest.Encrypted = true
	manifest.Encryption = &EncryptionMetadata{KDF: encryptionKDF, Cipher: encryptionCipher, Salt: hex.EncodeToString(salt), Iterations: encryptionIterations}
	key := deriveKey([]byte(password), salt, encryptionIterations, 32)
	for _, asset := range manifest.Assets {
		source := filepath.Join(directory, "payload", asset.RelativePath)
		temporary := source + ".encrypted"
		if err := copyAndEncryptAndVerify(ctx, source, temporary, asset, key); err != nil {
			return err
		}
		if err := os.Rename(temporary, source); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, manifestName), data, 0600); err != nil {
		return err
	}
	_, err = VerifyLocalEncrypted(ctx, directory, password)
	return err
}

// CopyCloudBundle 在隔离目录复制并校验归档，已有完全相同的包可复用，其他内容拒绝覆盖。
func CopyCloudBundle(ctx context.Context, source, destination, password string) error {
	manifest, err := verifyLocal(ctx, source, password)
	if err != nil {
		return err
	}
	if pathHasCodexCloud(destination) || pathsOverlapCloud(source, destination) {
		return fmt.Errorf("归档目的地与源冲突")
	}
	if _, err := os.Lstat(destination); err == nil {
		existing, err := verifyLocal(ctx, destination, password)
		left, _ := json.Marshal(manifest)
		right, _ := json.Marshal(existing)
		if err != nil || string(left) != string(right) {
			return fmt.Errorf("本地归档已存在且内容不同，未覆盖")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ensureRestoreParent(destination); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".archive-copy-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	files, err := regularArchiveFiles(source)
	if err != nil {
		return err
	}
	for _, relative := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		from := filepath.Join(source, filepath.FromSlash(relative))
		to := filepath.Join(staging, filepath.FromSlash(relative))
		if err := ensureParent(to); err != nil {
			return err
		}
		hash, err := hashFile(ctx, from)
		if err != nil {
			return err
		}
		if err := copyAndVerify(ctx, from, to, lifecycle.AssetFingerprint{RelativePath: relative, SHA256: hash}); err != nil {
			return err
		}
		file, err := os.Open(to)
		if err != nil {
			return err
		}
		err = file.Sync()
		file.Close()
		if err != nil {
			return err
		}
	}
	if _, err := verifyLocal(ctx, staging, password); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	return durablefs.SyncDirectory(filepath.Dir(destination))
}

type cloudContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r cloudContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

// ExtractCloudBundle 拒绝链接、越界、重复路径及解压超限；验证失败清理本次隔离输出。
func ExtractCloudBundle(ctx context.Context, directory, destination string, maxBytes int64, passwords ...string) (CloudBackupMetadata, error) {
	password := ""
	if len(passwords) > 0 {
		password = passwords[0]
	}
	manifest, err := verifyLocal(ctx, directory, password)
	if err != nil {
		return CloudBackupMetadata{}, err
	}
	m := manifest.CloudBackup
	if m == nil || m.Format != "tar+zstd-v1" || m.OriginalBytes < 0 || m.OriginalBytes > maxBytes || len(m.ConversationIDs) == 0 || len(m.ConversationIDs) > 100 || !validSHA256(m.SnapshotSHA256) {
		return CloudBackupMetadata{}, fmt.Errorf("云备份格式或解压预算无效")
	}
	if pathsOverlapCloud(directory, destination) || pathHasCodexCloud(destination) {
		return *m, fmt.Errorf("隔离输出不能位于 .codex 或归档包内部")
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return *m, err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(destination)
		}
	}()
	payloadPath := filepath.Join(directory, "payload", cloudPayload)
	if manifest.Encrypted {
		// 临时明文仅存在本次隔离解压目录内，成功和失败均清理。
		payloadPath = filepath.Join(destination, ".compressed-plaintext")
		if err := decryptAndVerifyToFile(ctx, filepath.Join(directory, "payload", cloudPayload), payloadPath, manifest.Assets[0], manifest, password); err != nil {
			return *m, err
		}
		defer os.Remove(payloadPath)
	}
	input, err := os.Open(payloadPath)
	if err != nil {
		return *m, err
	}
	defer input.Close()
	decoder, err := zstd.NewReader(input, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderMaxWindow(32<<20))
	if err != nil {
		return *m, err
	}
	defer decoder.Close()
	reader := tar.NewReader(cloudContextReader{ctx, decoder})
	seen := map[string]bool{}
	var total int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return *m, err
		}
		relative, err := safeRemoteRelative(header.Name)
		if err != nil || relative != header.Name || seen[relative] || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > m.OriginalBytes-total {
			return *m, fmt.Errorf("压缩包含不安全或超限条目")
		}
		seen[relative] = true
		if len(seen) > 10000 {
			return *m, fmt.Errorf("压缩包条目过多")
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return *m, err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return *m, err
		}
		_, err = io.Copy(file, reader)
		syncErr := file.Sync()
		file.Close()
		if err != nil {
			return *m, err
		}
		if syncErr != nil {
			return *m, syncErr
		}
		total += header.Size
	}
	if total != m.OriginalBytes {
		return *m, fmt.Errorf("解压总大小不匹配")
	}
	if manifest.Encrypted {
		input.Close()
		if err := os.Remove(payloadPath); err != nil {
			return *m, err
		}
	}
	if _, err := validateCloudExport(ctx, destination, m.SnapshotSHA256, m.ConversationIDs); err != nil {
		return *m, err
	}
	success = true
	return *m, nil
}

// VerifyCloudRecovery 复核中断前已解压完成的隔离目录，不覆盖已有文件。
func VerifyCloudRecovery(ctx context.Context, directory string, metadata CloudBackupMetadata) error {
	_, err := validateCloudExport(ctx, directory, metadata.SnapshotSHA256, metadata.ConversationIDs)
	return err
}

func validateCloudExport(ctx context.Context, directory, expected string, ids []string) (cloudSnapshot, error) {
	var snapshot cloudSnapshot
	info, err := os.Lstat(filepath.Join(directory, "snapshot.json"))
	if err != nil {
		return snapshot, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCloudSnapshotBytes {
		return snapshot, fmt.Errorf("目标行快照过大或不是普通文件")
	}
	data, err := os.ReadFile(filepath.Join(directory, "snapshot.json"))
	if err != nil {
		return snapshot, err
	}
	if expected != "" && sha256Hex(data) != expected {
		return snapshot, fmt.Errorf("目标行快照 SHA256 不匹配")
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return snapshot, err
	}
	if snapshot.Kind != "chronicle-cloud-snapshot-v1" || len(snapshot.IDs) == 0 || len(snapshot.IDs) > 100 || ids != nil && !slices.Equal(ids, snapshot.IDs) {
		return snapshot, fmt.Errorf("快照身份不匹配")
	}
	allowed := map[string]bool{}
	for _, id := range snapshot.IDs {
		if allowed[id] || !validCloudID(id) {
			return snapshot, fmt.Errorf("快照会话 ID 无效")
		}
		allowed[id] = true
	}
	paths := map[string]bool{"snapshot.json": true}
	for _, asset := range snapshot.Assets {
		relative, err := safeRemoteRelative(asset.Relative)
		if err != nil || relative != asset.Relative || !allowed[asset.ID] || asset.Bytes < 0 || !validSHA256(asset.SHA256) {
			return snapshot, fmt.Errorf("目标资产身份无效")
		}
		name := "files/" + relative
		if paths[name] {
			return snapshot, fmt.Errorf("目标资产重复")
		}
		paths[name] = true
		file := filepath.Join(directory, filepath.FromSlash(name))
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Size() != asset.Bytes {
			return snapshot, fmt.Errorf("目标文件缺失或大小不匹配: %s", relative)
		}
		digest, err := hashFile(ctx, file)
		if err != nil {
			return snapshot, err
		}
		if digest != asset.SHA256 {
			return snapshot, fmt.Errorf("目标文件 SHA256 不匹配: %s", relative)
		}
	}
	if len(snapshot.Assets) == 0 || len(snapshot.Databases) == 0 {
		return snapshot, fmt.Errorf("备份缺少正文或数据库行快照")
	}
	for name, database := range snapshot.Databases {
		if filepath.Base(name) != name || !strings.HasSuffix(name, ".sqlite") || !validSHA256(database.SchemaSHA256) {
			return snapshot, fmt.Errorf("数据库身份无效")
		}
		for _, group := range database.Rows {
			owner := slices.Index(group.Columns, group.Owner)
			if owner < 0 {
				return snapshot, fmt.Errorf("数据库归属列缺失")
			}
			for _, row := range group.Values {
				var id string
				if len(row) != len(group.Columns) || json.Unmarshal(row[owner], &id) != nil || !allowed[id] {
					return snapshot, fmt.Errorf("备份包含非目标数据库记录")
				}
			}
		}
	}
	for name, index := range snapshot.Indexes {
		if name != "history.jsonl" && name != "session_index.jsonl" {
			return snapshot, fmt.Errorf("未知索引快照")
		}
		content, err := base64.StdEncoding.DecodeString(index.Content)
		if err != nil || sha256Hex(content) != index.SHA256 {
			return snapshot, fmt.Errorf("索引内容摘要不匹配")
		}
		lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
		if len(content) == 0 {
			lines = nil
		}
		if len(lines) != index.Lines {
			return snapshot, fmt.Errorf("索引行数不匹配")
		}
		for _, line := range lines {
			var record map[string]json.RawMessage
			var id string
			if json.Unmarshal([]byte(line), &record) != nil || json.Unmarshal(record[index.Owner], &id) != nil || !allowed[id] {
				return snapshot, fmt.Errorf("索引包含非目标记录")
			}
		}
	}
	files, err := regularArchiveFiles(directory)
	if err != nil {
		return snapshot, err
	}
	for _, file := range files {
		if !paths[file] {
			return snapshot, fmt.Errorf("快照含未声明文件: %s", file)
		}
	}
	return snapshot, nil
}

func validCloudID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
func pathHasCodexCloud(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".codex" {
			return true
		}
	}
	return false
}
func pathsOverlapCloud(a, b string) bool {
	a, _ = filepath.Abs(a)
	b, _ = filepath.Abs(b)
	return a == b || pathWithin(a, b) || pathWithin(b, a)
}
