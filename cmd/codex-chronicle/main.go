// 中文说明：Codex Chronicle 本地 daemon 入口；默认只读，显式区分 fixture 写入与四 ID 受限真实废纸篓执行。
package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"codex-chronicle.local/internal/api"
	archivepkg "codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/auth"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/compat/authorizedlive"
	"codex-chronicle.local/internal/desktop"
	"codex-chronicle.local/internal/platform"
	resourcepkg "codex-chronicle.local/internal/resource"
	"codex-chronicle.local/internal/store"
	summarypkg "codex-chronicle.local/internal/summary"
	updatepkg "codex-chronicle.local/internal/update"
)

var version = "0.0.0-dev"

// githubOAuthClientID 是维护者公开 OAuth App 的构建配置：go build -ldflags "-X main.githubOAuthClientID=公开ClientID"。
// 内置项目已注册的公开标识；用户访问令牌仍由各自的系统凭证库保存。
var githubOAuthClientID = "Ov23liLNhUPunXpkx5f0"

func main() {
	desktopStateDir := flag.String("desktop-state-dir", "", "安装版私有实例目录；由桌面启动器设置")
	desktopStartPaused := flag.Bool("desktop-start-paused", false, "安装器启动候选版本，确认前只开放私有控制通道和健康检查")
	desktopAutoPort := flag.Bool("desktop-auto-port", false, "启动器专用：首选端口被占用时自动绑定本机空闲端口")
	desktopNoBrowser := flag.Bool("desktop-no-browser", false, "启动器专用：更新重启后保持无浏览器、无托盘运行方式")
	address := flag.String("address", "127.0.0.1:8787", "本地健康检查监听地址")
	allowRemote := flag.Bool("allow-remote", false, "允许非回环监听；启动后必须通过一次性绑定码建立会话")
	authStatePath := flag.String("auth-state", defaultAuthStatePath(), "远程认证状态文件；必须位于 .codex 外部")
	authBindingCode := flag.String("auth-binding-code", "", "可选的一次性初始绑定码；不提供时启动生成并仅打印一次")
	authToken := flag.String("auth-token", "", "兼容旧参数：作为一次性初始绑定码使用，不再作为长期 Bearer 令牌")
	scanRoot := flag.String("scan-root", "", "首次使用的只读扫描根；省略时沿用工作区选择，或识别 CODEX_HOME 和用户 .codex")
	installRoot := flag.String("install-root", "", "独立安装版本根目录；为空时不开放离线更新控制面，不能位于 .codex 或扫描根内")
	webRoot := flag.String("web-root", "", "已构建 Svelte 工作台的静态目录；为空时只提供 API")
	updateRequireSignature := flag.Bool("update-require-signature", false, "离线更新必须通过 Ed25519 清单签名校验")
	updateTrustedKeyFile := flag.String("update-trusted-public-key-file", "", "离线更新信任公钥文件；每行一个 Base64 或十六进制 Ed25519 公钥")
	updateFeedURL := flag.String("update-feed-url", "", "在线更新 JSON feed 地址；必须是 HTTP/HTTPS")
	updateProxyURL := flag.String("update-proxy-url", "", "在线更新 HTTP/HTTPS/SOCKS5 代理地址")
	updateMaxDownloadBytes := flag.Int64("update-max-download-bytes", 0, "在线更新包最大下载字节数；0 使用默认值")
	archivePath := flag.String("archive-db", "", "独立 archive.db 路径；为空时不启用 catalog sync")
	workspaceConfig := flag.String("workspace-config", "", "工作区注册表 JSON 路径；省略时自动保存到独立 archive.db 旁或用户配置目录")
	trashSettingsPath := flag.String("trash-settings", "", "独立回收站设置文件；为空使用用户配置目录，实验时可指定项目内路径")
	summaryProvider := flag.String("summary-provider", "local-deterministic", "摘要 provider：local-deterministic 或 openai-compatible")
	summaryBaseURL := flag.String("summary-base-url", "", "OpenAI-compatible 摘要 BaseURL；仅在显式选择云端 provider 时使用")
	summaryModel := flag.String("summary-model", "", "OpenAI-compatible 摘要模型名；仅在显式选择云端 provider 时使用")
	summaryAPIKey := flag.String("summary-api-key", os.Getenv("CODEX_CHRONICLE_SUMMARY_API_KEY"), "OpenAI-compatible 摘要 API key；仅保存在本次进程内，也可使用环境变量")
	summaryProxy := flag.String("summary-proxy", "", "云端摘要 HTTP/HTTPS 代理地址")
	summaryMaxOutputTokens := flag.Int("summary-max-output-tokens", 700, "云端摘要单个响应的最大输出 tokens")
	memoryLimitBytes := flag.Int64("memory-limit-bytes", resourcepkg.DefaultMemoryLimitBytes, "本项目运行时堆内存预算；0 使用默认值")
	persistentLimitBytes := flag.Int64("persistent-limit-bytes", resourcepkg.DefaultPersistentLimitBytes, "本项目派生持久数据空间预算；0 使用默认值")
	temporaryLimitBytes := flag.Int64("temporary-limit-bytes", resourcepkg.DefaultTemporaryLimitBytes, "本项目运行时临时空间预算；0 使用默认值")
	fixtureWrite := flag.Bool("fixture-write", false, "仅对明确的合成/副本根目录启用生命周期写执行；拒绝真实 .codex")
	authorizedRealTrash := flag.Bool("authorized-real-trash", false, "旧合成 ID 兼容模式；真实闲置会话请显式启用 --real-conversation-write")
	realConversationWrite := flag.Bool("real-conversation-write", false, "对本机已适配的闲置会话开放前端确认后的原生回收站和永久删除；活动或未知关联仍阻断")
	authorizedPython := flag.String("authorized-python", "/usr/bin/python3", "受限执行器使用的 Python 3 绝对路径")
	authorizedBridge := flag.String("authorized-trash-bridge", "", "已构建原生废纸篓 bridge 的绝对路径")
	authorizedSocket := flag.String("authorized-daemon-socket", "", "当前 Codex daemon Unix socket；必须能核对 loaded/list")
	realCopyWrite := flag.Bool("real-copy-write", false, "在 --fixture-write 下选择 codex-real-copy-v1 副本 adapter；拒绝未标记根目录和真实 .codex")
	archiveRoot := flag.String("archive-root", "", "本地归档目录；真实执行默认使用用户 Documents/Codex Chronicle/archives")
	journalRoot := flag.String("journal-root", "", "fixture 或受限真实执行的独立 journal 目录；必须位于源外")
	trashRoot := flag.String("trash-root", "", "fixture 写模式的隔离回收站目录")
	cleanupRoot := flag.String("cleanup-root", "", "fixture 写模式的生成物 cleanup registry 目录；为空时使用 journal-root/cleanup-registry")
	cleanupScanInterval := flag.String("cleanup-scan-interval", "0", "只读清理候选刷新间隔，例如 15m；0 不启动后台刷新")
	systemTrash := flag.Bool("system-trash", false, "fixture 写模式显式使用当前平台原生回收站")
	githubClientID := flag.String("github-oauth-client-id", githubOAuthClientID, "覆盖发行包内置的 GitHub 公开 Client ID；为空时不启用设备授权")
	githubBaseURL := flag.String("github-oauth-base-url", "https://github.com", "GitHub OAuth 地址；企业 GitHub 可替换")
	githubAPIBaseURL := flag.String("github-api-base-url", "https://api.github.com", "GitHub API 地址；企业 GitHub 可替换")
	githubScope := flag.String("github-oauth-scope", "repo", "GitHub OAuth scope")
	githubProxyURL := flag.String("github-proxy-url", "", "GitHub OAuth HTTP/HTTPS/SOCKS5 代理地址")
	githubAccount := flag.String("github-account", "default", "本地凭证库中的 GitHub 账户名")
	githubOwner := flag.String("github-archive-owner", "", "远端 GitHub 归档仓库 owner；为空时等待 UI 选择")
	githubRepository := flag.String("github-archive-repository", "", "远端 GitHub 归档仓库名")
	githubSelectionPath := flag.String("github-archive-selection", "", "GitHub 归档仓库选择文件；为空使用当前用户配置目录")
	githubSettingsPath := flag.String("github-settings", "", "GitHub 公开连接设置文件；为空使用当前用户配置目录，支持在设置页保存")
	githubRecoveryRoot := flag.String("github-recovery-root", "", "远端 GitHub 归档下载的独立本地根目录")
	githubArchiveRoot := flag.String("github-archive-root", "codex-chronicle", "远端 GitHub 归档根路径")
	githubBranch := flag.String("github-archive-branch", "", "远端 GitHub 归档分支；为空使用默认分支")
	githubChunkBytes := flag.Int64("github-archive-chunk-bytes", 0, "远端 GitHub 分片字节数，最大 50 MiB")
	giteeBaseURL := flag.String("gitee-api-base-url", "https://gitee.com/api/v5", "Gitee Contents API 地址")
	giteeOwner := flag.String("gitee-archive-owner", "", "远端 Gitee 归档仓库 owner")
	giteeRepository := flag.String("gitee-archive-repository", "", "远端 Gitee 归档仓库名")
	giteeToken := flag.String("gitee-token", "", "Gitee 访问令牌；只在本次进程内使用")
	giteeArchiveRoot := flag.String("gitee-archive-root", "codex-chronicle", "远端 Gitee 归档根路径")
	giteeBranch := flag.String("gitee-archive-branch", "", "远端 Gitee 归档分支；为空使用默认分支")
	giteeChunkBytes := flag.Int64("gitee-archive-chunk-bytes", 0, "远端 Gitee 分片字节数，最大 50 MiB")
	giteeProxyURL := flag.String("gitee-proxy-url", "", "Gitee HTTP/HTTPS/SOCKS5 代理地址")
	webDAVBaseURL := flag.String("webdav-base-url", "", "用户自有 WebDAV 服务目录地址；为空不启用 WebDAV 归档")
	webDAVRoot := flag.String("webdav-root", "codex-chronicle", "WebDAV 归档根路径")
	webDAVUsername := flag.String("webdav-username", "", "WebDAV Basic 认证用户名；凭证仅在本次进程内使用")
	webDAVPassword := flag.String("webdav-password", "", "WebDAV Basic 认证密码；不写入配置和日志")
	webDAVToken := flag.String("webdav-token", "", "WebDAV Bearer 令牌；不写入配置和日志")
	webDAVProxy := flag.String("webdav-proxy", "", "WebDAV HTTP/HTTPS/SOCKS5 代理地址")
	httpArchiveBaseURL := flag.String("http-archive-base-url", "", "自定义 HTTP 对象归档 BaseURL；为空不启用")
	httpArchiveRoot := flag.String("http-archive-root", "codex-chronicle", "自定义 HTTP 归档对象根路径")
	httpArchiveIndex := flag.String("http-archive-index", "archives.json", "自定义 HTTP 归档索引文件名")
	httpArchiveUsername := flag.String("http-archive-username", "", "自定义 HTTP Basic 用户名；凭证仅在本次进程内使用")
	httpArchivePassword := flag.String("http-archive-password", "", "自定义 HTTP Basic 密码；不写入配置和日志")
	httpArchiveToken := flag.String("http-archive-token", "", "自定义 HTTP Bearer 令牌；不写入配置和日志")
	httpArchiveProxy := flag.String("http-archive-proxy", "", "自定义 HTTP 归档 HTTP/HTTPS/SOCKS5 代理地址")
	httpArchiveChunkBytes := flag.Int64("http-archive-chunk-bytes", 0, "自定义 HTTP 归档逻辑分片字节数，最大 50 MiB")
	s3Endpoint := flag.String("s3-endpoint", "", "S3 或 S3-compatible endpoint；为空使用 AWS 区域 endpoint")
	s3Region := flag.String("s3-region", "us-east-1", "S3 region")
	s3Bucket := flag.String("s3-bucket", "", "S3 bucket")
	s3Prefix := flag.String("s3-prefix", "codex-chronicle", "S3 归档对象前缀")
	s3AccessKeyID := flag.String("s3-access-key-id", "", "S3 access key ID；只在本次进程内使用")
	s3SecretAccessKey := flag.String("s3-secret-access-key", "", "S3 secret access key；不写入配置和日志")
	s3SessionToken := flag.String("s3-session-token", "", "S3 临时会话令牌；不写入配置和日志")
	s3PathStyle := flag.Bool("s3-path-style", true, "使用 S3 path-style 地址；MinIO、Ceph 和自定义 endpoint 通常需要")
	s3ChunkBytes := flag.Int64("s3-chunk-bytes", 0, "S3 逻辑分片字节数，最大 50 MiB")
	s3Proxy := flag.String("s3-proxy", "", "S3 HTTP/HTTPS/SOCKS5 代理地址")
	remoteRecoveryRoot := flag.String("remote-recovery-root", "", "所有远端 provider 下载恢复使用的独立本地根目录")
	printVersion := flag.Bool("version", false, "输出版本并退出")
	flag.Parse()
	if *printVersion {
		_, _ = os.Stdout.WriteString(version + "\n")
		return
	}
	var desktopLease *desktop.Lease
	if strings.TrimSpace(*desktopStateDir) != "" {
		if !isLoopbackListenAddress(*address) || *allowRemote {
			log.Fatal("桌面启动器只支持本机回环访问")
		}
		var err error
		desktopLease, err = desktop.Acquire(*desktopStateDir)
		if err != nil {
			log.Fatal(err)
		}
		defer desktopLease.Close()
		desktopLease.SetStartupPaused(*desktopStartPaused)
	}
	if *desktopAutoPort && desktopLease == nil {
		log.Fatal("--desktop-auto-port 只能由安装版启动器配合 --desktop-state-dir 使用")
	}
	if (*fixtureWrite || *authorizedRealTrash || *realConversationWrite) && strings.TrimSpace(*scanRoot) == "" {
		log.Fatal("写执行模式必须显式指定 --scan-root，不使用自动发现的目录")
	}
	explicitRoot := *scanRoot
	resolvedRoot, resolvedConfig, err := resolveWorkspaceStartup(context.Background(), *scanRoot, *archivePath, *workspaceConfig)
	if err != nil {
		log.Fatal(err)
	}
	if *fixtureWrite || *authorizedRealTrash || *realConversationWrite {
		explicitAbs, err := filepath.Abs(explicitRoot)
		if err != nil || explicitAbs != resolvedRoot {
			log.Fatal("工作区已保存的读取目录与显式写执行根不一致，请先使用只读模式核对选择")
		}
	}
	*scanRoot, *workspaceConfig = resolvedRoot, resolvedConfig
	cleanupInterval, err := time.ParseDuration(strings.TrimSpace(*cleanupScanInterval))
	if err != nil || cleanupInterval < 0 {
		log.Fatal("--cleanup-scan-interval 必须是非负 duration，例如 15m 或 0")
	}
	if *realCopyWrite && !*fixtureWrite {
		log.Fatal("--real-copy-write 必须与 --fixture-write 同时使用")
	}
	if (*authorizedRealTrash || *realConversationWrite) && (*fixtureWrite || strings.TrimSpace(*journalRoot) == "") {
		log.Fatal("真实会话执行必须使用独立 --journal-root，不能与 --fixture-write 同时开启")
	}
	if *authorizedRealTrash && *realConversationWrite {
		log.Fatal("--authorized-real-trash 与 --real-conversation-write 只能选择一个")
	}
	if strings.TrimSpace(*installRoot) == "" && (strings.TrimSpace(*updateFeedURL) != "" || strings.TrimSpace(*updateProxyURL) != "" || *updateMaxDownloadBytes != 0) {
		log.Fatal("在线更新配置必须与 --install-root 同时使用")
	}
	if !isLoopbackListenAddress(*address) {
		if !*allowRemote {
			log.Fatal("非回环监听默认禁止；如确需远程访问，请使用 --allow-remote，并置于 TLS 反向代理后")
		}
	}
	if *allowRemote {
		if strings.TrimSpace(*authBindingCode) != "" && strings.TrimSpace(*authToken) != "" {
			log.Fatal("--auth-binding-code 与兼容参数 --auth-token 不能同时使用")
		}
		if authStatePathContainsCodex(*authStatePath) || (strings.TrimSpace(*scanRoot) != "" && pathIsWithin(*scanRoot, *authStatePath)) {
			log.Fatal("认证状态文件必须位于真实 .codex 和扫描根之外")
		}
	}

	var archive *store.Store
	if strings.TrimSpace(*archivePath) != "" {
		var err error
		archive, err = store.Open(context.Background(), *archivePath)
		if err != nil {
			log.Fatal(err)
		}
		defer archive.Close()
	}
	ownerConfigured := strings.TrimSpace(*githubOwner) != ""
	repositoryConfigured := strings.TrimSpace(*githubRepository) != ""
	clientIDExplicit := false
	flag.Visit(func(option *flag.Flag) {
		if option.Name == "github-oauth-client-id" {
			clientIDExplicit = true
		}
	})
	var githubAuth *archivepkg.GitHubDeviceAuthService
	// 发行包内置应用由运行设置延迟接入，不能因此覆盖用户选择的 Gitee、S3 等其他远端服务。
	if strings.TrimSpace(*githubClientID) != "" && (clientIDExplicit || ownerConfigured || repositoryConfigured) {
		var err error
		credentialStore, storeErr := platform.NewSystemGitHubTokenStore("")
		if storeErr != nil {
			log.Fatal(storeErr)
		}
		githubAuth, err = archivepkg.NewGitHubDeviceAuthService(archivepkg.GitHubDeviceAuthConfig{
			BaseURL: *githubBaseURL, ClientID: *githubClientID, Scope: *githubScope, ProxyURL: *githubProxyURL,
		}, credentialStore, *githubAccount)
		if err != nil {
			log.Fatal(err)
		}
		defer githubAuth.Close()
	}
	if ownerConfigured != repositoryConfigured {
		log.Fatal("配置 GitHub 远端归档时必须同时配置 --github-archive-owner 和 --github-archive-repository")
	}
	if (ownerConfigured || repositoryConfigured) && githubAuth == nil {
		log.Fatal("配置 GitHub 远端归档时必须同时配置 --github-oauth-client-id")
	}
	giteeOwnerConfigured := strings.TrimSpace(*giteeOwner) != ""
	giteeRepositoryConfigured := strings.TrimSpace(*giteeRepository) != ""
	giteeTokenConfigured := strings.TrimSpace(*giteeToken) != ""
	giteeOptionsConfigured := giteeOwnerConfigured || giteeRepositoryConfigured || giteeTokenConfigured || strings.TrimSpace(*giteeProxyURL) != "" || strings.TrimSpace(*giteeArchiveRoot) != "codex-chronicle" || strings.TrimSpace(*giteeBranch) != "" || *giteeChunkBytes != 0 || strings.TrimSpace(*giteeBaseURL) != "https://gitee.com/api/v5"
	if giteeOptionsConfigured && (!giteeOwnerConfigured || !giteeRepositoryConfigured || !giteeTokenConfigured) {
		log.Fatal("配置 Gitee 远端归档时必须同时配置 --gitee-archive-owner、--gitee-archive-repository 和 --gitee-token")
	}
	if giteeOptionsConfigured && githubAuth != nil {
		log.Fatal("GitHub 和 Gitee 归档 provider 当前不能同时启用")
	}
	s3OptionsConfigured := strings.TrimSpace(*s3Endpoint) != "" || strings.TrimSpace(*s3Bucket) != "" || strings.TrimSpace(*s3AccessKeyID) != "" || strings.TrimSpace(*s3SecretAccessKey) != "" || strings.TrimSpace(*s3SessionToken) != "" || strings.TrimSpace(*s3Proxy) != "" || strings.TrimSpace(*s3Region) != "us-east-1" || strings.TrimSpace(*s3Prefix) != "codex-chronicle" || *s3ChunkBytes != 0 || !*s3PathStyle
	if s3OptionsConfigured && (strings.TrimSpace(*s3Bucket) == "" || strings.TrimSpace(*s3AccessKeyID) == "" || strings.TrimSpace(*s3SecretAccessKey) == "") {
		log.Fatal("配置 S3 远端归档时必须同时配置 --s3-bucket、--s3-access-key-id 和 --s3-secret-access-key")
	}
	if s3OptionsConfigured && (githubAuth != nil || giteeOptionsConfigured) {
		log.Fatal("GitHub、Gitee 和 S3 归档 provider 当前不能同时启用")
	}
	var remoteArchive archivepkg.RemoteArchiveProvider
	var remoteArchiveCatalog archivepkg.RemoteArchiveCatalog
	var githubRepositories archivepkg.GitHubRepositoryCatalog
	if githubAuth != nil {
		repositoryClient, err := archivepkg.NewGitHubCredentialRepositoryClient(githubAuth, archivepkg.GitHubRepositoryClientConfig{
			BaseURL: *githubAPIBaseURL, ProxyURL: *githubProxyURL,
		})
		if err != nil {
			log.Fatal(err)
		}
		githubRepositories = repositoryClient
		selectionPath := strings.TrimSpace(*githubSelectionPath)
		if selectionPath == "" {
			if configDir, configErr := os.UserConfigDir(); configErr == nil {
				selectionPath = filepath.Join(configDir, "Codex Chronicle", "github-archive-selection.json")
			}
		}
		var selectionStore archivepkg.GitHubArchiveSelectionStore
		if selectionPath != "" {
			selectionStore, err = archivepkg.NewFileGitHubArchiveSelectionStore(selectionPath)
			if err != nil {
				log.Fatal(err)
			}
		}
		provider, err := archivepkg.NewGitHubCredentialArchiveProviderWithSelectionStore(githubAuth, archivepkg.GitHubConfig{
			BaseURL: *githubAPIBaseURL, Owner: *githubOwner, Repository: *githubRepository, RootPath: *githubArchiveRoot,
			Branch: *githubBranch, ChunkBytes: *githubChunkBytes, ProxyURL: *githubProxyURL,
		}, selectionStore)
		if err != nil {
			log.Fatal(err)
		}
		remoteArchive = provider
		remoteArchiveCatalog = provider
	}
	if giteeOptionsConfigured {
		provider, err := archivepkg.NewGiteeProvider(archivepkg.GiteeConfig{
			BaseURL: *giteeBaseURL, Owner: *giteeOwner, Repository: *giteeRepository, Token: *giteeToken,
			RootPath: *giteeArchiveRoot, Branch: *giteeBranch, ChunkBytes: *giteeChunkBytes, ProxyURL: *giteeProxyURL,
		})
		if err != nil {
			log.Fatal(err)
		}
		remoteArchive = provider
		remoteArchiveCatalog = provider
	}
	if s3OptionsConfigured {
		provider, err := archivepkg.NewS3Provider(archivepkg.S3Config{
			Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket, Prefix: *s3Prefix,
			AccessKeyID: *s3AccessKeyID, SecretAccessKey: *s3SecretAccessKey, SessionToken: *s3SessionToken,
			PathStyle: *s3PathStyle, ChunkBytes: *s3ChunkBytes, ProxyURL: *s3Proxy,
		})
		if err != nil {
			log.Fatal(err)
		}
		remoteArchive = provider
		remoteArchiveCatalog = provider
	}
	webDAVConfigured := strings.TrimSpace(*webDAVBaseURL) != ""
	if !webDAVConfigured && (strings.TrimSpace(*webDAVUsername) != "" || strings.TrimSpace(*webDAVPassword) != "" || strings.TrimSpace(*webDAVToken) != "" || strings.TrimSpace(*webDAVProxy) != "") {
		log.Fatal("配置 WebDAV 凭证或代理时必须同时配置 --webdav-base-url")
	}
	if webDAVConfigured {
		if githubAuth != nil || remoteArchive != nil {
			log.Fatal("GitHub、Gitee、S3 和 WebDAV 归档 provider 当前不能同时启用")
		}
		provider, err := archivepkg.NewWebDAVProvider(archivepkg.WebDAVConfig{
			BaseURL: *webDAVBaseURL, RootPath: *webDAVRoot,
			Username: *webDAVUsername, Password: *webDAVPassword, Token: *webDAVToken,
			ProxyURL: *webDAVProxy,
		})
		if err != nil {
			log.Fatal(err)
		}
		remoteArchive = provider
		remoteArchiveCatalog = provider
	}
	httpArchiveOptionsConfigured := strings.TrimSpace(*httpArchiveBaseURL) != "" || strings.TrimSpace(*httpArchiveUsername) != "" || strings.TrimSpace(*httpArchivePassword) != "" || strings.TrimSpace(*httpArchiveToken) != "" || strings.TrimSpace(*httpArchiveProxy) != "" || strings.TrimSpace(*httpArchiveRoot) != "codex-chronicle" || strings.TrimSpace(*httpArchiveIndex) != "archives.json" || *httpArchiveChunkBytes != 0
	if httpArchiveOptionsConfigured {
		if githubAuth != nil || giteeOptionsConfigured || s3OptionsConfigured || webDAVConfigured {
			log.Fatal("GitHub、Gitee、S3、WebDAV 和自定义 HTTP 归档 provider 当前不能同时启用")
		}
		provider, err := archivepkg.NewHTTPArchiveProvider(archivepkg.HTTPArchiveConfig{
			BaseURL: *httpArchiveBaseURL, RootPath: *httpArchiveRoot, IndexPath: *httpArchiveIndex,
			Username: *httpArchiveUsername, Password: *httpArchivePassword, Token: *httpArchiveToken,
			ProxyURL: *httpArchiveProxy, ChunkBytes: *httpArchiveChunkBytes,
		})
		if err != nil {
			log.Fatal(err)
		}
		remoteArchive = provider
		remoteArchiveCatalog = provider
	}
	recoveryRoot := strings.TrimSpace(*remoteRecoveryRoot)
	if recoveryRoot == "" {
		recoveryRoot = strings.TrimSpace(*githubRecoveryRoot)
	}
	var lifecycleOptions *api.LifecycleOptions
	if *fixtureWrite {
		if *realCopyWrite && strings.TrimSpace(*journalRoot) == "" {
			log.Fatal("--real-copy-write 必须配置 --journal-root，以保存独立控制面快照")
		}
		var mutation compat.ControlPlaneMutation
		descriptor := compat.SyntheticFixtureDescriptor()
		if strings.TrimSpace(*journalRoot) != "" {
			if *realCopyWrite {
				adapter, err := compat.NewRealShapeCopyMutation(filepath.Join(*journalRoot, "mutation-snapshots"))
				if err != nil {
					log.Fatal(err)
				}
				mutation = adapter
				descriptor = compat.RealShapeCopyDescriptor()
			} else {
				adapter, err := compat.NewSyntheticSQLiteMutation(filepath.Join(*journalRoot, "mutation-snapshots"))
				if err != nil {
					log.Fatal(err)
				}
				mutation = adapter
			}
		}
		lifecycleOptions = &api.LifecycleOptions{
			Enabled: true, FixtureOnly: true, Descriptor: descriptor,
			ControlPlaneMutation: mutation,
			ArchiveRoot:          *archiveRoot, JournalRoot: *journalRoot, TrashRoot: *trashRoot, CleanupRoot: *cleanupRoot,
			CleanupScanInterval:            cleanupInterval,
			UseSystemTrash:                 *systemTrash,
			RequireDestinationConfirmation: true,
			GitHubAuth:                     githubAuth,
			GitHubRepositories:             githubRepositories,
			RemoteArchive:                  remoteArchive,
			RemoteArchiveCatalog:           remoteArchiveCatalog,
			RemoteRecoveryRoot:             recoveryRoot,
		}
	} else if *authorizedRealTrash || *realConversationWrite {
		config := &authorizedlive.Config{Python: *authorizedPython, Bridge: *authorizedBridge, Socket: *authorizedSocket, AllConversations: *realConversationWrite}
		if err := config.Validate(*scanRoot); err != nil {
			log.Fatal(err)
		}
		if strings.TrimSpace(*archiveRoot) == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				log.Fatal(err)
			}
			*archiveRoot = defaultArchiveRoot(home)
		}
		lifecycleOptions = &api.LifecycleOptions{Enabled: true, AuthorizedLive: config, JournalRoot: *journalRoot, ArchiveRoot: *archiveRoot,
			UseSystemTrash: true, RequireDestinationConfirmation: true}
	} else if *systemTrash {
		log.Fatal("--system-trash 只允许在显式 fixture 写模式下使用")
	} else if githubAuth != nil || giteeOptionsConfigured || s3OptionsConfigured || webDAVConfigured || httpArchiveOptionsConfigured || cleanupInterval > 0 {
		lifecycleOptions = &api.LifecycleOptions{GitHubAuth: githubAuth, GitHubRepositories: githubRepositories, RemoteArchive: remoteArchive, RemoteArchiveCatalog: remoteArchiveCatalog, RemoteRecoveryRoot: recoveryRoot, CleanupScanInterval: cleanupInterval}
	}
	// 远端连接独立于源目录写权限；真实会话模式同样提供账户绑定与远端清单。
	if lifecycleOptions == nil {
		lifecycleOptions = &api.LifecycleOptions{}
	}
	lifecycleOptions.GitHubAuth, lifecycleOptions.GitHubRepositories = githubAuth, githubRepositories
	lifecycleOptions.RemoteArchive, lifecycleOptions.RemoteArchiveCatalog = remoteArchive, remoteArchiveCatalog
	if recoveryRoot == "" {
		if directory, configErr := os.UserConfigDir(); configErr == nil {
			recoveryRoot = filepath.Join(directory, "Codex Chronicle", "remote-recovery")
		}
	}
	lifecycleOptions.RemoteRecoveryRoot = recoveryRoot
	server, err := api.NewServerWithWorkspaceConfig(*scanRoot, archive, lifecycleOptions, *workspaceConfig)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	defer server.CloseWorkspaceStores()
	server.SetVersion(version)
	settingsPath := strings.TrimSpace(*githubSettingsPath)
	selectionPath := strings.TrimSpace(*githubSelectionPath)
	if directory, configErr := os.UserConfigDir(); configErr == nil {
		if settingsPath == "" {
			settingsPath = filepath.Join(directory, "Codex Chronicle", "github-settings.json")
		}
		if selectionPath == "" {
			selectionPath = filepath.Join(directory, "Codex Chronicle", "github-archive-selection.json")
		}
	}
	if settingsPath != "" && selectionPath != "" {
		tokens, err := platform.NewSystemGitHubTokenStore("")
		if err != nil {
			log.Fatal(err)
		}
		selection, err := archivepkg.NewFileGitHubArchiveSelectionStore(selectionPath)
		if err != nil {
			log.Fatal(err)
		}
		chunkMiB := int64(0)
		if *githubChunkBytes > 0 {
			chunkMiB = (*githubChunkBytes + (1 << 20) - 1) >> 20
		}
		if err := server.SetGitHubSettings(settingsPath, api.GitHubSettings{
			ClientID: *githubClientID, OAuthBaseURL: *githubBaseURL, APIBaseURL: *githubAPIBaseURL,
			ProxyURL: *githubProxyURL, ArchiveRoot: *githubArchiveRoot, Branch: *githubBranch, ChunkMiB: chunkMiB,
		}, tokens, *githubAccount, selection); err != nil {
			log.Fatal(err)
		}
	}
	if strings.TrimSpace(*trashSettingsPath) != "" {
		if strings.TrimSpace(*scanRoot) != "" && pathIsWithin(*scanRoot, *trashSettingsPath) {
			log.Fatal("--trash-settings 必须位于扫描根之外")
		}
		settings, settingsErr := platform.NewTrashSettingsStore(*trashSettingsPath)
		if settingsErr != nil {
			log.Fatal(settingsErr)
		}
		server.SetTrashSettingsStore(settings)
	}
	if strings.TrimSpace(*installRoot) != "" {
		if strings.TrimSpace(*scanRoot) != "" && pathIsWithin(*scanRoot, *installRoot) {
			log.Fatal("--install-root 不能位于真实 .codex 扫描根内")
		}
		managerOptions := updatepkg.ManagerOptions{RequireSignature: *updateRequireSignature, Online: updatepkg.OnlineConfig{FeedURL: *updateFeedURL, ProxyURL: *updateProxyURL, MaxDownloadBytes: *updateMaxDownloadBytes}}
		if strings.TrimSpace(*updateTrustedKeyFile) != "" {
			if !*updateRequireSignature {
				log.Fatal("--update-trusted-public-key-file 必须与 --update-require-signature 同时使用")
			}
			trustedKeys, keyErr := updatepkg.LoadTrustedPublicKeys(*updateTrustedKeyFile)
			if keyErr != nil {
				log.Fatal(keyErr)
			}
			managerOptions.TrustedPublicKeys = trustedKeys
		}
		manager, managerErr := updatepkg.NewManagerWithOptions(*installRoot, managerOptions)
		if managerErr != nil {
			log.Fatal(managerErr)
		}
		server.SetUpdateManager(manager)
		if desktopLease != nil {
			executable, executableErr := os.Executable()
			if executableErr != nil {
				log.Fatal(executableErr)
			}
			server.SetUpdateRestart(func(ctx context.Context, target string) error {
				return desktop.ScheduleUpdateRestart(ctx, desktop.LaunchOptions{PackageRoot: desktop.PackageRoot(executable), DataDir: filepath.Dir(*desktopStateDir), UpdateFeedURL: *updateFeedURL, NoBrowser: *desktopNoBrowser}, desktopLease.Instance.ID, target)
			})
			server.SetAutomaticUpdateRestart(func(ctx context.Context, target string) error {
				return desktop.ScheduleUpdateRestart(ctx, desktop.LaunchOptions{PackageRoot: desktop.PackageRoot(executable), DataDir: filepath.Dir(*desktopStateDir), UpdateFeedURL: *updateFeedURL, NoBrowser: *desktopNoBrowser, Background: true}, desktopLease.Instance.ID, target)
			})
		}
		if err := manager.Recover(context.Background()); err != nil {
			log.Fatalf("在线/离线更新中断恢复失败，未启动服务：%v", err)
		}
		server.SetUpdatePackageCheck(func(ctx context.Context, root string) error {
			// 候选启动器的 --verify 使用新建空源和隔离数据库，不接触运行中的真实工作区。
			command := exec.CommandContext(ctx, filepath.Join(root, "bin", desktop.ExecutableName("codex-chronicle-desktop")), "--verify")
			if err := command.Run(); err != nil {
				return fmt.Errorf("候选版本隔离启动检查失败：%w", err)
			}
			return nil
		})
	}
	summaryRegistry := summarypkg.DefaultRegistry()
	switch strings.TrimSpace(*summaryProvider) {
	case "", summarypkg.LocalProviderID:
	case summarypkg.OpenAICompatibleProviderID:
		provider, providerErr := summarypkg.NewOpenAICompatibleProvider(summarypkg.OpenAICompatibleConfig{
			BaseURL: *summaryBaseURL, APIKey: *summaryAPIKey, Model: *summaryModel, ProxyURL: *summaryProxy, MaxOutputTokens: *summaryMaxOutputTokens,
		})
		if providerErr != nil {
			log.Fatal(providerErr)
		}
		summaryRegistry = summarypkg.NewRegistry(provider)
	default:
		log.Fatalf("不支持的摘要 provider: %s", *summaryProvider)
	}
	server.SetSummaryRegistry(summaryRegistry)
	if err := server.SetResourceBudget(resourcepkg.Budget{MemoryLimitBytes: *memoryLimitBytes, PersistentLimitBytes: *persistentLimitBytes, TemporaryLimitBytes: *temporaryLimitBytes}); err != nil {
		log.Fatal(err)
	}
	if *allowRemote {
		bindingCode := strings.TrimSpace(*authBindingCode)
		if bindingCode == "" {
			bindingCode = strings.TrimSpace(*authToken)
		}
		manager, bootstrap, authErr := auth.NewManager(auth.Config{StatePath: *authStatePath, InitialBindingCode: bindingCode})
		if authErr != nil {
			log.Fatal(authErr)
		}
		server.SetAuthManager(manager)
		if bootstrap.Code != "" {
			log.Printf("Codex Chronicle 远程一次性绑定码（仅本次显示，过期时间 %s）：%s", bootstrap.ExpiresAt.Format("2006-01-02 15:04:05 MST"), bootstrap.Code)
		} else if bootstrap.Provided {
			log.Printf("Codex Chronicle 使用已配置的一次性远程绑定码；绑定后该码立即失效")
		}
	}
	if *desktopStateDir != "" {
		if err := server.SetUIPreferencesPath(filepath.Join(filepath.Dir(*desktopStateDir), "ui-preferences.json")); err != nil {
			log.Fatal(err)
		}
	} else if *archivePath != "" && *archivePath != ":memory:" {
		if err := server.SetUIPreferencesPath(filepath.Join(filepath.Dir(*archivePath), "ui-preferences.json")); err != nil {
			log.Fatal(err)
		}
	}
	compatibilityDir := ""
	if *desktopStateDir != "" {
		compatibilityDir = filepath.Dir(*desktopStateDir)
	} else if *archivePath != "" && *archivePath != ":memory:" {
		compatibilityDir = filepath.Dir(*archivePath)
	}
	if compatibilityDir != "" {
		if err := server.SetCompatibilityPath(filepath.Join(compatibilityDir, "compatibility.json")); err != nil {
			log.Fatal(err)
		}
	}
	mux := server.Handler()
	if strings.TrimSpace(*webRoot) != "" {
		wrapped, webErr := api.WithWebRoot(mux, *webRoot)
		if webErr != nil {
			log.Fatal(webErr)
		}
		mux = wrapped
	}
	if err := serve(*address, mux, server, desktopLease, *desktopAutoPort); err != nil {
		log.Fatal(err)
	}
}

