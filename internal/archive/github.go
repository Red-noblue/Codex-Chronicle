// 中文说明：实现面向用户自有 GitHub 私有仓库的分片归档 provider；不保存凭证，也不触碰源 .codex。
package archive

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

const defaultGitHubChunkBytes int64 = 8 * 1024 * 1024
const maxGitHubChunkBytes int64 = 50 * 1024 * 1024

var errGitHubContentNotFound = errors.New("GitHub 归档对象不存在")

// GitHubConfig 是一次运行时 GitHub provider 配置；Token 只保存在内存中。
type GitHubConfig struct {
	BaseURL    string
	Owner      string
	Repository string
	RootPath   string
	Token      string
	Branch     string
	ProxyURL   string
	ChunkBytes int64
	ProviderID string
	HTTPClient *http.Client
}

// GitHubProvider 使用 Contents API 将一个已验证的本地归档目录上传到用户自己的仓库。
type GitHubProvider struct {
	config  GitHubConfig
	baseURL *url.URL
	client  *http.Client
}

// RemoteArchiveProvider 是远端归档 provider 的最小上传/校验边界。
// 删除、恢复和列表能力继续由具体 provider 提供；生命周期 saga 只依赖这两个前置保证。
type RemoteArchiveProvider interface {
	UploadDirectory(ctx context.Context, directory, archiveID string) (RemoteArchiveResult, error)
	VerifyArchive(ctx context.Context, archiveID string) (RemoteArchiveResult, error)
}

// RemoteArchiveProviderInfo 是 UI 和 API 识别远端 provider 的非敏感元数据。
// 它不包含令牌、密码、仓库内容或远端连接地址。
type RemoteArchiveProviderInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	TargetRequired   bool   `json:"target_required"`
	SupportsListing  bool   `json:"supports_listing"`
	SupportsRecovery bool   `json:"supports_recovery"`
	SupportsDelete   bool   `json:"supports_delete"`
}

// RemoteArchiveDescriber 允许通用 API/UI 展示 provider 身份，同时保持归档能力接口最小化。
type RemoteArchiveDescriber interface {
	ProviderInfo() RemoteArchiveProviderInfo
}

// RemoteArchiveCatalog 是远端归档审阅边界；列表和校验保持只读。
type RemoteArchiveCatalog interface {
	ListArchives(ctx context.Context) ([]GitHubArchiveSummary, error)
	VerifyArchive(ctx context.Context, archiveID string) (RemoteArchiveResult, error)
}

// RemoteArchiveRecovery 是下载远端归档到独立本地目录的能力边界。
// 恢复结果不会覆盖源 .codex；调用方应先生成并展示 dry-run 计划。
type RemoteArchiveRecovery interface {
	DownloadArchive(ctx context.Context, archiveID, destination string) (RemoteArchiveResult, error)
}

// RemoteArchiveDeletion 是远端归档的不可逆删除能力边界，默认不接入普通 UI。
type RemoteArchiveDeletion interface {
	DeleteArchive(ctx context.Context, archiveID string) error
}

// GitHubArchiveSelector 是可被 UI 运行时切换的 GitHub 归档目标边界。
// 选择只改变本地 provider 配置，不创建、删除或修改远端仓库。
type GitHubArchiveSelector interface {
	SelectedArchiveRepository(ctx context.Context) (GitHubRepositorySummary, bool, error)
	SelectArchiveRepository(ctx context.Context, owner, repository string) (GitHubRepositorySummary, error)
	ClearArchiveRepository(ctx context.Context) error
}

// GitHubRemoteManifest 是远端分片目录；其中不保存访问令牌或源正文摘要以外的派生内容。
type GitHubRemoteManifest struct {
	Version              int                `json:"version"`
	ArchiveID            string             `json:"archive_id"`
	CreatedAt            time.Time          `json:"created_at"`
	SourceManifestSHA256 string             `json:"source_manifest_sha256"`
	Files                []GitHubRemoteFile `json:"files"`
}

// GitHubRemoteFile 描述一个归档目录文件及其远端分片。
type GitHubRemoteFile struct {
	RelativePath string              `json:"relative_path"`
	SizeBytes    int64               `json:"size_bytes"`
	SHA256       string              `json:"sha256"`
	Chunks       []GitHubRemoteChunk `json:"chunks"`
}

// GitHubRemoteChunk 描述一个 Contents API 文件对象。
type GitHubRemoteChunk struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// RemoteArchiveResult 是远端归档 provider 可消费的最小技术结果。
type RemoteArchiveResult struct {
	ArchiveID    string `json:"archive_id"`
	ManifestPath string `json:"manifest_path"`
	FileCount    int    `json:"file_count"`
	ChunkCount   int    `json:"chunk_count"`
	Verified     bool   `json:"verified"`
}

// GitHubArchiveResult 保留 GitHub provider 的兼容名称；底层结果契约与其他远端 provider 相同。
type GitHubArchiveResult = RemoteArchiveResult

