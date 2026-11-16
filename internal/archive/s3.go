// 中文说明：实现兼容 AWS S3 和常见 S3-compatible 服务的归档 provider；使用 SigV4、分片 manifest 和独立 staging 目录，不触碰源 .codex。
package archive

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultS3ChunkBytes int64 = 50 * 1024 * 1024

var errS3NotFound = errors.New("S3 归档对象不存在")

// S3Config 是一次运行时 S3 provider 配置；密钥只保存在当前进程内存。
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	PathStyle       bool
	ChunkBytes      int64
	ProxyURL        string
	HTTPClient      *http.Client
}

// S3Provider 使用普通 S3 对象保存归档分片和远端 manifest。
type S3Provider struct {
	config   S3Config
	endpoint *url.URL
	client   *http.Client
}

var _ RemoteArchiveProvider = (*S3Provider)(nil)
var _ RemoteArchiveCatalog = (*S3Provider)(nil)
var _ RemoteArchiveRecovery = (*S3Provider)(nil)
var _ RemoteArchiveDeletion = (*S3Provider)(nil)

// NewS3Provider 创建 AWS S3 或 S3-compatible provider。
// Endpoint 为空时使用 AWS 的区域 endpoint；PathStyle 适合 MinIO、Ceph 和自定义 endpoint。
func NewS3Provider(config S3Config) (*S3Provider, error) {
	if strings.TrimSpace(config.Region) == "" {
		config.Region = "us-east-1"
	}
	if strings.TrimSpace(config.Bucket) == "" || !validS3Bucket(config.Bucket) {
		return nil, fmt.Errorf("S3 bucket 无效")
	}
	if strings.TrimSpace(config.AccessKeyID) == "" || strings.TrimSpace(config.SecretAccessKey) == "" {
		return nil, fmt.Errorf("S3 access key 和 secret key 不能为空")
	}
	if strings.TrimSpace(config.Prefix) != "" {
		prefix, err := safeRemoteRelative(config.Prefix)
		if err != nil {
			return nil, fmt.Errorf("S3 prefix 无效: %w", err)
		}
		config.Prefix = strings.Trim(prefix, "/")
	}
	if config.ChunkBytes == 0 {
		config.ChunkBytes = defaultS3ChunkBytes
	}
	if config.ChunkBytes < 1 || config.ChunkBytes > defaultS3ChunkBytes {
		return nil, fmt.Errorf("S3 分片大小必须在 1 到 50 MiB 之间")
	}
	endpointValue := strings.TrimRight(strings.TrimSpace(config.Endpoint), "/")
	if endpointValue == "" {
		endpointValue = "https://s3." + config.Region + ".amazonaws.com"
	}
	endpoint, err := url.Parse(endpointValue)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("S3 endpoint 无效")
	}
	client, err := newGitHubHTTPClient(config.ProxyURL, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	return &S3Provider{config: config, endpoint: endpoint, client: client}, nil
}

// ProviderInfo 返回 S3 provider 的非敏感能力信息。
func (p *S3Provider) ProviderInfo() RemoteArchiveProviderInfo {
	return RemoteArchiveProviderInfo{ID: "s3", Name: "S3", TargetRequired: false, SupportsListing: true, SupportsRecovery: true, SupportsDelete: true}
}

