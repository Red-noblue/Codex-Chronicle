// 中文说明：实现受限的自定义 HTTP 对象归档 provider；使用分片、manifest、索引、校验和 staging 恢复。
package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultHTTPChunkBytes int64 = 50 * 1024 * 1024
	maxHTTPManifestBytes        = 64 * 1024 * 1024
	httpArchiveIndexName        = "archives.json"
	httpRequestAttempts         = 3
)

var errHTTPArchiveNotFound = errors.New("HTTP 归档对象不存在")

// HTTPArchiveConfig 是自定义 HTTP 对象协议的运行时配置；凭证只保存在内存中。
type HTTPArchiveConfig struct {
	BaseURL    string
	RootPath   string
	IndexPath  string
	Username   string
	Password   string
	Token      string
	ProxyURL   string
	ChunkBytes int64
	HTTPClient *http.Client
}

// HTTPArchiveProvider 使用固定对象路径和 archives.json 索引保存远端归档。
// 协议约定：对象使用 PUT/GET/DELETE，归档清单位于 {root}/{archiveID}/remote-manifest.json，
// 索引位于 {root}/archives.json，服务端不需要实现 WebDAV 或云厂商专有 API。
type HTTPArchiveProvider struct {
	config HTTPArchiveConfig
	base   *url.URL
	client *http.Client
}

type httpArchiveIndex struct {
	Version  int                    `json:"version"`
	Archives []GitHubArchiveSummary `json:"archives"`
}

// ProviderInfo 返回自定义 HTTP provider 的非敏感能力摘要。
func (p *HTTPArchiveProvider) ProviderInfo() RemoteArchiveProviderInfo {
	return RemoteArchiveProviderInfo{ID: "http", Name: "自定义 HTTP", TargetRequired: false, SupportsListing: true, SupportsRecovery: true, SupportsDelete: true}
}

// NewHTTPArchiveProvider 校验自定义 HTTP 对象协议配置。
func NewHTTPArchiveProvider(config HTTPArchiveConfig) (*HTTPArchiveProvider, error) {
	baseValue := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseValue == "" {
		return nil, fmt.Errorf("HTTP 归档 BaseURL 不能为空")
	}
	base, err := url.Parse(baseValue)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("HTTP 归档 BaseURL 无效")
	}
	if strings.TrimSpace(config.RootPath) != "" {
		root, rootErr := safeRemoteRelative(config.RootPath)
		if rootErr != nil {
			return nil, fmt.Errorf("HTTP 归档 RootPath 无效: %w", rootErr)
		}
		config.RootPath = root
	}
	if strings.TrimSpace(config.IndexPath) == "" {
		config.IndexPath = httpArchiveIndexName
	}
	indexPath, err := safeRemoteRelative(config.IndexPath)
	if err != nil || strings.Contains(indexPath, "/") {
		return nil, fmt.Errorf("HTTP 归档 IndexPath 必须是根目录下的单个文件")
	}
	config.IndexPath = indexPath
	if strings.TrimSpace(config.Token) != "" && (strings.TrimSpace(config.Username) != "" || strings.TrimSpace(config.Password) != "") {
		return nil, fmt.Errorf("HTTP 归档认证方式只能选择 Bearer 或 Basic 之一")
	}
	if strings.TrimSpace(config.Password) != "" && strings.TrimSpace(config.Username) == "" {
		return nil, fmt.Errorf("HTTP 归档 Basic 密码不能脱离用户名")
	}
	if config.ChunkBytes == 0 {
		config.ChunkBytes = defaultHTTPChunkBytes
	}
	if config.ChunkBytes < 1 || config.ChunkBytes > defaultHTTPChunkBytes {
		return nil, fmt.Errorf("HTTP 归档分片大小必须在 1 到 50 MiB 之间")
	}
	client, err := newGitHubHTTPClient(config.ProxyURL, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	return &HTTPArchiveProvider{config: config, base: base, client: client}, nil
}