// GitHubArchiveSummary 是远端归档列表中的可审阅元数据，不包含归档正文。
type GitHubArchiveSummary struct {
	ArchiveID            string    `json:"archive_id"`
	ManifestPath         string    `json:"manifest_path"`
	CreatedAt            time.Time `json:"created_at"`
	FileCount            int       `json:"file_count"`
	ChunkCount           int       `json:"chunk_count"`
	SourceManifestSHA256 string    `json:"source_manifest_sha256"`
}

// ProviderInfo 返回 GitHub provider 的通用身份和能力声明。
func (p *GitHubProvider) ProviderInfo() RemoteArchiveProviderInfo {
	id := strings.TrimSpace(p.config.ProviderID)
	if id == "" {
		id = "github"
	}
	name := "GitHub"
	if id == "gitee" {
		name = "Gitee"
	}
	return RemoteArchiveProviderInfo{
		ID: id, Name: name, TargetRequired: strings.TrimSpace(p.config.Owner) == "" || strings.TrimSpace(p.config.Repository) == "",
		SupportsListing: true, SupportsRecovery: true, SupportsDelete: true,
	}
}

// NewGitHubArchiveID 生成不包含用户数据的短随机远端归档 ID。
// 调用方应持久化返回值，以便网络中断后使用同一 ID 重试。
func NewGitHubArchiveID() (string, error) {
	randomBytes := make([]byte, 12)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("生成 GitHub 归档 ID 失败: %w", err)
	}
	return "cc-" + base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

// NewGitHubProvider 创建 provider。默认 API 地址为 api.github.com，支持 HTTP/HTTPS/SOCKS5 代理。
func NewGitHubProvider(config GitHubConfig) (*GitHubProvider, error) {
	if strings.TrimSpace(config.Owner) == "" || strings.TrimSpace(config.Repository) == "" {
		return nil, fmt.Errorf("GitHub owner 和 repository 不能为空")
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, fmt.Errorf("GitHub token 不能为空")
	}
	if !validGitHubIdentifier(config.Owner) || !validGitHubIdentifier(config.Repository) {
		return nil, fmt.Errorf("GitHub owner 或 repository 含有不安全字符")
	}
	if config.RootPath != "" {
		cleanRoot, err := safeRemoteRelative(config.RootPath)
		if err != nil {
			return nil, fmt.Errorf("GitHub RootPath 无效: %w", err)
		}
		config.RootPath = cleanRoot
	}
	if config.ChunkBytes == 0 {
		config.ChunkBytes = defaultGitHubChunkBytes
	}
	if config.ChunkBytes < 1 || config.ChunkBytes > maxGitHubChunkBytes {
		return nil, fmt.Errorf("GitHub 分片大小必须在 1 到 50 MiB 之间")
	}
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("GitHub API BaseURL 无效")
	}
	client, err := newGitHubHTTPClient(config.ProxyURL, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	return &GitHubProvider{config: config, baseURL: parsed, client: client}, nil
}

func newGitHubHTTPClient(proxyValue string, custom *http.Client) (*http.Client, error) {
	var proxyURL *url.URL
	if strings.TrimSpace(proxyValue) != "" {
		parsedProxy, err := url.Parse(proxyValue)
		if err != nil {
			return nil, fmt.Errorf("GitHub provider 代理地址无效: %w", err)
		}
		if err := validateProxyURL(parsedProxy); err != nil {
			return nil, fmt.Errorf("GitHub provider 代理地址无效: %w", err)
		}
		proxyURL = parsedProxy
	}
	if custom != nil {
		return custom, nil
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("无法复制默认 HTTP transport")
	}
	transport = transport.Clone()
	if proxyURL != nil {
		switch strings.ToLower(proxyURL.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxyURL)
		case "socks5", "socks5h":
			dialer, err := newSOCKS5Dialer(proxyURL)
			if err != nil {
				return nil, fmt.Errorf("创建 SOCKS5 transport 失败: %w", err)
			}
			transport.Proxy = nil
			transport.DialContext = dialer.DialContext
		}
	}
	return &http.Client{Transport: transport}, nil
}

// NewProxyHTTPClient 为其他本地 provider 复用已验证的 HTTP/HTTPS/SOCKS5 transport。
func NewProxyHTTPClient(proxyValue string, custom *http.Client) (*http.Client, error) {
	return newGitHubHTTPClient(proxyValue, custom)
}

