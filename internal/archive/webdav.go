// 中文说明：实现面向用户自有 WebDAV 目录的归档 provider；使用 manifest、SHA-256 和独立 staging 目录完成恢复。
package archive

import (
	"bytes"
	"context"
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

var errWebDAVNotFound = errors.New("WebDAV 归档对象不存在")

const webDAVMethodPropfind = "PROPFIND"

// WebDAVConfig 是一次运行时 WebDAV provider 配置；凭证只保存在内存中。
type WebDAVConfig struct {
	BaseURL    string
	RootPath   string
	Username   string
	Password   string
	Token      string
	ProxyURL   string
	HTTPClient *http.Client
}

// WebDAVProvider 使用标准 HTTP/WebDAV 方法保存归档对象。
type WebDAVProvider struct {
	config WebDAVConfig
	base   *url.URL
	client *http.Client
}

// ProviderInfo 返回 WebDAV provider 的通用身份；认证凭证不会出现在结果中。
func (p *WebDAVProvider) ProviderInfo() RemoteArchiveProviderInfo {
	return RemoteArchiveProviderInfo{
		ID: "webdav", Name: "WebDAV", TargetRequired: false,
		SupportsListing: true, SupportsRecovery: true, SupportsDelete: true,
	}
}

// NewWebDAVProvider 创建 provider；BaseURL 必须是 HTTP(S) 目录地址。
func NewWebDAVProvider(config WebDAVConfig) (*WebDAVProvider, error) {
	baseValue := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseValue == "" {
		return nil, fmt.Errorf("WebDAV BaseURL 不能为空")
	}
	base, err := url.Parse(baseValue)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("WebDAV BaseURL 无效")
	}
	if strings.TrimSpace(config.RootPath) != "" {
		root, rootErr := safeRemoteRelative(config.RootPath)
		if rootErr != nil {
			return nil, fmt.Errorf("WebDAV RootPath 无效: %w", rootErr)
		}
		config.RootPath = root
	}
	if strings.TrimSpace(config.Token) != "" && (strings.TrimSpace(config.Username) != "" || strings.TrimSpace(config.Password) != "") {
		return nil, fmt.Errorf("WebDAV 认证方式只能选择 Basic 或 Bearer 之一")
	}
	if strings.TrimSpace(config.Password) != "" && strings.TrimSpace(config.Username) == "" {
		return nil, fmt.Errorf("WebDAV Basic 认证密码不能脱离用户名")
	}
	client, err := newGitHubHTTPClient(config.ProxyURL, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	return &WebDAVProvider{config: config, base: base, client: client}, nil
}

// UploadDirectory 上传本地归档目录；manifest 最后发布，失败时可用同一 archiveID 重试。
func (p *WebDAVProvider) UploadDirectory(ctx context.Context, directory, archiveID string) (RemoteArchiveResult, error) {
	if p == nil {
		return RemoteArchiveResult{}, fmt.Errorf("WebDAV provider 未初始化")
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
	if source.Encrypted {
		if source.Encryption == nil || source.Encryption.KDF != encryptionKDF || source.Encryption.Cipher != encryptionCipher || source.Encryption.Iterations <= 0 {
			return RemoteArchiveResult{}, fmt.Errorf("本地加密归档参数无效")
		}
		if salt, saltErr := hex.DecodeString(source.Encryption.Salt); saltErr != nil || len(salt) == 0 {
			return RemoteArchiveResult{}, fmt.Errorf("本地加密归档盐无效")
		}
	} else if _, verifyErr := VerifyLocal(ctx, root); verifyErr != nil {
		return RemoteArchiveResult{}, fmt.Errorf("本地归档校验失败: %w", verifyErr)
	}
	sourceHash := sha256Hex(manifestBytes)
	previous, previousErr := p.readRemoteManifest(ctx, archiveID)
	if previousErr != nil && !errors.Is(previousErr, errWebDAVNotFound) {
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
		file, fileErr := os.Open(filepath.Join(root, filepath.FromSlash(relative)))
		if fileErr != nil {
			return RemoteArchiveResult{}, fmt.Errorf("打开归档文件 %s 失败: %w", relative, fileErr)
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return RemoteArchiveResult{}, statErr
		}
		hash := sha256.New()
		remotePath := p.objectPath(archiveID, "files/"+relative)
		putErr := p.putStream(ctx, remotePath, io.TeeReader(file, hash), info.Size())
		closeErr := file.Close()
		if putErr != nil {
			return RemoteArchiveResult{}, putErr
		}
		if closeErr != nil {
			return RemoteArchiveResult{}, closeErr
		}
		remoteManifest.Files = append(remoteManifest.Files, GitHubRemoteFile{RelativePath: relative, SizeBytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), Chunks: []GitHubRemoteChunk{{Path: remotePath, SizeBytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))}}})
		chunkCount++
	}
	encoded, err := json.MarshalIndent(remoteManifest, "", "  ")
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	manifestRemotePath := p.objectPath(archiveID, "remote-manifest.json")
	if err := p.putBytes(ctx, manifestRemotePath, append(encoded, '\n')); err != nil {
		return RemoteArchiveResult{}, err
	}
	if previous != nil {
		keep := make(map[string]struct{})
		for _, file := range remoteManifest.Files {
			for _, chunk := range file.Chunks {
				keep[chunk.Path] = struct{}{}
			}
		}
		for _, file := range previous.Files {
			for _, chunk := range file.Chunks {
				if _, ok := keep[chunk.Path]; !ok {
					_ = p.deleteObject(ctx, chunk.Path)
				}
			}
		}
	}
	if _, err := p.VerifyArchive(ctx, archiveID); err != nil {
		return RemoteArchiveResult{}, err
	}
	return RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: manifestRemotePath, FileCount: len(files), ChunkCount: chunkCount, Verified: true}, nil
}