// UploadDirectory 上传本地归档目录；远端 manifest 最后发布，同一 archive ID 只允许同源清单重试。
func (p *S3Provider) UploadDirectory(ctx context.Context, directory, archiveID string) (RemoteArchiveResult, error) {
	if p == nil {
		return RemoteArchiveResult{}, fmt.Errorf("S3 provider 未初始化")
	}
	if strings.TrimSpace(archiveID) == "" {
		generated, err := NewGitHubArchiveID()
		if err != nil {
			return RemoteArchiveResult{}, err
		}
		archiveID = generated
	}
	if err := validateArchiveID(archiveID); err != nil {
		return RemoteArchiveResult{}, err
	}
	root, err := validateArchiveDirectory(directory)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	manifestBytes, source, err := readS3SourceManifest(root)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	if source.Encrypted {
		if err := validateS3Encryption(source); err != nil {
			return RemoteArchiveResult{}, err
		}
	} else if _, verifyErr := VerifyLocal(ctx, root); verifyErr != nil {
		return RemoteArchiveResult{}, fmt.Errorf("本地归档校验失败: %w", verifyErr)
	}
	sourceHash := sha256Hex(manifestBytes)
	if err := p.checkArchiveReuse(ctx, archiveID, sourceHash); err != nil {
		return RemoteArchiveResult{}, err
	}
	files, err := regularArchiveFiles(root)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	remoteManifest := GitHubRemoteManifest{Version: 1, ArchiveID: archiveID, CreatedAt: time.Now().UTC(), SourceManifestSHA256: sourceHash, Files: make([]GitHubRemoteFile, 0, len(files))}
	chunkCount := 0
	for _, relative := range files {
		remoteFile, count, uploadErr := p.uploadFile(ctx, root, archiveID, relative)
		if uploadErr != nil {
			return RemoteArchiveResult{}, uploadErr
		}
		remoteManifest.Files = append(remoteManifest.Files, remoteFile)
		chunkCount += count
	}
	if err := p.validateRemoteManifest(remoteManifest, archiveID); err != nil {
		return RemoteArchiveResult{}, err
	}
	encoded, err := json.MarshalIndent(remoteManifest, "", "  ")
	if err != nil {
		return RemoteArchiveResult{}, fmt.Errorf("生成 S3 归档 manifest 失败: %w", err)
	}
	manifestKey := p.objectKey(archiveID, "remote-manifest.json")
	if err := p.putObject(ctx, manifestKey, append(encoded, '\n')); err != nil {
		return RemoteArchiveResult{}, err
	}
	if err := p.cleanupUnreferenced(ctx, archiveID, remoteManifest); err != nil {
		return RemoteArchiveResult{}, err
	}
	verified, err := p.VerifyArchive(ctx, archiveID)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	verified.FileCount = len(files)
	verified.ChunkCount = chunkCount
	return verified, nil
}

// VerifyArchive 只读取并校验远端 manifest、每个分片以及源 manifest 摘要。
func (p *S3Provider) VerifyArchive(ctx context.Context, archiveID string) (RemoteArchiveResult, error) {
	manifest, err := p.readRemoteManifest(ctx, archiveID)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	remoteFiles := make(map[string]GitHubRemoteFile, len(manifest.Files))
	var sourceManifestBytes []byte
	chunkCount := 0
	for _, remoteFile := range manifest.Files {
		fileHash := sha256.New()
		var fileSize int64
		var manifestBuffer bytes.Buffer
		var target io.Writer = io.MultiWriter(io.Discard, fileHash)
		if remoteFile.RelativePath == manifestName {
			target = io.MultiWriter(&manifestBuffer, fileHash)
		}
		for _, chunk := range remoteFile.Chunks {
			hash, size, _, streamErr := p.streamObjectTo(ctx, chunk.Path, target)
			if streamErr != nil {
				return RemoteArchiveResult{}, streamErr
			}
			if hash != chunk.SHA256 || size != chunk.SizeBytes {
				return RemoteArchiveResult{}, fmt.Errorf("S3 分片校验失败: %s", chunk.Path)
			}
			fileSize += size
			chunkCount++
		}
		if fileSize != remoteFile.SizeBytes || hex.EncodeToString(fileHash.Sum(nil)) != remoteFile.SHA256 {
			return RemoteArchiveResult{}, fmt.Errorf("S3 文件校验失败: %s", remoteFile.RelativePath)
		}
		if remoteFile.RelativePath == manifestName {
			sourceManifestBytes = append([]byte(nil), manifestBuffer.Bytes()...)
		}
		remoteFiles[remoteFile.RelativePath] = remoteFile
	}
	if sha256Hex(sourceManifestBytes) != manifest.SourceManifestSHA256 {
		return RemoteArchiveResult{}, fmt.Errorf("S3 源 manifest 摘要不匹配")
	}
	var source Manifest
	if err := json.Unmarshal(sourceManifestBytes, &source); err != nil || source.Version != 1 || source.OperationID == "" || len(source.Assets) == 0 {
		return RemoteArchiveResult{}, fmt.Errorf("S3 源 manifest 无效")
	}
	if source.Encrypted {
		if err := validateS3Encryption(source); err != nil {
			return RemoteArchiveResult{}, err
		}
	} else if err := validateS3Payloads(source, remoteFiles); err != nil {
		return RemoteArchiveResult{}, err
	}
	return RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: p.objectKey(archiveID, "remote-manifest.json"), FileCount: len(manifest.Files), ChunkCount: chunkCount, Verified: true}, nil
}