// 中文说明：将默认本地归档放在用户容易从文件管理器找到的 Documents 目录下。
func defaultArchiveRoot(home string) string {
	return filepath.Join(home, "Documents", "Codex Chronicle", "archives")
}

func defaultAuthStatePath() string {
	if directory, err := os.UserConfigDir(); err == nil && strings.TrimSpace(directory) != "" {
		return filepath.Join(directory, "Codex Chronicle", "auth.json")
	}
	if directory, err := os.UserHomeDir(); err == nil && strings.TrimSpace(directory) != "" {
		return filepath.Join(directory, ".codex-chronicle-auth.json")
	}
	return filepath.Join(".", ".codex-chronicle-auth.json")
}

func authStatePathContainsCodex(path string) bool {
	clean := filepath.Clean(path)
	for _, part := range strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return false
}

func pathIsWithin(root, candidate string) bool {
	rootAbs, rootErr := filepath.Abs(root)
	candidateAbs, candidateErr := filepath.Abs(candidate)
	if rootErr != nil || candidateErr != nil {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(candidateAbs))
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func isLoopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return strings.EqualFold(strings.TrimSpace(address), "localhost")
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "::0" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func requireBearerToken(next http.Handler, expected string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := strings.TrimSpace(request.Header.Get("Authorization"))
		const prefix = "Bearer "
		if !strings.HasPrefix(provided, prefix) {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="codex-chronicle"`)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		providedToken := strings.TrimSpace(strings.TrimPrefix(provided, prefix))
		if subtle.ConstantTimeCompare([]byte(providedToken), []byte(expected)) != 1 {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="codex-chronicle"`)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