// UploadDirectory 上传本地归档目录。目录应先通过 CreateLocal/CreateLocalEncrypted 校验。
// 上传失败时已提交的对象会保留但不会发布远端 manifest，可安全重试同一 archiveID。
func (p *GitHubProvider) UploadDirectory(ctx context.Context, directory, archiveID string) (GitHubArchiveResult, error) {
	if p == nil {
		return GitHubArchiveResult{}, fmt.Errorf("GitHub provider 未初始化")
	}
	if strings.TrimSpace(archiveID) == "" {
		generatedID, err := NewGitHubArchiveID()
		if err != nil {
			return GitHubArchiveResult{}, err
		}
		archiveID = generatedID
	} else if err := validateArchiveID(archiveID); err != nil {
		return GitHubArchiveResult{}, err
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("解析本地归档目录失败: %w", err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("检查本地归档目录失败: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return GitHubArchiveResult{}, fmt.Errorf("本地归档路径必须是普通目录")
	}
	manifestPath := filepath.Join(root, manifestName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("读取本地归档 manifest 失败: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.Version != 1 || manifest.OperationID == "" {
		return GitHubArchiveResult{}, fmt.Errorf("本地归档 manifest 无效")
	}
	if len(manifest.Assets) == 0 {
		return GitHubArchiveResult{}, fmt.Errorf("本地归档 manifest 没有资产")
	}
	if manifest.Encrypted {
		if manifest.Encryption == nil || manifest.Encryption.KDF != encryptionKDF || manifest.Encryption.Cipher != encryptionCipher || manifest.Encryption.Iterations <= 0 {
			return GitHubArchiveResult{}, fmt.Errorf("本地加密归档参数无效")
		}
		if salt, saltErr := hex.DecodeString(manifest.Encryption.Salt); saltErr != nil || len(salt) == 0 {
			return GitHubArchiveResult{}, fmt.Errorf("本地加密归档盐无效")
		}
	}
	sourceManifestSHA256 := sha256Hex(manifestBytes)
	if err := p.checkArchiveReuse(ctx, archiveID, sourceManifestSHA256); err != nil {
		return GitHubArchiveResult{}, err
	}
	if _, err := VerifyLocal(ctx, root); err != nil && !manifest.Encrypted {
		return GitHubArchiveResult{}, fmt.Errorf("本地归档校验失败: %w", err)
	}
	if manifest.Encrypted {
		if _, err := os.Stat(manifestPath); err != nil {
			return GitHubArchiveResult{}, err
		}
	}
	files, err := regularArchiveFiles(root)
	if err != nil {
		return GitHubArchiveResult{}, err
	}
	remoteManifest := GitHubRemoteManifest{
		Version: 1, ArchiveID: archiveID, CreatedAt: manifest.CreatedAt,
		SourceManifestSHA256: sourceManifestSHA256, Files: make([]GitHubRemoteFile, 0, len(files)),
	}
	chunkCount := 0
	for _, relative := range files {
		remoteFile, count, uploadErr := p.uploadArchiveFile(ctx, root, archiveID, relative)
		if uploadErr != nil {
			return GitHubArchiveResult{}, uploadErr
		}
		remoteManifest.Files = append(remoteManifest.Files, remoteFile)
		chunkCount += count
	}
	if err := p.validateRemoteManifest(remoteManifest, archiveID); err != nil {
		return GitHubArchiveResult{}, err
	}
	data, err := json.MarshalIndent(remoteManifest, "", "  ")
	if err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("生成 GitHub 归档 manifest 失败: %w", err)
	}
	remotePath := p.remotePath(archiveID, "remote-manifest.json")
	if err := p.putContent(ctx, remotePath, append(data, '\n'), "发布 Codex Chronicle 归档 manifest"); err != nil {
		return GitHubArchiveResult{}, err
	}
	if err := p.cleanupUnreferencedArchiveObjects(ctx, archiveID, remoteManifest); err != nil {
		return GitHubArchiveResult{}, err
	}
	return GitHubArchiveResult{ArchiveID: archiveID, ManifestPath: remotePath, FileCount: len(files), ChunkCount: chunkCount, Verified: true}, nil
}

// cleanupUnreferencedArchiveObjects 清理同一 archive ID 下失败重试遗留、但未被新 manifest 引用的分片。
// 只访问当前归档的 files 子目录，不触碰仓库其他路径；manifest 已先发布，清理失败时远端归档仍可校验和重试。
func (p *GitHubProvider) cleanupUnreferencedArchiveObjects(ctx context.Context, archiveID string, manifest GitHubRemoteManifest) error {
	root := p.remotePath(archiveID, "files")
	paths, err := p.listFilesRecursive(ctx, root)
	if errors.Is(err, errGitHubContentNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("列出 GitHub 归档遗留分片失败: %w", err)
	}
	keep := make(map[string]struct{})
	for _, file := range manifest.Files {
		for _, chunk := range file.Chunks {
			keep[strings.Trim(chunk.Path, "/")] = struct{}{}
		}
	}
	for _, remotePath := range paths {
		if _, exists := keep[strings.Trim(remotePath, "/")]; exists {
			continue
		}
		if err := p.deleteContent(ctx, remotePath, "清理 Codex Chronicle 归档遗留分片"); err != nil && !errors.Is(err, errGitHubContentNotFound) {
			return err
		}
	}
	return nil
}

// checkArchiveReuse 只允许同一源 manifest 对既有 archive ID 做断点重试。
// 不同源归档必须使用新的 ID，避免旧分片和新 manifest 混合。
func (p *GitHubProvider) checkArchiveReuse(ctx context.Context, archiveID, sourceManifestSHA256 string) error {
	data, _, err := p.getContent(ctx, p.remotePath(archiveID, "remote-manifest.json"))
	if errors.Is(err, errGitHubContentNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var existing GitHubRemoteManifest
	if err := json.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("已有 GitHub 归档 manifest 无法解析，拒绝覆盖")
	}
	if err := p.validateRemoteManifest(existing, archiveID); err != nil {
		return fmt.Errorf("已有 GitHub 归档 manifest 无效，拒绝覆盖: %w", err)
	}
	if existing.SourceManifestSHA256 != sourceManifestSHA256 {
		return fmt.Errorf("archive ID %s 已绑定其他源归档，请使用新的归档 ID", archiveID)
	}
	return nil
}

// DownloadArchive 下载并重组一个远端归档；目标已存在时拒绝覆盖。
func (p *GitHubProvider) DownloadArchive(ctx context.Context, archiveID, destination string) (GitHubArchiveResult, error) {
	if p == nil {
		return GitHubArchiveResult{}, fmt.Errorf("GitHub provider 未初始化")
	}
	if err := validateArchiveID(archiveID); err != nil {
		return GitHubArchiveResult{}, err
	}
	data, _, err := p.getContent(ctx, p.remotePath(archiveID, "remote-manifest.json"))
	if err != nil {
		return GitHubArchiveResult{}, err
	}
	var remoteManifest GitHubRemoteManifest
	if err := json.Unmarshal(data, &remoteManifest); err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("GitHub 归档 manifest 无效")
	}
	if err := p.validateRemoteManifest(remoteManifest, archiveID); err != nil {
		return GitHubArchiveResult{}, err
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return GitHubArchiveResult{}, err
	}
	if info, statErr := os.Lstat(root); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return GitHubArchiveResult{}, fmt.Errorf("下载目标根必须是普通目录")
		}
	} else if os.IsNotExist(statErr) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return GitHubArchiveResult{}, fmt.Errorf("创建下载目录失败: %w", err)
		}
	} else {
		return GitHubArchiveResult{}, fmt.Errorf("检查下载目录失败: %w", statErr)
	}
	final := filepath.Join(root, archiveID)
	if _, err := os.Lstat(final); err == nil {
		return GitHubArchiveResult{}, fmt.Errorf("下载目标已存在: %s", final)
	} else if !os.IsNotExist(err) {
		return GitHubArchiveResult{}, err
	}
	staging := filepath.Join(root, "."+archiveID+".tmp")
	if err := os.Mkdir(staging, 0o700); err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("创建下载临时目录失败: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	chunkCount := 0
	for _, remoteFile := range remoteManifest.Files {
		relative, err := safeRemoteRelative(remoteFile.RelativePath)
		if err != nil {
			return GitHubArchiveResult{}, err
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if err := ensureParent(target); err != nil {
			return GitHubArchiveResult{}, err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return GitHubArchiveResult{}, fmt.Errorf("创建下载文件 %s 失败: %w", relative, err)
		}
		hash := sha256.New()
		var size int64
		for _, chunk := range remoteFile.Chunks {
			content, _, getErr := p.getContent(ctx, chunk.Path)
			if getErr != nil {
				_ = output.Close()
				return GitHubArchiveResult{}, getErr
			}
			if int64(len(content)) != chunk.SizeBytes || sha256Hex(content) != chunk.SHA256 {
				_ = output.Close()
				return GitHubArchiveResult{}, fmt.Errorf("GitHub 分片校验失败: %s", chunk.Path)
			}
			if _, err := output.Write(content); err != nil {
				_ = output.Close()
				return GitHubArchiveResult{}, fmt.Errorf("写入下载文件 %s 失败: %w", relative, err)
			}
			_, _ = hash.Write(content)
			size += int64(len(content))
			chunkCount++
		}
		if err := output.Close(); err != nil {
			return GitHubArchiveResult{}, err
		}
		if size != remoteFile.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != remoteFile.SHA256 {
			return GitHubArchiveResult{}, fmt.Errorf("GitHub 文件校验失败: %s", relative)
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(staging, manifestName))
	if err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("下载归档缺少 manifest: %w", err)
	}
	if sha256Hex(manifestBytes) != remoteManifest.SourceManifestSHA256 {
		return GitHubArchiveResult{}, fmt.Errorf("GitHub 源 manifest 摘要不匹配")
	}
	var sourceManifest Manifest
	if err := json.Unmarshal(manifestBytes, &sourceManifest); err != nil || sourceManifest.Version != 1 || sourceManifest.OperationID == "" {
		return GitHubArchiveResult{}, fmt.Errorf("下载归档的源 manifest 无效")
	}
	if !sourceManifest.Encrypted {
		if _, err := VerifyLocal(ctx, staging); err != nil {
			return GitHubArchiveResult{}, fmt.Errorf("下载归档本地校验失败: %w", err)
		}
	}
	if err := os.Rename(staging, final); err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("发布下载归档失败: %w", err)
	}
	cleanup = false
	return GitHubArchiveResult{ArchiveID: archiveID, ManifestPath: p.remotePath(archiveID, "remote-manifest.json"), FileCount: len(remoteManifest.Files), ChunkCount: chunkCount, Verified: true}, nil
}