// DownloadArchive 将远端归档下载到独立 staging 目录，成功后原子发布且拒绝覆盖已有目录。
func (p *S3Provider) DownloadArchive(ctx context.Context, archiveID, destination string) (RemoteArchiveResult, error) {
	manifest, err := p.readRemoteManifest(ctx, archiveID)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	root, err := prepareDownloadRoot(destination, archiveID)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	staging := filepath.Join(filepath.Dir(root), "."+archiveID+".tmp")
	if err := os.Mkdir(staging, 0o700); err != nil {
		return RemoteArchiveResult{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	chunkCount := 0
	for _, remoteFile := range manifest.Files {
		relative, err := safeRemoteRelative(remoteFile.RelativePath)
		if err != nil {
			return RemoteArchiveResult{}, err
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if err := ensureParent(target); err != nil {
			return RemoteArchiveResult{}, err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return RemoteArchiveResult{}, fmt.Errorf("创建下载文件 %s 失败: %w", relative, err)
		}
		fileHash := sha256.New()
		var fileSize int64
		for _, chunk := range remoteFile.Chunks {
			hash, size, _, streamErr := p.streamObjectTo(ctx, chunk.Path, io.MultiWriter(output, fileHash))
			if streamErr != nil {
				_ = output.Close()
				return RemoteArchiveResult{}, streamErr
			}
			if hash != chunk.SHA256 || size != chunk.SizeBytes {
				_ = output.Close()
				return RemoteArchiveResult{}, fmt.Errorf("S3 分片校验失败: %s", chunk.Path)
			}
			fileSize += size
			chunkCount++
		}
		if err := output.Close(); err != nil {
			return RemoteArchiveResult{}, err
		}
		if fileSize != remoteFile.SizeBytes || hex.EncodeToString(fileHash.Sum(nil)) != remoteFile.SHA256 {
			return RemoteArchiveResult{}, fmt.Errorf("S3 文件校验失败: %s", relative)
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(staging, manifestName))
	if err != nil {
		return RemoteArchiveResult{}, fmt.Errorf("下载归档缺少 manifest: %w", err)
	}
	if sha256Hex(manifestBytes) != manifest.SourceManifestSHA256 {
		return RemoteArchiveResult{}, fmt.Errorf("S3 源 manifest 摘要不匹配")
	}
	var source Manifest
	if err := json.Unmarshal(manifestBytes, &source); err != nil || source.Version != 1 || source.OperationID == "" || len(source.Assets) == 0 {
		return RemoteArchiveResult{}, fmt.Errorf("下载归档的源 manifest 无效")
	}
	if source.Encrypted {
		if err := validateS3Encryption(source); err != nil {
			return RemoteArchiveResult{}, err
		}
	} else if _, err := VerifyLocal(ctx, staging); err != nil {
		return RemoteArchiveResult{}, fmt.Errorf("下载归档本地校验失败: %w", err)
	}
	if err := os.Rename(staging, root); err != nil {
		return RemoteArchiveResult{}, fmt.Errorf("发布下载归档失败: %w", err)
	}
	cleanup = false
	return RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: p.objectKey(archiveID, "remote-manifest.json"), FileCount: len(manifest.Files), ChunkCount: chunkCount, Verified: true}, nil
}

// DeleteArchive 删除指定归档前缀内的全部对象；这也会清理失败重试遗留的分片。
func (p *S3Provider) DeleteArchive(ctx context.Context, archiveID string) error {
	if err := validateArchiveID(archiveID); err != nil {
		return err
	}
	if _, err := p.readRemoteManifest(ctx, archiveID); err != nil {
		return err
	}
	keys, err := p.listKeys(ctx, p.objectKey(archiveID, "")+"/")
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := p.deleteObject(ctx, key); err != nil && !errors.Is(err, errS3NotFound) {
			return err
		}
	}
	return nil
}

// ListArchives 列出 Prefix 下含有有效 remote-manifest.json 的归档。
func (p *S3Provider) ListArchives(ctx context.Context) ([]GitHubArchiveSummary, error) {
	rootPrefix := p.rootPrefix()
	result, err := p.listObjects(ctx, rootPrefix, "/")
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{})
	for _, prefix := range result.Prefixes {
		id := strings.TrimPrefix(prefix, rootPrefix)
		id = strings.TrimSuffix(id, "/")
		if id != "" && !strings.Contains(id, "/") && validateArchiveID(id) == nil {
			ids[id] = struct{}{}
		}
	}
	for _, key := range result.Keys {
		if !strings.HasPrefix(key, rootPrefix) {
			continue
		}
		rest := strings.TrimPrefix(key, rootPrefix)
		parts := strings.Split(rest, "/")
		if len(parts) > 1 && validateArchiveID(parts[0]) == nil {
			ids[parts[0]] = struct{}{}
		}
	}
	archives := make([]GitHubArchiveSummary, 0, len(ids))
	for archiveID := range ids {
		manifest, manifestErr := p.readRemoteManifest(ctx, archiveID)
		if errors.Is(manifestErr, errS3NotFound) {
			continue
		}
		if manifestErr != nil {
			return nil, manifestErr
		}
		chunkCount := 0
		for _, file := range manifest.Files {
			chunkCount += len(file.Chunks)
		}
		archives = append(archives, GitHubArchiveSummary{ArchiveID: archiveID, ManifestPath: p.objectKey(archiveID, "remote-manifest.json"), CreatedAt: manifest.CreatedAt, FileCount: len(manifest.Files), ChunkCount: chunkCount, SourceManifestSHA256: manifest.SourceManifestSHA256})
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].CreatedAt.Equal(archives[j].CreatedAt) {
			return archives[i].ArchiveID < archives[j].ArchiveID
		}
		return archives[i].CreatedAt.After(archives[j].CreatedAt)
	})
	return archives, nil
}