// VerifyArchive 流式校验远端每个对象和源 manifest 摘要。
func (p *WebDAVProvider) VerifyArchive(ctx context.Context, archiveID string) (RemoteArchiveResult, error) {
	manifest, err := p.readRemoteManifest(ctx, archiveID)
	if err != nil {
		return RemoteArchiveResult{}, err
	}
	var sourceManifestBytes []byte
	remoteFiles := make(map[string]GitHubRemoteFile, len(manifest.Files))
	chunkCount := 0
	for _, file := range manifest.Files {
		fileHash := sha256.New()
		var fileSize int64
		var manifestBytes []byte
		for _, chunk := range file.Chunks {
			var target io.Writer = fileHash
			var buffer bytes.Buffer
			if file.RelativePath == manifestName {
				target = io.MultiWriter(fileHash, &buffer)
			}
			hash, size, _, streamErr := p.streamObjectTo(ctx, chunk.Path, target)
			if streamErr != nil {
				return RemoteArchiveResult{}, streamErr
			}
			if size != chunk.SizeBytes || hash != chunk.SHA256 {
				return RemoteArchiveResult{}, fmt.Errorf("WebDAV 分片校验失败: %s", chunk.Path)
			}
			fileSize += size
			if file.RelativePath == manifestName {
				manifestBytes = append(manifestBytes, buffer.Bytes()...)
			}
			chunkCount++
		}
		if fileSize != file.SizeBytes || hex.EncodeToString(fileHash.Sum(nil)) != file.SHA256 {
			return RemoteArchiveResult{}, fmt.Errorf("WebDAV 文件校验失败: %s", file.RelativePath)
		}
		if file.RelativePath == manifestName {
			sourceManifestBytes = manifestBytes
		}
		remoteFiles[file.RelativePath] = file
	}
	if sha256Hex(sourceManifestBytes) != manifest.SourceManifestSHA256 {
		return RemoteArchiveResult{}, fmt.Errorf("WebDAV 源 manifest 摘要不匹配")
	}
	var source Manifest
	if err := json.Unmarshal(sourceManifestBytes, &source); err != nil || source.Version != 1 || source.OperationID == "" || len(source.Assets) == 0 {
		return RemoteArchiveResult{}, fmt.Errorf("WebDAV 源 manifest 无效")
	}
	if source.Encrypted {
		if source.Encryption == nil || source.Encryption.KDF != encryptionKDF || source.Encryption.Cipher != encryptionCipher || source.Encryption.Iterations <= 0 {
			return RemoteArchiveResult{}, fmt.Errorf("WebDAV 加密归档参数无效")
		}
		if salt, saltErr := hex.DecodeString(source.Encryption.Salt); saltErr != nil || len(salt) == 0 {
			return RemoteArchiveResult{}, fmt.Errorf("WebDAV 加密归档盐无效")
		}
	} else {
		for _, asset := range source.Assets {
			relative, pathErr := safePayloadRelative(asset.RelativePath)
			if pathErr != nil {
				return RemoteArchiveResult{}, fmt.Errorf("WebDAV 源 manifest 资产路径无效: %w", pathErr)
			}
			remoteFile, exists := remoteFiles["payload/"+relative]
			if !exists || remoteFile.SizeBytes != asset.SizeBytes || remoteFile.SHA256 != asset.SHA256 {
				return RemoteArchiveResult{}, fmt.Errorf("WebDAV payload 与源 manifest 不匹配: %s", asset.RelativePath)
			}
		}
	}
	return RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: p.objectPath(archiveID, "remote-manifest.json"), FileCount: len(manifest.Files), ChunkCount: chunkCount, Verified: true}, nil
}