// DeleteArchive 删除远端 manifest 和其列出的所有分片；Git 历史缓存不由该方法承诺清除。
func (p *GitHubProvider) DeleteArchive(ctx context.Context, archiveID string) error {
	if p == nil {
		return fmt.Errorf("GitHub provider 未初始化")
	}
	if err := validateArchiveID(archiveID); err != nil {
		return err
	}
	data, _, err := p.getContent(ctx, p.remotePath(archiveID, "remote-manifest.json"))
	if err != nil {
		return err
	}
	var remoteManifest GitHubRemoteManifest
	if err := json.Unmarshal(data, &remoteManifest); err != nil {
		return fmt.Errorf("GitHub 归档 manifest 无效")
	}
	if err := p.validateRemoteManifest(remoteManifest, archiveID); err != nil {
		return err
	}
	paths := make([]string, 0)
	for _, file := range remoteManifest.Files {
		for _, chunk := range file.Chunks {
			paths = append(paths, chunk.Path)
		}
	}
	paths = append(paths, p.remotePath(archiveID, "remote-manifest.json"))
	for _, remotePath := range paths {
		if err := p.deleteContent(ctx, remotePath, "删除 Codex Chronicle 归档对象"); err != nil && !errors.Is(err, errGitHubContentNotFound) {
			return err
		}
	}
	return nil
}