func (p *S3Provider) uploadFile(ctx context.Context, root, archiveID, relative string) (GitHubRemoteFile, int, error) {
	input, err := os.Open(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return GitHubRemoteFile{}, 0, err
	}
	defer input.Close()
	result := GitHubRemoteFile{RelativePath: filepath.ToSlash(relative), Chunks: make([]GitHubRemoteChunk, 0)}
	hash := sha256.New()
	buffer := make([]byte, p.config.ChunkBytes)
	var total int64
	index := 0
	for {
		read, readErr := input.Read(buffer)
		if read > 0 {
			content := append([]byte(nil), buffer[:read]...)
			chunkPath := p.objectKey(archiveID, "files/"+relative+fmt.Sprintf(".part-%06d", index))
			if err := p.putObject(ctx, chunkPath, content); err != nil {
				return GitHubRemoteFile{}, 0, err
			}
			chunkHash := sha256Hex(content)
			result.Chunks = append(result.Chunks, GitHubRemoteChunk{Path: chunkPath, SizeBytes: int64(read), SHA256: chunkHash})
			_, _ = hash.Write(content)
			total += int64(read)
			index++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return GitHubRemoteFile{}, 0, fmt.Errorf("读取归档文件 %s 失败: %w", relative, readErr)
		}
	}
	if index == 0 {
		chunkPath := p.objectKey(archiveID, "files/"+relative+".part-000000")
		if err := p.putObject(ctx, chunkPath, nil); err != nil {
			return GitHubRemoteFile{}, 0, err
		}
		result.Chunks = append(result.Chunks, GitHubRemoteChunk{Path: chunkPath, SizeBytes: 0, SHA256: sha256Hex(nil)})
		index = 1
	}
	result.SizeBytes = total
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return result, index, nil
}