// DownloadArchive 将远端对象下载到独立目标根，目标已存在时拒绝覆盖。
func (p *WebDAVProvider) DownloadArchive(ctx context.Context, archiveID, destination string) (RemoteArchiveResult, error) {
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
	for _, file := range manifest.Files {
		relative, err := safeRemoteRelative(file.RelativePath)
		if err != nil {
			return RemoteArchiveResult{}, err
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if err := ensureParent(target); err != nil {
			return RemoteArchiveResult{}, err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return RemoteArchiveResult{}, err
		}
		hash := sha256.New()
		var size int64
		for _, chunk := range file.Chunks {
			chunkHash, chunkSize, _, streamErr := p.streamObjectTo(ctx, chunk.Path, io.MultiWriter(output, hash))
			if streamErr != nil {
				_ = output.Close()
				return RemoteArchiveResult{}, streamErr
			}
			if chunkSize != chunk.SizeBytes || chunkHash != chunk.SHA256 {
				_ = output.Close()
				return RemoteArchiveResult{}, fmt.Errorf("WebDAV 分片校验失败: %s", chunk.Path)
			}
			size += chunkSize
			chunkCount++
		}
		if closeErr := output.Close(); closeErr != nil {
			return RemoteArchiveResult{}, closeErr
		}
		if size != file.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
			return RemoteArchiveResult{}, fmt.Errorf("WebDAV 文件校验失败: %s", relative)
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(staging, manifestName))
	if err != nil {
		return RemoteArchiveResult{}, fmt.Errorf("下载归档缺少 manifest: %w", err)
	}
	if sha256Hex(manifestBytes) != manifest.SourceManifestSHA256 {
		return RemoteArchiveResult{}, fmt.Errorf("WebDAV 源 manifest 摘要不匹配")
	}
	var source Manifest
	if err := json.Unmarshal(manifestBytes, &source); err != nil || source.Version != 1 || source.OperationID == "" || len(source.Assets) == 0 {
		return RemoteArchiveResult{}, fmt.Errorf("WebDAV 源 manifest 无效")
	}
	if !source.Encrypted {
		if _, err := VerifyLocal(ctx, staging); err != nil {
			return RemoteArchiveResult{}, fmt.Errorf("WebDAV 下载归档本地校验失败: %w", err)
		}
	}
	if err := os.Rename(staging, root); err != nil {
		return RemoteArchiveResult{}, err
	}
	cleanup = false
	return RemoteArchiveResult{ArchiveID: archiveID, ManifestPath: p.objectPath(archiveID, "remote-manifest.json"), FileCount: len(manifest.Files), ChunkCount: chunkCount, Verified: true}, nil
}

// DeleteArchive 删除 manifest 及其列出的对象。
func (p *WebDAVProvider) DeleteArchive(ctx context.Context, archiveID string) error {
	manifest, err := p.readRemoteManifest(ctx, archiveID)
	if err != nil {
		return err
	}
	for _, file := range manifest.Files {
		for _, chunk := range file.Chunks {
			if err := p.deleteObject(ctx, chunk.Path); err != nil && !errors.Is(err, errWebDAVNotFound) {
				return err
			}
		}
	}
	return p.deleteObject(ctx, p.objectPath(archiveID, "remote-manifest.json"))
}

// ListArchives 通过 Depth=1 PROPFIND 找出包含 remote-manifest.json 的归档目录。
func (p *WebDAVProvider) ListArchives(ctx context.Context) ([]GitHubArchiveSummary, error) {
	request, err := http.NewRequestWithContext(ctx, webDAVMethodPropfind, p.objectPathURL(p.config.RootPath), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Depth", "1")
	p.applyAuth(request)
	response, err := p.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return []GitHubArchiveSummary{}, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("WebDAV PROPFIND 失败: HTTP %d", response.StatusCode)
	}
	var result davMultistatus
	if err := xml.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 WebDAV PROPFIND 失败: %w", err)
	}
	archives := make([]GitHubArchiveSummary, 0)
	seen := make(map[string]struct{})
	collectionURL, parseErr := url.Parse(p.objectPathURL(p.config.RootPath))
	if parseErr != nil {
		return nil, fmt.Errorf("解析 WebDAV 根目录失败: %w", parseErr)
	}
	basePath := strings.TrimRight(collectionURL.Path, "/")
	for _, item := range result.Responses {
		parsed, parseErr := url.Parse(item.Href)
		if parseErr != nil {
			continue
		}
		if !parsed.IsAbs() {
			parsed = collectionURL.ResolveReference(parsed)
		}
		itemPath := strings.TrimRight(parsed.Path, "/")
		relative := itemPath
		if basePath != "" {
			relative = strings.TrimPrefix(itemPath, basePath+"/")
		} else {
			relative = strings.TrimPrefix(itemPath, "/")
		}
		if relative == "" || strings.Contains(relative, "/") || validateArchiveID(relative) != nil {
			continue
		}
		if _, ok := seen[relative]; ok {
			continue
		}
		manifest, manifestErr := p.readRemoteManifest(ctx, relative)
		if errors.Is(manifestErr, errWebDAVNotFound) {
			continue
		}
		if manifestErr != nil {
			return nil, manifestErr
		}
		seen[relative] = struct{}{}
		chunkCount := 0
		for _, file := range manifest.Files {
			chunkCount += len(file.Chunks)
		}
		archives = append(archives, GitHubArchiveSummary{ArchiveID: relative, ManifestPath: p.objectPath(relative, "remote-manifest.json"), CreatedAt: manifest.CreatedAt, FileCount: len(manifest.Files), ChunkCount: chunkCount, SourceManifestSHA256: manifest.SourceManifestSHA256})
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].CreatedAt.After(archives[j].CreatedAt) })
	return archives, nil
}