// ListArchives 列出 RootPath 下包含有效 remote-manifest.json 的归档目录。
// 仓库中的其他目录会被跳过，列表只返回清单元数据。
func (p *GitHubProvider) ListArchives(ctx context.Context) ([]GitHubArchiveSummary, error) {
	if p == nil {
		return nil, fmt.Errorf("GitHub provider 未初始化")
	}
	entries, err := p.listDirectory(ctx, p.config.RootPath)
	if errors.Is(err, errGitHubContentNotFound) {
		return []GitHubArchiveSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	rootPrefix := strings.Trim(p.config.RootPath, "/")
	if rootPrefix != "" {
		rootPrefix += "/"
	}
	archives := make([]GitHubArchiveSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != "dir" {
			continue
		}
		fullPath := strings.Trim(entry.Path, "/")
		if !strings.HasPrefix(fullPath, rootPrefix) {
			continue
		}
		archiveID := strings.TrimPrefix(fullPath, rootPrefix)
		if strings.Contains(archiveID, "/") || validateArchiveID(archiveID) != nil {
			continue
		}
		data, _, manifestErr := p.getContent(ctx, p.remotePath(archiveID, "remote-manifest.json"))
		if errors.Is(manifestErr, errGitHubContentNotFound) {
			continue
		}
		if manifestErr != nil {
			return nil, manifestErr
		}
		var manifest GitHubRemoteManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("解析 GitHub 归档 %s manifest 失败: %w", archiveID, err)
		}
		if err := p.validateRemoteManifest(manifest, archiveID); err != nil {
			return nil, err
		}
		chunkCount := 0
		for _, file := range manifest.Files {
			chunkCount += len(file.Chunks)
		}
		archives = append(archives, GitHubArchiveSummary{
			ArchiveID:            archiveID,
			ManifestPath:         p.remotePath(archiveID, "remote-manifest.json"),
			CreatedAt:            manifest.CreatedAt,
			FileCount:            len(manifest.Files),
			ChunkCount:           chunkCount,
			SourceManifestSHA256: manifest.SourceManifestSHA256,
		})
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].CreatedAt.Equal(archives[j].CreatedAt) {
			return archives[i].ArchiveID < archives[j].ArchiveID
		}
		return archives[i].CreatedAt.After(archives[j].CreatedAt)
	})
	return archives, nil
}