func (p *S3Provider) checkArchiveReuse(ctx context.Context, archiveID, sourceHash string) error {
	manifest, err := p.readRemoteManifest(ctx, archiveID)
	if errors.Is(err, errS3NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if manifest.SourceManifestSHA256 != sourceHash {
		return fmt.Errorf("archive ID %s 已绑定其他源归档，请使用新的归档 ID", archiveID)
	}
	return nil
}

func (p *S3Provider) cleanupUnreferenced(ctx context.Context, archiveID string, manifest GitHubRemoteManifest) error {
	keys, err := p.listKeys(ctx, p.objectKey(archiveID, "files/")+"/")
	if err != nil {
		return err
	}
	keep := make(map[string]struct{})
	for _, file := range manifest.Files {
		for _, chunk := range file.Chunks {
			keep[chunk.Path] = struct{}{}
		}
	}
	for _, key := range keys {
		if _, ok := keep[key]; ok {
			continue
		}
		if err := p.deleteObject(ctx, key); err != nil && !errors.Is(err, errS3NotFound) {
			return err
		}
	}
	return nil
}

func (p *S3Provider) readRemoteManifest(ctx context.Context, archiveID string) (*GitHubRemoteManifest, error) {
	if err := validateArchiveID(archiveID); err != nil {
		return nil, err
	}
	data, err := p.getObject(ctx, p.objectKey(archiveID, "remote-manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest GitHubRemoteManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("解析 S3 归档 manifest 失败: %w", err)
	}
	if err := p.validateRemoteManifest(manifest, archiveID); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (p *S3Provider) validateRemoteManifest(manifest GitHubRemoteManifest, archiveID string) error {
	if manifest.Version != 1 || manifest.ArchiveID != archiveID || len(manifest.Files) == 0 || !validSHA256(manifest.SourceManifestSHA256) {
		return fmt.Errorf("S3 归档 manifest 无效")
	}
	chunkRoot := p.objectKey(archiveID, "files") + "/"
	seenFiles := make(map[string]struct{}, len(manifest.Files))
	seenChunks := make(map[string]struct{})
	hasSourceManifest := false
	for _, file := range manifest.Files {
		relative, err := safeRemoteRelative(file.RelativePath)
		if err != nil || relative != file.RelativePath || file.SizeBytes < 0 || !validSHA256(file.SHA256) || len(file.Chunks) == 0 {
			return fmt.Errorf("S3 归档文件清单无效: %s", file.RelativePath)
		}
		if _, ok := seenFiles[relative]; ok {
			return fmt.Errorf("S3 归档包含重复文件: %s", relative)
		}
		seenFiles[relative] = struct{}{}
		if relative == manifestName {
			hasSourceManifest = true
		}
		var chunkBytes int64
		for _, chunk := range file.Chunks {
			chunkPath, err := safeRemoteRelative(chunk.Path)
			if err != nil || chunkPath != chunk.Path || !strings.HasPrefix(chunkPath, chunkRoot) || chunk.SizeBytes < 0 || chunk.SizeBytes > defaultS3ChunkBytes || !validSHA256(chunk.SHA256) {
				return fmt.Errorf("S3 分片清单无效: %s", chunk.Path)
			}
			if _, ok := seenChunks[chunkPath]; ok {
				return fmt.Errorf("S3 归档包含重复分片: %s", chunkPath)
			}
			seenChunks[chunkPath] = struct{}{}
			if chunkBytes > file.SizeBytes-chunk.SizeBytes {
				return fmt.Errorf("S3 文件分片大小溢出: %s", file.RelativePath)
			}
			chunkBytes += chunk.SizeBytes
		}
		if chunkBytes != file.SizeBytes {
			return fmt.Errorf("S3 文件分片大小不匹配: %s", file.RelativePath)
		}
	}
	if !hasSourceManifest {
		return fmt.Errorf("S3 归档缺少源 manifest.json")
	}
	return nil
}

func (p *S3Provider) validateRemoteKey(key string) error {
	clean, err := safeRemoteRelative(key)
	if err != nil || clean != key {
		return fmt.Errorf("S3 对象键无效: %s", key)
	}
	return nil
}

func (p *S3Provider) putObject(ctx context.Context, key string, content []byte) error {
	if err := p.validateRemoteKey(key); err != nil {
		return err
	}
	response, err := p.perform(ctx, http.MethodPut, key, nil, content)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return p.s3ResponseError(response, "上传", key)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

func (p *S3Provider) getObject(ctx context.Context, key string) ([]byte, error) {
	var buffer bytes.Buffer
	_, _, _, err := p.streamObjectTo(ctx, key, &buffer)
	if err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (p *S3Provider) streamObjectTo(ctx context.Context, key string, target io.Writer) (string, int64, []byte, error) {
	if err := p.validateRemoteKey(key); err != nil {
		return "", 0, nil, err
	}
	response, err := p.perform(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return "", 0, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", 0, nil, errS3NotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", 0, nil, p.s3ResponseError(response, "读取", key)
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(target, hash), response.Body)
	return hex.EncodeToString(hash.Sum(nil)), size, nil, err
}

func (p *S3Provider) deleteObject(ctx context.Context, key string) error {
	if err := p.validateRemoteKey(key); err != nil {
		return err
	}
	response, err := p.perform(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return errS3NotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return p.s3ResponseError(response, "删除", key)
	}
	return nil
}

type s3ListResult struct {
	Keys      []string
	Prefixes  []string
	Truncated bool
	NextToken string
}

type s3ListXML struct {
	IsTruncated      bool   `xml:"IsTruncated"`
	NextContinuation string `xml:"NextContinuationToken"`
	Contents         []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (p *S3Provider) listObjects(ctx context.Context, prefix, delimiter string) (s3ListResult, error) {
	result := s3ListResult{}
	token := ""
	for {
		query := url.Values{"list-type": []string{"2"}, "max-keys": []string{"1000"}}
		if prefix != "" {
			query.Set("prefix", prefix)
		}
		if delimiter != "" {
			query.Set("delimiter", delimiter)
		}
		if token != "" {
			query.Set("continuation-token", token)
		}
		response, err := p.perform(ctx, http.MethodGet, "", query, nil)
		if err != nil {
			return s3ListResult{}, err
		}
		if response.StatusCode == http.StatusNotFound {
			response.Body.Close()
			return s3ListResult{}, errS3NotFound
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			err := p.s3ResponseError(response, "列出对象", prefix)
			response.Body.Close()
			return s3ListResult{}, err
		}
		var page s3ListXML
		decodeErr := xml.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&page)
		response.Body.Close()
		if decodeErr != nil {
			return s3ListResult{}, fmt.Errorf("解析 S3 对象列表失败: %w", decodeErr)
		}
		for _, item := range page.Contents {
			result.Keys = append(result.Keys, item.Key)
		}
		for _, item := range page.CommonPrefixes {
			result.Prefixes = append(result.Prefixes, item.Prefix)
		}
		if !page.IsTruncated || page.NextContinuation == "" {
			break
		}
		token = page.NextContinuation
	}
	return result, nil
}

func (p *S3Provider) listKeys(ctx context.Context, prefix string) ([]string, error) {
	result, err := p.listObjects(ctx, prefix, "")
	if err != nil {
		return nil, err
	}
	return result.Keys, nil
}

func (p *S3Provider) perform(ctx context.Context, method, key string, query url.Values, content []byte) (*http.Response, error) {
	const attempts = 3
	for attempt := 0; attempt < attempts; attempt++ {
		request, err := p.newSignedRequest(ctx, method, key, query, content)
		if err != nil {
			return nil, err
		}
		response, doErr := p.client.Do(request)
		if doErr == nil && response.StatusCode < 500 {
			return response, nil
		}
		if response != nil {
			response.Body.Close()
		}
		if attempt == attempts-1 {
			if doErr != nil {
				return nil, fmt.Errorf("S3 %s %s 失败: %w", method, key, doErr)
			}
			return nil, fmt.Errorf("S3 %s %s 连续返回服务器错误", method, key)
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, ctx.Err()
}

func (p *S3Provider) newSignedRequest(ctx context.Context, method, key string, query url.Values, content []byte) (*http.Request, error) {
	requestURL, err := p.objectURL(key)
	if err != nil {
		return nil, err
	}
	if query != nil {
		requestURL.RawQuery = canonicalS3Query(query)
	}
	payloadHash := sha256Hex(content)
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	request.Host = requestURL.Host
	request.Header.Set("x-amz-content-sha256", payloadHash)
	request.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	if p.config.SessionToken != "" {
		request.Header.Set("x-amz-security-token", p.config.SessionToken)
	}
	if method == http.MethodPut {
		request.ContentLength = int64(len(content))
	}
	if err := signS3Request(request, p.config.AccessKeyID, p.config.SecretAccessKey, p.config.Region, "s3", payloadHash); err != nil {
		return nil, err
	}
	return request, nil
}

func (p *S3Provider) objectKey(archiveID, relative string) string {
	parts := make([]string, 0, 3)
	if p.config.Prefix != "" {
		parts = append(parts, strings.Trim(p.config.Prefix, "/"))
	}
	if archiveID != "" {
		parts = append(parts, strings.Trim(archiveID, "/"))
	}
	if strings.Trim(relative, "/") != "" {
		parts = append(parts, strings.Trim(relative, "/"))
	}
	return strings.Join(parts, "/")
}

func (p *S3Provider) rootPrefix() string {
	if p.config.Prefix == "" {
		return ""
	}
	return strings.Trim(p.config.Prefix, "/") + "/"
}

func (p *S3Provider) objectURL(key string) (*url.URL, error) {
	if key != "" {
		if err := p.validateRemoteKey(key); err != nil {
			return nil, err
		}
	}
	copyURL := *p.endpoint
	baseRaw := strings.TrimRight(copyURL.EscapedPath(), "/")
	if p.config.PathStyle {
		baseRaw += "/" + awsEncode(p.config.Bucket, true)
	} else {
		copyURL.Host = p.config.Bucket + "." + copyURL.Host
	}
	if key != "" {
		baseRaw += "/" + awsEncode(key, false)
	}
	if baseRaw == "" {
		baseRaw = "/"
	}
	decoded, err := url.PathUnescape(baseRaw)
	if err != nil {
		return nil, fmt.Errorf("S3 对象路径编码失败: %w", err)
	}
	copyURL.Path = decoded
	copyURL.RawPath = baseRaw
	return &copyURL, nil
}

func (p *S3Provider) s3ResponseError(response *http.Response, action, key string) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	return fmt.Errorf("S3 %s %s 失败（HTTP %d）: %s", action, key, response.StatusCode, strings.TrimSpace(string(body)))
}

func validS3Bucket(value string) bool {
	if len(value) < 3 || len(value) > 63 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return value[0] != '-' && value[len(value)-1] != '-' && !strings.Contains(value, "..")
}

func readS3SourceManifest(root string) ([]byte, Manifest, error) {
	data, err := os.ReadFile(filepath.Join(root, manifestName))
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("读取本地归档 manifest 失败: %w", err)
	}
	var source Manifest
	if err := json.Unmarshal(data, &source); err != nil || source.Version != 1 || source.OperationID == "" || len(source.Assets) == 0 {
		return nil, Manifest{}, fmt.Errorf("本地归档 manifest 无效")
	}
	return data, source, nil
}

func validateS3Encryption(source Manifest) error {
	if source.Encryption == nil || source.Encryption.KDF != encryptionKDF || source.Encryption.Cipher != encryptionCipher || source.Encryption.Iterations <= 0 {
		return fmt.Errorf("S3 加密归档参数无效")
	}
	if salt, err := hex.DecodeString(source.Encryption.Salt); err != nil || len(salt) == 0 {
		return fmt.Errorf("S3 加密归档盐无效")
	}
	return nil
}

func validateS3Payloads(source Manifest, remoteFiles map[string]GitHubRemoteFile) error {
	for _, asset := range source.Assets {
		relative, err := safePayloadRelative(asset.RelativePath)
		if err != nil {
			return fmt.Errorf("S3 源 manifest 资产路径无效: %w", err)
		}
		remoteFile, ok := remoteFiles["payload/"+relative]
		if !ok || remoteFile.SizeBytes != asset.SizeBytes || remoteFile.SHA256 != asset.SHA256 {
			return fmt.Errorf("S3 payload 与源 manifest 不匹配: %s", asset.RelativePath)
		}
	}
	return nil
}

func signS3Request(request *http.Request, accessKey, secretKey, region, service, payloadHash string) error {
	amzDate := request.Header.Get("x-amz-date")
	if len(amzDate) != len("20060102T150405Z") {
		return fmt.Errorf("S3 x-amz-date 无效")
	}
	date := amzDate[:8]
	headerValues := make(map[string][]string)
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "host" {
			continue
		}
		headerValues[lower] = append(headerValues[lower], values...)
	}
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	headerValues["host"] = []string{host}
	names := make([]string, 0, len(headerValues))
	for name := range headerValues {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		values := make([]string, 0, len(headerValues[name]))
		for _, value := range headerValues[name] {
			values = append(values, strings.Join(strings.Fields(value), " "))
		}
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.Join(values, ","))
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{request.Method, request.URL.EscapedPath(), canonicalS3Query(request.URL.Query()), canonicalHeaders.String(), signedHeaders, payloadHash}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(hashBytes([]byte(canonicalRequest)))}, "\n")
	signature := hex.EncodeToString(hmacSHA256(deriveS3SigningKey(secretKey, date, region, service), []byte(stringToSign)))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
	return nil
}

func canonicalS3Query(query url.Values) string {
	type pair struct{ key, value string }
	pairs := make([]pair, 0)
	for key, values := range query {
		if len(values) == 0 {
			pairs = append(pairs, pair{key: awsEncode(key, true), value: ""})
			continue
		}
		for _, value := range values {
			pairs = append(pairs, pair{key: awsEncode(key, true), value: awsEncode(value, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	parts := make([]string, 0, len(pairs))
	for _, item := range pairs {
		parts = append(parts, item.key+"="+item.value)
	}
	return strings.Join(parts, "&")
}

func awsEncode(value string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' || character == '~' || (!encodeSlash && character == '/') {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[character>>4])
		builder.WriteByte(hexDigits[character&0x0f])
	}
	return builder.String()
}

func hashBytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func hmacSHA256(key, value []byte) []byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write(value)
	return hash.Sum(nil)
}

func deriveS3SigningKey(secret, date, region, service string) []byte {
	dateKey := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	regionKey := hmacSHA256(dateKey, []byte(region))
	serviceKey := hmacSHA256(regionKey, []byte(service))
	return hmacSHA256(serviceKey, []byte("aws4_request"))
}