func (p *WebDAVProvider) readRemoteManifest(ctx context.Context, archiveID string) (*GitHubRemoteManifest, error) {
	if err := validateArchiveID(archiveID); err != nil {
		return nil, err
	}
	data, err := p.getBytes(ctx, p.objectPath(archiveID, "remote-manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest GitHubRemoteManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("解析 WebDAV 归档 manifest 失败: %w", err)
	}
	if err := p.validateWebDAVManifest(manifest, archiveID); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (p *WebDAVProvider) validateWebDAVManifest(manifest GitHubRemoteManifest, archiveID string) error {
	if manifest.Version != 1 || manifest.ArchiveID != archiveID || len(manifest.Files) == 0 || !validSHA256(manifest.SourceManifestSHA256) {
		return fmt.Errorf("WebDAV 归档 manifest 无效")
	}
	chunkRoot := p.objectPath(archiveID, "files") + "/"
	seenFiles := make(map[string]struct{}, len(manifest.Files))
	seenChunks := make(map[string]struct{})
	hasSourceManifest := false
	for _, file := range manifest.Files {
		relative, relativeErr := safeRemoteRelative(file.RelativePath)
		if relativeErr != nil || relative != file.RelativePath || len(file.Chunks) == 0 || file.SizeBytes < 0 || !validSHA256(file.SHA256) {
			return fmt.Errorf("WebDAV 归档文件清单无效: %s", file.RelativePath)
		}
		if _, exists := seenFiles[relative]; exists {
			return fmt.Errorf("WebDAV 归档包含重复文件: %s", relative)
		}
		seenFiles[relative] = struct{}{}
		if relative == manifestName {
			hasSourceManifest = true
		}
		var chunkBytes int64
		for _, chunk := range file.Chunks {
			chunkPath, chunkErr := safeRemoteRelative(chunk.Path)
			if chunkErr != nil || chunkPath != chunk.Path || !strings.HasPrefix(chunkPath, chunkRoot) || chunk.SizeBytes < 0 || !validSHA256(chunk.SHA256) {
				return fmt.Errorf("WebDAV 分片清单无效: %s", chunk.Path)
			}
			if _, exists := seenChunks[chunkPath]; exists {
				return fmt.Errorf("WebDAV 归档包含重复分片: %s", chunkPath)
			}
			seenChunks[chunkPath] = struct{}{}
			if chunkBytes > file.SizeBytes-chunk.SizeBytes {
				return fmt.Errorf("WebDAV 文件分片大小溢出: %s", file.RelativePath)
			}
			chunkBytes += chunk.SizeBytes
		}
		if chunkBytes != file.SizeBytes {
			return fmt.Errorf("WebDAV 文件分片大小不匹配: %s", file.RelativePath)
		}
	}
	if !hasSourceManifest {
		return fmt.Errorf("WebDAV 归档缺少源 manifest.json")
	}
	return nil
}

func (p *WebDAVProvider) objectPath(archiveID, relative string) string {
	parts := []string{strings.Trim(p.config.RootPath, "/"), strings.Trim(archiveID, "/"), strings.Trim(relative, "/")}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			values = append(values, part)
		}
	}
	return strings.Join(values, "/")
}