// VerifyArchive 只读取并校验远端归档，不在本地创建文件或目录。
func (p *GitHubProvider) VerifyArchive(ctx context.Context, archiveID string) (GitHubArchiveResult, error) {
	if p == nil {
		return GitHubArchiveResult{}, fmt.Errorf("GitHub provider 未初始化")
	}
	if err := validateArchiveID(archiveID); err != nil {
		return GitHubArchiveResult{}, err
	}
	data, _, err := p.getContent(ctx, p.remotePath(archiveID, "remote-manifest.json"))
	if err != nil {
		return GitHubArchiveResult{}, err
	}
	var remoteManifest GitHubRemoteManifest
	if err := json.Unmarshal(data, &remoteManifest); err != nil {
		return GitHubArchiveResult{}, fmt.Errorf("GitHub 归档 manifest 无效")
	}
	if err := p.validateRemoteManifest(remoteManifest, archiveID); err != nil {
		return GitHubArchiveResult{}, err
	}
	remoteFiles := make(map[string]GitHubRemoteFile, len(remoteManifest.Files))
	var sourceManifestBytes []byte
	chunkCount := 0
	for _, remoteFile := range remoteManifest.Files {
		hash := sha256.New()
		var size int64
		var manifestBuffer []byte
		for _, chunk := range remoteFile.Chunks {
			content, _, getErr := p.getContent(ctx, chunk.Path)
			if getErr != nil {
				return GitHubArchiveResult{}, getErr
			}
			if int64(len(content)) != chunk.SizeBytes || sha256Hex(content) != chunk.SHA256 {
				return GitHubArchiveResult{}, fmt.Errorf("GitHub 分片校验失败: %s", chunk.Path)
			}
			_, _ = hash.Write(content)
			size += int64(len(content))
			chunkCount++
			if remoteFile.RelativePath == manifestName {
				manifestBuffer = append(manifestBuffer, content...)
			}
		}
		if size != remoteFile.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != remoteFile.SHA256 {
			return GitHubArchiveResult{}, fmt.Errorf("GitHub 文件校验失败: %s", remoteFile.RelativePath)
		}
		if remoteFile.RelativePath == manifestName {
			sourceManifestBytes = manifestBuffer
		}
		remoteFiles[remoteFile.RelativePath] = remoteFile
	}
	if sha256Hex(sourceManifestBytes) != remoteManifest.SourceManifestSHA256 {
		return GitHubArchiveResult{}, fmt.Errorf("GitHub 源 manifest 摘要不匹配")
	}
	var sourceManifest Manifest
	if err := json.Unmarshal(sourceManifestBytes, &sourceManifest); err != nil || sourceManifest.Version != 1 || sourceManifest.OperationID == "" || len(sourceManifest.Assets) == 0 {
		return GitHubArchiveResult{}, fmt.Errorf("GitHub 源 manifest 无效")
	}
	if sourceManifest.Encrypted {
		if sourceManifest.Encryption == nil || sourceManifest.Encryption.KDF != encryptionKDF || sourceManifest.Encryption.Cipher != encryptionCipher || sourceManifest.Encryption.Iterations <= 0 {
			return GitHubArchiveResult{}, fmt.Errorf("GitHub 加密归档参数无效")
		}
		if salt, saltErr := hex.DecodeString(sourceManifest.Encryption.Salt); saltErr != nil || len(salt) == 0 {
			return GitHubArchiveResult{}, fmt.Errorf("GitHub 加密归档盐无效")
		}
	} else {
		for _, asset := range sourceManifest.Assets {
			relative, pathErr := safePayloadRelative(asset.RelativePath)
			if pathErr != nil {
				return GitHubArchiveResult{}, fmt.Errorf("GitHub 源 manifest 资产路径无效: %w", pathErr)
			}
			remoteFile, exists := remoteFiles["payload/"+relative]
			if !exists || remoteFile.SizeBytes != asset.SizeBytes || remoteFile.SHA256 != asset.SHA256 {
				return GitHubArchiveResult{}, fmt.Errorf("GitHub payload 与源 manifest 不匹配: %s", asset.RelativePath)
			}
		}
	}
	return GitHubArchiveResult{
		ArchiveID: archiveID, ManifestPath: p.remotePath(archiveID, "remote-manifest.json"),
		FileCount: len(remoteManifest.Files), ChunkCount: chunkCount, Verified: true,
	}, nil
}

func (p *GitHubProvider) uploadArchiveFile(ctx context.Context, root, archiveID, relative string) (GitHubRemoteFile, int, error) {
	pathOnDisk := filepath.Join(root, filepath.FromSlash(relative))
	input, err := os.Open(pathOnDisk)
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
		read, readErr := io.ReadFull(input, buffer)
		if read > 0 {
			content := buffer[:read]
			chunkPath := p.remotePath(archiveID, "files", relative+fmt.Sprintf(".part-%06d", index))
			if err := p.putContent(ctx, chunkPath, content, "上传 Codex Chronicle 归档分片"); err != nil {
				return GitHubRemoteFile{}, 0, err
			}
			reportTransfer(ctx, int64(read))
			chunkHash := sha256Hex(content)
			result.Chunks = append(result.Chunks, GitHubRemoteChunk{Path: chunkPath, SizeBytes: int64(read), SHA256: chunkHash})
			_, _ = hash.Write(content)
			total += int64(read)
			index++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return GitHubRemoteFile{}, 0, fmt.Errorf("读取归档文件 %s 失败: %w", relative, readErr)
		}
	}
	if total == 0 {
		chunkPath := p.remotePath(archiveID, "files", relative+".part-000000")
		if err := p.putContent(ctx, chunkPath, []byte{}, "上传 Codex Chronicle 空归档分片"); err != nil {
			return GitHubRemoteFile{}, 0, err
		}
		result.Chunks = append(result.Chunks, GitHubRemoteChunk{Path: chunkPath, SHA256: sha256Hex(nil)})
		index = 1
	}
	result.SizeBytes = total
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return result, index, nil
}