// UploadDirectory 上传一个已验证的本地归档目录；manifest 最后发布，失败后可用同一 ID 重试。
func (p *HTTPArchiveProvider) UploadDirectory(ctx context.Context, directory, archiveID string) (RemoteArchiveResult, error) {
	if p == nil {
		return RemoteArchiveResult{}, fmt.Errorf("HTTP 归档 provider 未初始化")
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
	manifestPath := filepath.Join(root, manifestName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return RemoteArchiveResult{}, fmt.Errorf("读取本地归档 manifest 失败: %w", err)
	}
	var source Manifest
	if err := json.Unmarshal(manifestBytes, &source); err != nil || source.Version != 1 || source.OperationID == "" || len(source.Assets) == 0 {
		return RemoteArchiveResult{}, fmt.Errorf("本地归档 manifest 无效")
	}
	if !source.Encrypted {
		if _, verifyErr := VerifyLocal(ctx, root); verifyErr != nil {
			return RemoteArchiveResult{}, fmt.Errorf("本地归档校验失败: %w", verifyErr)
		}
	}
	sourceHash := sha256Hex(manifestBytes)
	previous, previousErr := p.readRemoteManifest(ctx, archiveID)
	if previousErr != nil && !errors.Is(previousErr, errHTTPArchiveNotFound) {
		return RemoteArchiveResult{}, previousErr
	}
	if previous != nil && previous.SourceManifestSHA256 != sourceHash {
		return RemoteArchiveResult{}, fmt.Errorf("archive ID %s 已绑定其他源归档，请使用新的归档 ID", archiveID)
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
		return RemoteArchiveResult{}, fmt.Errorf("生成 HTTP 归档 manifest 失败: %w", err)
	}
	manifestRemotePath := p.objectPath(archiveID, "remote-manifest.json")
	if err := p.putBytes(ctx, manifestRemotePath, append(encoded, '\n')); err != nil {
		return RemoteArchiveResult{}, err
	}
	verified, err := p.VerifyArchive(ctx, archiveID)
	if err != nil || !verified.Verified {
		if err != nil {
			return RemoteArchiveResult{}, err
		}
		return RemoteArchiveResult{}, fmt.Errorf("HTTP 归档发布后校验失败")
	}
	summary := GitHubArchiveSummary{ArchiveID: archiveID, ManifestPath: manifestRemotePath, CreatedAt: remoteManifest.CreatedAt, FileCount: len(remoteManifest.Files), ChunkCount: chunkCount, SourceManifestSHA256: sourceHash}
	if err := p.upsertIndex(ctx, summary); err != nil {
		return RemoteArchiveResult{}, err
	}
	return RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: manifestRemotePath, FileCount: len(remoteManifest.Files), ChunkCount: chunkCount, Verified: true}, nil
}