func (p *WebDAVProvider) objectPathURL(relative string) string {
	copyURL := *p.base
	pathValue := strings.TrimRight(copyURL.Path, "/")
	if strings.Trim(relative, "/") != "" {
		pathValue += "/" + strings.Trim(relative, "/")
	}
	copyURL.Path = pathValue
	copyURL.RawPath = ""
	return copyURL.String()
}

func (p *WebDAVProvider) applyAuth(request *http.Request) {
	if p.config.Token != "" {
		request.Header.Set("Authorization", "Bearer "+p.config.Token)
	} else if p.config.Username != "" {
		request.SetBasicAuth(p.config.Username, p.config.Password)
	}
}

func (p *WebDAVProvider) putBytes(ctx context.Context, relative string, content []byte) error {
	return p.putStream(ctx, relative, bytes.NewReader(content), int64(len(content)))
}

func (p *WebDAVProvider) putStream(ctx context.Context, relative string, body io.Reader, size int64) error {
	if err := p.ensureRemoteParent(ctx, relative); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, p.objectPathURL(relative), body)
	if err != nil {
		return err
	}
	request.ContentLength = size
	p.applyAuth(request)
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("WebDAV PUT 失败: HTTP %d", response.StatusCode)
	}
	return nil
}

func (p *WebDAVProvider) getBytes(ctx context.Context, relative string) ([]byte, error) {
	_, _, content, err := p.streamObject(ctx, relative, true)
	if err != nil {
		return nil, err
	}
	return content, nil
}

func (p *WebDAVProvider) streamObject(ctx context.Context, relative string, capture bool) (string, int64, []byte, error) {
	var buffer []byte
	var target io.Writer = io.Discard
	if capture {
		target = &byteBuffer{value: &buffer}
	}
	hash, size, _, err := p.streamObjectTo(ctx, relative, target)
	return hash, size, buffer, err
}

func (p *WebDAVProvider) streamObjectTo(ctx context.Context, relative string, target io.Writer) (string, int64, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.objectPathURL(relative), nil)
	if err != nil {
		return "", 0, nil, err
	}
	p.applyAuth(request)
	response, err := p.client.Do(request)
	if err != nil {
		return "", 0, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", 0, nil, errWebDAVNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", 0, nil, fmt.Errorf("WebDAV GET 失败: HTTP %d", response.StatusCode)
	}
	hash := sha256.New()
	writer := io.MultiWriter(target, hash)
	size, err := io.Copy(writer, response.Body)
	return hex.EncodeToString(hash.Sum(nil)), size, nil, err
}

func (p *WebDAVProvider) deleteObject(ctx context.Context, relative string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.objectPathURL(relative), nil)
	if err != nil {
		return err
	}
	p.applyAuth(request)
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return errWebDAVNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("WebDAV DELETE 失败: HTTP %d", response.StatusCode)
	}
	return nil
}

// ensureRemoteParent 尝试创建对象的远端父目录；已存在或服务器不支持 MKCOL 时继续交给 PUT 处理。
func (p *WebDAVProvider) ensureRemoteParent(ctx context.Context, relative string) error {
	clean, err := safeRemoteRelative(relative)
	if err != nil {
		return err
	}
	parts := strings.Split(clean, "/")
	if len(parts) < 2 {
		return nil
	}
	current := make([]string, 0, len(parts)-1)
	for _, part := range parts[:len(parts)-1] {
		current = append(current, part)
		request, requestErr := http.NewRequestWithContext(ctx, "MKCOL", p.objectPathURL(strings.Join(current, "/")), nil)
		if requestErr != nil {
			return requestErr
		}
		p.applyAuth(request)
		response, requestErr := p.client.Do(request)
		if requestErr != nil {
			return requestErr
		}
		response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode == http.StatusMethodNotAllowed {
			continue
		}
		return fmt.Errorf("WebDAV MKCOL 失败: HTTP %d", response.StatusCode)
	}
	return nil
}

type byteBuffer struct{ value *[]byte }

func (b *byteBuffer) Write(value []byte) (int, error) {
	*b.value = append(*b.value, value...)
	return len(value), nil
}

type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href string `xml:"href"`
}

func validateArchiveDirectory(directory string) (string, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("本地归档路径必须是普通目录")
	}
	return root, nil
}

func prepareDownloadRoot(destination, archiveID string) (string, error) {
	root, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Lstat(root); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("下载目标根必须是普通目录")
		}
	} else if os.IsNotExist(statErr) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
	} else {
		return "", statErr
	}
	final := filepath.Join(root, archiveID)
	if _, err := os.Lstat(final); err == nil {
		return "", fmt.Errorf("下载目标已存在: %s", final)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return final, nil
}