// validateRemoteManifest 确保远端清单只能引用当前归档目录中的安全对象。
// 清单来自远端，不能把其中的路径直接当作仓库任意路径使用。
func (p *GitHubProvider) validateRemoteManifest(manifest GitHubRemoteManifest, archiveID string) error {
	if manifest.Version != 1 || manifest.ArchiveID != archiveID {
		return fmt.Errorf("GitHub 归档 manifest 版本或归档 ID 无效")
	}
	if !validSHA256(manifest.SourceManifestSHA256) {
		return fmt.Errorf("GitHub 源 manifest 摘要无效")
	}
	if len(manifest.Files) == 0 {
		return fmt.Errorf("GitHub 归档 manifest 没有文件")
	}
	chunkRoot := p.remotePath(archiveID, "files") + "/"
	seenFiles := make(map[string]struct{}, len(manifest.Files))
	seenChunks := make(map[string]struct{})
	hasSourceManifest := false
	for _, file := range manifest.Files {
		relative, err := safeRemoteRelative(file.RelativePath)
		if err != nil || relative != file.RelativePath {
			return fmt.Errorf("GitHub 归档文件路径无效: %s", file.RelativePath)
		}
		if _, exists := seenFiles[relative]; exists {
			return fmt.Errorf("GitHub 归档包含重复文件: %s", relative)
		}
		seenFiles[relative] = struct{}{}
		if relative == manifestName {
			hasSourceManifest = true
		}
		if file.SizeBytes < 0 || !validSHA256(file.SHA256) || len(file.Chunks) == 0 {
			return fmt.Errorf("GitHub 归档文件元数据无效: %s", relative)
		}
		var chunkBytes int64
		for _, chunk := range file.Chunks {
			chunkPath, err := safeRemoteRelative(chunk.Path)
			if err != nil || chunkPath != chunk.Path || !strings.HasPrefix(chunkPath, chunkRoot) {
				return fmt.Errorf("GitHub 归档分片路径越出当前归档: %s", chunk.Path)
			}
			if _, exists := seenChunks[chunkPath]; exists {
				return fmt.Errorf("GitHub 归档包含重复分片: %s", chunkPath)
			}
			seenChunks[chunkPath] = struct{}{}
			if chunk.SizeBytes < 0 || chunk.SizeBytes > maxGitHubChunkBytes || !validSHA256(chunk.SHA256) {
				return fmt.Errorf("GitHub 归档分片元数据无效: %s", chunkPath)
			}
			if chunkBytes > file.SizeBytes-chunk.SizeBytes {
				return fmt.Errorf("GitHub 归档文件分片大小溢出: %s", relative)
			}
			chunkBytes += chunk.SizeBytes
		}
		if chunkBytes != file.SizeBytes {
			return fmt.Errorf("GitHub 归档文件分片大小不匹配: %s", relative)
		}
	}
	if !hasSourceManifest {
		return fmt.Errorf("GitHub 归档缺少源 manifest.json")
	}
	return nil
}