func (p *HTTPArchiveProvider) uploadFile(ctx context.Context, root, archiveID, relative string) (GitHubRemoteFile, int, error) {
	input, err := os.Open(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return GitHubRemoteFile{}, 0, err
	}
	defer input.Close()
	result := GitHubRemoteFile{RelativePath: filepath.ToSlash(relative), Chunks: make([]GitHubRemoteChunk, 0)}
	hash := sha256.New()
	buffer := make([]byte, p.config.ChunkBytes)
	index := 0
	var total int64
	for {
		read, readErr := input.Read(buffer)
		if read > 0 {
			content := append([]byte(nil), buffer[:read]...)
			chunkPath := p.objectPath(archiveID, "files/"+relative+fmt.Sprintf(".part-%06d", index))
			if err := p.putBytes(ctx, chunkPath, content); err != nil {
				return GitHubRemoteFile{}, 0, fmt.Errorf("上传 HTTP 归档分片 %s 失败: %w", relative, err)
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
		chunkPath := p.objectPath(archiveID, "files/"+relative+".part-000000")
		if err := p.putBytes(ctx, chunkPath, nil); err != nil {
			return GitHubRemoteFile{}, 0, err
		}
		result.Chunks = append(result.Chunks, GitHubRemoteChunk{Path: chunkPath, SHA256: sha256Hex(nil)})
		index = 1
	}
	result.SizeBytes = total
	result.SHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	return result, index, nil
}

// VerifyArchive 读取远端 manifest、所有分片和源 manifest，完全在远端校验阶段不创建本地文件。
func (p *HTTPArchiveProvider) VerifyArchive(ctx context.Context, archiveID string) (RemoteArchiveResult, error) {
	if p == nil {
		return RemoteArchiveResult{}, fmt.Errorf("HTTP 归档 provider 未初始化")
	}
	manifest, err := p.readRemoteManifest(ctx, archiveID)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	remoteFiles := make(map[string]GitHubRemoteFile, len(manifest.Files))
	var sourceManifestBytes []byte
	chunkCount := 0
	for _, remoteFile := range manifest.Files {
		fileHash := sha256.New()
		var size int64
		var manifestBuffer bytes.Buffer
		for _, chunk := range remoteFile.Chunks {
			var target io.Writer = fileHash
			if remoteFile.RelativePath == manifestName {
				target = io.MultiWriter(fileHash, &manifestBuffer)
			}
			actualHash, actualSize, streamErr := p.streamObject(ctx, chunk.Path, target, defaultHTTPChunkBytes)
			if streamErr != nil {
				return RemoteArchiveResult{}, streamErr
			}
			if actualSize != chunk.SizeBytes || actualHash != chunk.SHA256 {
				return RemoteArchiveResult{}, fmt.Errorf("HTTP 归档分片校验失败: %s", chunk.Path)
			}
			size += actualSize
			chunkCount++
		}
		if size != remoteFile.SizeBytes || fmt.Sprintf("%x", fileHash.Sum(nil)) != remoteFile.SHA256 {
			return RemoteArchiveResult{}, fmt.Errorf("HTTP 归档文件校验失败: %s", remoteFile.RelativePath)
		}
		if remoteFile.RelativePath == manifestName {
			sourceManifestBytes = append([]byte(nil), manifestBuffer.Bytes()...)
		}
		remoteFiles[remoteFile.RelativePath] = remoteFile
	}
	if sha256Hex(sourceManifestBytes) != manifest.SourceManifestSHA256 {
		return RemoteArchiveResult{}, fmt.Errorf("HTTP 源 manifest 摘要不匹配")
	}
	var source Manifest
	if err := json.Unmarshal(sourceManifestBytes, &source); err != nil || source.Version != 1 || source.OperationID == "" || len(source.Assets) == 0 {
		return RemoteArchiveResult{}, fmt.Errorf("HTTP 源 manifest 无效")
	}
	if !source.Encrypted {
		for _, asset := range source.Assets {
			relative, pathErr := safePayloadRelative(asset.RelativePath)
			if pathErr != nil {
				return RemoteArchiveResult{}, fmt.Errorf("HTTP 源 manifest 资产路径无效: %w", pathErr)
			}
			remoteFile, exists := remoteFiles["payload/"+relative]
			if !exists || remoteFile.SizeBytes != asset.SizeBytes || remoteFile.SHA256 != asset.SHA256 {
				return RemoteArchiveResult{}, fmt.Errorf("HTTP payload 与源 manifest 不匹配: %s", asset.RelativePath)
			}
		}
	}
	return RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: p.objectPath(archiveID, "remote-manifest.json"), FileCount: len(manifest.Files), ChunkCount: chunkCount, Verified: true}, nil
}

// DownloadArchive 将远端归档下载到独立 staging 目录，校验后原子发布且拒绝覆盖。
func (p *HTTPArchiveProvider) DownloadArchive(ctx context.Context, archiveID, destination string) (RemoteArchiveResult, error) {
	if p == nil {
		return RemoteArchiveResult{}, fmt.Errorf("HTTP 归档 provider 未初始化")
	}
	if err := validateArchiveID(archiveID); err != nil {
		return RemoteArchiveResult{}, err
	}
	manifest, err := p.readRemoteManifest(ctx, archiveID)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	final, err := prepareDownloadRoot(destination, archiveID)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	staging := final + ".tmp"
	if err := os.Mkdir(staging, 0o700); err != nil {
		return RemoteArchiveResult{}, fmt.Errorf("创建 HTTP 下载临时目录失败: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	chunkCount := 0
	for _, remoteFile := range manifest.Files {
		relative, relativeErr := safeRemoteRelative(remoteFile.RelativePath)
		if relativeErr != nil {
			return RemoteArchiveResult{}, relativeErr
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if err := ensureParent(target); err != nil {
			return RemoteArchiveResult{}, err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return RemoteArchiveResult{}, fmt.Errorf("创建 HTTP 下载文件 %s 失败: %w", relative, err)
		}
		hash := sha256.New()
		var size int64
		for _, chunk := range remoteFile.Chunks {
			actualHash, actualSize, streamErr := p.streamObject(ctx, chunk.Path, io.MultiWriter(output, hash), defaultHTTPChunkBytes)
			if streamErr != nil {
				_ = output.Close()
				return RemoteArchiveResult{}, streamErr
			}
			if actualSize != chunk.SizeBytes || actualHash != chunk.SHA256 {
				_ = output.Close()
				return RemoteArchiveResult{}, fmt.Errorf("HTTP 归档分片校验失败: %s", chunk.Path)
			}
			size += actualSize
			chunkCount++
		}
		if err := output.Close(); err != nil {
			return RemoteArchiveResult{}, err
		}
		if size != remoteFile.SizeBytes || fmt.Sprintf("%x", hash.Sum(nil)) != remoteFile.SHA256 {
			return RemoteArchiveResult{}, fmt.Errorf("HTTP 归档文件校验失败: %s", relative)
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(staging, manifestName))
	if err != nil || sha256Hex(manifestBytes) != manifest.SourceManifestSHA256 {
		return RemoteArchiveResult{}, fmt.Errorf("HTTP 下载归档缺少或损坏源 manifest")
	}
	var source Manifest
	if err := json.Unmarshal(manifestBytes, &source); err != nil || source.Version != 1 || source.OperationID == "" {
		return RemoteArchiveResult{}, fmt.Errorf("HTTP 下载归档源 manifest 无效")
	}
	if !source.Encrypted {
		if _, err := VerifyLocal(ctx, staging); err != nil {
			return RemoteArchiveResult{}, fmt.Errorf("HTTP 下载归档本地校验失败: %w", err)
		}
	}
	if err := os.Rename(staging, final); err != nil {
		return RemoteArchiveResult{}, fmt.Errorf("发布 HTTP 下载归档失败: %w", err)
	}
	cleanup = false
	return RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: p.objectPath(archiveID, "remote-manifest.json"), FileCount: len(manifest.Files), ChunkCount: chunkCount, Verified: true}, nil
}

// DeleteArchive 删除 manifest、分片和索引中的归档记录。
func (p *HTTPArchiveProvider) DeleteArchive(ctx context.Context, archiveID string) error {
	if p == nil {
		return fmt.Errorf("HTTP 归档 provider 未初始化")
	}
	manifest, err := p.readRemoteManifest(ctx, archiveID)
	if err != nil {
		return err
	}
	for _, file := range manifest.Files {
		for _, chunk := range file.Chunks {
			if deleteErr := p.deleteObject(ctx, chunk.Path); deleteErr != nil && !errors.Is(deleteErr, errHTTPArchiveNotFound) {
				return deleteErr
			}
		}
	}
	if deleteErr := p.deleteObject(ctx, p.objectPath(archiveID, "remote-manifest.json")); deleteErr != nil && !errors.Is(deleteErr, errHTTPArchiveNotFound) {
		return deleteErr
	}
	return p.removeIndex(ctx, archiveID)
}

// ListArchives 从固定的 archives.json 索引读取并重新验证每个 manifest。
func (p *HTTPArchiveProvider) ListArchives(ctx context.Context) ([]GitHubArchiveSummary, error) {
	if p == nil {
		return nil, fmt.Errorf("HTTP 归档 provider 未初始化")
	}
	entries, err := p.readIndex(ctx)
	if err != nil {
		return nil, err
	}
	archives := make([]GitHubArchiveSummary, 0, len(entries))
	for _, entry := range entries {
		manifest, manifestErr := p.readRemoteManifest(ctx, entry.ArchiveID)
		if errors.Is(manifestErr, errHTTPArchiveNotFound) {
			continue
		}
		if manifestErr != nil {
			return nil, manifestErr
		}
		chunkCount := 0
		for _, file := range manifest.Files {
			chunkCount += len(file.Chunks)
		}
		archives = append(archives, GitHubArchiveSummary{ArchiveID: entry.ArchiveID, ManifestPath: p.objectPath(entry.ArchiveID, "remote-manifest.json"), CreatedAt: manifest.CreatedAt, FileCount: len(manifest.Files), ChunkCount: chunkCount, SourceManifestSHA256: manifest.SourceManifestSHA256})
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].CreatedAt.Equal(archives[j].CreatedAt) {
			return archives[i].ArchiveID < archives[j].ArchiveID
		}
		return archives[i].CreatedAt.After(archives[j].CreatedAt)
	})
	return archives, nil
}

func (p *HTTPArchiveProvider) readRemoteManifest(ctx context.Context, archiveID string) (*GitHubRemoteManifest, error) {
	if err := validateArchiveID(archiveID); err != nil {
		return nil, err
	}
	data, err := p.getObject(ctx, p.objectPath(archiveID, "remote-manifest.json"), maxHTTPManifestBytes)
	if err != nil {
		return nil, err
	}
	var manifest GitHubRemoteManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("解析 HTTP 归档 manifest 失败: %w", err)
	}
	if err := p.validateRemoteManifest(manifest, archiveID); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (p *HTTPArchiveProvider) validateRemoteManifest(manifest GitHubRemoteManifest, archiveID string) error {
	if manifest.Version != 1 || manifest.ArchiveID != archiveID || len(manifest.Files) == 0 || !validSHA256(manifest.SourceManifestSHA256) {
		return fmt.Errorf("HTTP 归档 manifest 无效")
	}
	chunkRoot := p.objectPath(archiveID, "files") + "/"
	seenFiles := make(map[string]struct{}, len(manifest.Files))
	seenChunks := make(map[string]struct{})
	hasSourceManifest := false
	for _, file := range manifest.Files {
		relative, err := safeRemoteRelative(file.RelativePath)
		if err != nil || relative != file.RelativePath || file.SizeBytes < 0 || !validSHA256(file.SHA256) || len(file.Chunks) == 0 {
			return fmt.Errorf("HTTP 归档文件清单无效: %s", file.RelativePath)
		}
		if _, ok := seenFiles[relative]; ok {
			return fmt.Errorf("HTTP 归档包含重复文件: %s", relative)
		}
		seenFiles[relative] = struct{}{}
		if relative == manifestName {
			hasSourceManifest = true
		}
		var chunkBytes int64
		for _, chunk := range file.Chunks {
			chunkPath, chunkErr := safeRemoteRelative(chunk.Path)
			if chunkErr != nil || chunkPath != chunk.Path || !strings.HasPrefix(chunkPath, chunkRoot) || chunk.SizeBytes < 0 || chunk.SizeBytes > defaultHTTPChunkBytes || !validSHA256(chunk.SHA256) {
				return fmt.Errorf("HTTP 归档分片清单无效: %s", chunk.Path)
			}
			if _, ok := seenChunks[chunkPath]; ok {
				return fmt.Errorf("HTTP 归档包含重复分片: %s", chunk.Path)
			}
			seenChunks[chunkPath] = struct{}{}
			if chunkBytes > file.SizeBytes-chunk.SizeBytes {
				return fmt.Errorf("HTTP 归档文件分片大小溢出: %s", file.RelativePath)
			}
			chunkBytes += chunk.SizeBytes
		}
		if chunkBytes != file.SizeBytes {
			return fmt.Errorf("HTTP 归档文件分片大小不匹配: %s", file.RelativePath)
		}
	}
	if !hasSourceManifest {
		return fmt.Errorf("HTTP 归档缺少源 manifest.json")
	}
	return nil
}

func (p *HTTPArchiveProvider) readIndex(ctx context.Context) ([]GitHubArchiveSummary, error) {
	data, err := p.getObject(ctx, p.indexObjectPath(), maxHTTPManifestBytes)
	if errors.Is(err, errHTTPArchiveNotFound) {
		return []GitHubArchiveSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	var envelope httpArchiveIndex
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Version == 1 {
		return sanitizeHTTPIndex(envelope.Archives)
	}
	var archives []GitHubArchiveSummary
	if err := json.Unmarshal(data, &archives); err != nil {
		return nil, fmt.Errorf("解析 HTTP 归档索引失败: %w", err)
	}
	return sanitizeHTTPIndex(archives)
}

func sanitizeHTTPIndex(values []GitHubArchiveSummary) ([]GitHubArchiveSummary, error) {
	result := make([]GitHubArchiveSummary, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateArchiveID(value.ArchiveID); err != nil {
			return nil, fmt.Errorf("HTTP 归档索引包含无效 ID: %w", err)
		}
		if _, ok := seen[value.ArchiveID]; ok {
			return nil, fmt.Errorf("HTTP 归档索引包含重复 ID: %s", value.ArchiveID)
		}
		seen[value.ArchiveID] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func (p *HTTPArchiveProvider) upsertIndex(ctx context.Context, summary GitHubArchiveSummary) error {
	archives, err := p.readIndex(ctx)
	if err != nil {
		return err
	}
	replaced := false
	for index := range archives {
		if archives[index].ArchiveID == summary.ArchiveID {
			archives[index] = summary
			replaced = true
			break
		}
	}
	if !replaced {
		archives = append(archives, summary)
	}
	return p.writeIndex(ctx, archives)
}

func (p *HTTPArchiveProvider) removeIndex(ctx context.Context, archiveID string) error {
	archives, err := p.readIndex(ctx)
	if err != nil {
		return err
	}
	filtered := archives[:0]
	for _, archive := range archives {
		if archive.ArchiveID != archiveID {
			filtered = append(filtered, archive)
		}
	}
	return p.writeIndex(ctx, filtered)
}

func (p *HTTPArchiveProvider) writeIndex(ctx context.Context, archives []GitHubArchiveSummary) error {
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].CreatedAt.Equal(archives[j].CreatedAt) {
			return archives[i].ArchiveID < archives[j].ArchiveID
		}
		return archives[i].CreatedAt.After(archives[j].CreatedAt)
	})
	data, err := json.MarshalIndent(httpArchiveIndex{Version: 1, Archives: archives}, "", "  ")
	if err != nil {
		return err
	}
	return p.putBytes(ctx, p.indexObjectPath(), append(data, '\n'))
}

func (p *HTTPArchiveProvider) indexObjectPath() string {
	if p.config.RootPath == "" {
		return p.config.IndexPath
	}
	return path.Join(p.config.RootPath, p.config.IndexPath)
}

func (p *HTTPArchiveProvider) objectPath(archiveID, suffix string) string {
	parts := make([]string, 0, 3)
	if p.config.RootPath != "" {
		parts = append(parts, p.config.RootPath)
	}
	parts = append(parts, archiveID)
	if suffix != "" {
		parts = append(parts, suffix)
	}
	return path.Join(parts...)
}

func (p *HTTPArchiveProvider) objectURL(remotePath string) (*url.URL, error) {
	clean, err := safeRemoteRelative(remotePath)
	if err != nil || clean != remotePath {
		return nil, fmt.Errorf("HTTP 归档对象路径无效: %s", remotePath)
	}
	value := *p.base
	basePath := strings.TrimRight(value.Path, "/")
	value.Path = basePath + "/" + clean
	value.RawPath = ""
	value.RawQuery = ""
	value.Fragment = ""
	return &value, nil
}

func (p *HTTPArchiveProvider) putBytes(ctx context.Context, remotePath string, content []byte) error {
	response, err := p.doBytes(ctx, http.MethodPut, remotePath, content)
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}

func (p *HTTPArchiveProvider) getObject(ctx context.Context, remotePath string, limit int64) ([]byte, error) {
	response, err := p.doBytes(ctx, http.MethodGet, remotePath, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("HTTP 归档对象超过允许大小: %s", remotePath)
	}
	return data, nil
}

func (p *HTTPArchiveProvider) streamObject(ctx context.Context, remotePath string, target io.Writer, limit int64) (string, int64, error) {
	response, err := p.doBytes(ctx, http.MethodGet, remotePath, nil)
	if err != nil {
		return "", 0, err
	}
	defer response.Body.Close()
	hash := sha256.New()
	writer := io.MultiWriter(hash, target)
	size, err := io.Copy(writer, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", size, err
	}
	if size > limit {
		return "", size, fmt.Errorf("HTTP 归档分片超过允许大小: %s", remotePath)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), size, nil
}

func (p *HTTPArchiveProvider) deleteObject(ctx context.Context, remotePath string) error {
	response, err := p.doBytes(ctx, http.MethodDelete, remotePath, nil)
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}

func (p *HTTPArchiveProvider) doBytes(ctx context.Context, method, remotePath string, content []byte) (*http.Response, error) {
	requestURL, err := p.objectURL(remotePath)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < httpRequestAttempts; attempt++ {
		var body io.Reader
		if content != nil {
			body = bytes.NewReader(content)
		}
		request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
		if err != nil {
			return nil, err
		}
		if content != nil {
			request.Header.Set("Content-Type", "application/octet-stream")
		}
		if p.config.Token != "" {
			request.Header.Set("Authorization", "Bearer "+p.config.Token)
		} else if p.config.Username != "" {
			request.SetBasicAuth(p.config.Username, p.config.Password)
		}
		response, requestErr := p.client.Do(request)
		if requestErr != nil {
			if attempt+1 < httpRequestAttempts && retryHTTPError(requestErr, 0) {
				if err := waitHTTPRetry(ctx, attempt); err != nil {
					return nil, err
				}
				continue
			}
			return nil, requestErr
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return nil, errHTTPArchiveNotFound
		}
		if attempt+1 < httpRequestAttempts && retryHTTPError(nil, response.StatusCode) {
			if err := waitHTTPRetry(ctx, attempt); err != nil {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("HTTP 归档 %s %s 失败（HTTP %d）: %s", method, remotePath, response.StatusCode, strings.TrimSpace(string(message)))
	}
	return nil, fmt.Errorf("HTTP 归档请求重试次数耗尽: %s %s", method, remotePath)
}

func retryHTTPError(requestErr error, status int) bool {
	if requestErr != nil {
		return true
	}
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func waitHTTPRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(attempt+1) * 50 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