func (p *GitHubProvider) putContent(ctx context.Context, remotePath string, content []byte, message string) error {
	existingSHA := ""
	metadata, err := p.contentMetadata(ctx, remotePath)
	if err != nil && !errors.Is(err, errGitHubContentNotFound) {
		return err
	}
	if err == nil {
		existingSHA = metadata.SHA
		if existingSHA == gitBlobSHA(content) {
			return nil
		}
	}
	payload := map[string]any{"message": message, "content": base64.StdEncoding.EncodeToString(content)}
	if existingSHA != "" {
		payload["sha"] = existingSHA
	}
	if p.config.Branch != "" {
		payload["branch"] = p.config.Branch
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := p.newRequest(ctx, http.MethodPut, remotePath, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.doRetry(request)
	if err != nil {
		return fmt.Errorf("GitHub 上传 %s 失败: %w", remotePath, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return p.responseError(response, "上传", remotePath)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

func (p *GitHubProvider) getContent(ctx context.Context, remotePath string) ([]byte, string, error) {
	metadata, err := p.contentMetadata(ctx, remotePath)
	if err != nil {
		return nil, "", err
	}
	if metadata.Encoding == "base64" {
		content, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(metadata.Content), ""))
		if err != nil {
			return nil, "", fmt.Errorf("解码 GitHub 对象失败: %w", err)
		}
		if metadata.SHA != gitBlobSHA(content) {
			return nil, "", fmt.Errorf("GitHub 对象 SHA 不匹配: %s", remotePath)
		}
		return content, metadata.SHA, nil
	}
	if metadata.Encoding != "none" {
		return nil, "", fmt.Errorf("不支持的 GitHub 对象编码: %s", remotePath)
	}
	request, err := p.newRequest(ctx, http.MethodGet, remotePath, nil)
	if err != nil {
		return nil, "", err
	}
	query := request.URL.Query()
	// 回读后核对 blob SHA；两次请求之间的文件变化会明确失败。
	query.Set("ref", p.config.Branch)
	if p.config.Branch == "" {
		query.Del("ref")
	}
	request.URL.RawQuery = query.Encode()
	request.Header.Set("Accept", "application/vnd.github.raw+json")
	response, err := p.doRetry(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", p.responseError(response, "读取正文", remotePath)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maxGitHubChunkBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(content)) > maxGitHubChunkBytes || gitBlobSHA(content) != metadata.SHA {
		return nil, "", fmt.Errorf("GitHub 对象大小超限或读取期间发生变化: %s", remotePath)
	}
	return content, metadata.SHA, nil
}

type githubDirectoryEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

func (p *GitHubProvider) listDirectory(ctx context.Context, remotePath string) ([]githubDirectoryEntry, error) {
	request, err := p.newRequest(ctx, http.MethodGet, remotePath, nil)
	if err != nil {
		return nil, err
	}
	query := request.URL.Query()
	if p.config.Branch != "" {
		query.Set("ref", p.config.Branch)
	}
	request.URL.RawQuery = query.Encode()
	response, err := p.doRetry(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, errGitHubContentNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, p.responseError(response, "列出目录", remotePath)
	}
	var entries []githubDirectoryEntry
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&entries); err != nil {
		return nil, err
	}
	if len(entries) >= 1000 {
		return p.listTreeDirectory(ctx, remotePath)
	}
	return entries, nil
}

func (p *GitHubProvider) listFilesRecursive(ctx context.Context, remotePath string) ([]string, error) {
	entries, err := p.listDirectory(ctx, remotePath)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type == "dir" {
			nested, nestedErr := p.listFilesRecursive(ctx, entry.Path)
			if nestedErr != nil {
				return nil, nestedErr
			}
			files = append(files, nested...)
			continue
		}
		if entry.Type == "file" {
			files = append(files, entry.Path)
		}
	}
	return files, nil
}

func (p *GitHubProvider) deleteContent(ctx context.Context, remotePath, message string) error {
	metadata, err := p.contentMetadata(ctx, remotePath)
	if err != nil {
		return err
	}
	payload := map[string]any{"message": message, "sha": metadata.SHA}
	if p.config.Branch != "" {
		payload["branch"] = p.config.Branch
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := p.newRequest(ctx, http.MethodDelete, remotePath, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("GitHub 删除 %s 失败: %w", remotePath, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return errGitHubContentNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return p.responseError(response, "删除", remotePath)
	}
	return nil
}

func (p *GitHubProvider) newRequest(ctx context.Context, method, remotePath string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, p.endpoint(remotePath), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	if strings.EqualFold(strings.TrimSpace(p.config.ProviderID), "gitee") {
		request.Header.Set("Authorization", "token "+p.config.Token)
	} else {
		request.Header.Set("Authorization", "Bearer "+p.config.Token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	return request, nil
}

func (p *GitHubProvider) endpoint(remotePath string) string {
	parts := []string{strings.TrimRight(p.baseURL.String(), "/"), "repos", url.PathEscape(p.config.Owner), url.PathEscape(p.config.Repository), "contents"}
	for _, part := range strings.Split(strings.Trim(remotePath, "/"), "/") {
		if part != "" {
			parts = append(parts, url.PathEscape(part))
		}
	}
	return strings.Join(parts, "/")
}

func (p *GitHubProvider) remotePath(archiveID string, parts ...string) string {
	values := append([]string{p.config.RootPath, archiveID}, parts...)
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if strings.Trim(value, "/") == "" {
			continue
		}
		cleaned = append(cleaned, strings.Trim(value, "/"))
	}
	return strings.Join(cleaned, "/")
}

func (p *GitHubProvider) responseError(response *http.Response, action, remotePath string) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	return fmt.Errorf("GitHub %s %s 失败（HTTP %d）: %s", action, remotePath, response.StatusCode, sanitizeGitHubMessage(string(message), p.config.Token))
}

func regularArchiveFiles(root string) ([]string, error) {
	files := make([]string, 0)
	err := filepath.WalkDir(root, func(pathName string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if pathName == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("本地归档包含符号链接: %s", pathName)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, pathName)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("读取本地归档文件清单失败: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func safeRemoteRelative(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\x00") {
		return "", fmt.Errorf("远端归档相对路径为空或包含 NUL")
	}
	clean := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || filepath.VolumeName(filepath.FromSlash(clean)) != "" || (len(clean) >= 2 && clean[1] == ':') {
		return "", fmt.Errorf("远端归档路径越出根目录: %s", value)
	}
	return clean, nil
}

func validateArchiveID(value string) error {
	if strings.TrimSpace(value) == "" || value == "." || value == ".." || len(value) > 128 || !validGitHubIdentifier(value) {
		return fmt.Errorf("归档 ID 只能包含 ASCII 字母、数字、点、下划线和短横线")
	}
	return nil
}

func validGitHubIdentifier(value string) bool {
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return value != ""
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
