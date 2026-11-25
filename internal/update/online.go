// 中文说明：提供受控的在线版本检查和流式下载；下载结果仍必须经过离线清单、签名和健康检查。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	archivepkg "codex-chronicle.local/internal/archive"
)

const (
	OnlineFeedFormatVersion    = 1
	DefaultOnlineFeedMaxBytes  = 4 << 20
	DefaultOnlineDownloadBytes = 2 << 30
)

// OnlineRelease 描述一个与当前平台绑定的发布包；包自身仍必须包含离线 manifest。
type OnlineRelease struct {
	Version   string `json:"version"`
	BuildID   string `json:"build_id,omitempty"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Notes     string `json:"notes,omitempty"`
}

// OnlineFeed 是可由静态 HTTP 服务托管的版本索引。
type OnlineFeed struct {
	FormatVersion int             `json:"format_version"`
	Product       string          `json:"product"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Releases      []OnlineRelease `json:"releases"`
}

// OnlineConfig 控制在线 feed、代理和下载上限；HTTPClient 只用于测试或上层显式注入。
type OnlineConfig struct {
	FeedURL          string
	ProxyURL         string
	MaxFeedBytes     int64
	MaxDownloadBytes int64
	HTTPClient       *http.Client
}

type onlineProxySettings struct {
	ProxyURL string `json:"proxy_url"`
}

func loadOnlineProxy(path string) (*string, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<10 {
		return nil, errors.New("在线更新代理设置不是有效的普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var settings onlineProxySettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, errors.New("无法解析在线更新代理设置")
	}
	value := strings.TrimSpace(settings.ProxyURL)
	return &value, nil
}

func saveOnlineProxy(path, proxyURL string) error {
	data, err := json.Marshal(onlineProxySettings{ProxyURL: proxyURL})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".online-proxy-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// OnlineCheck 是一次只读版本检查的结果。
type OnlineCheck struct {
	FeedURL        string         `json:"feed_url"`
	CurrentVersion string         `json:"current_version,omitempty"`
	Available      bool           `json:"available"`
	Release        *OnlineRelease `json:"release,omitempty"`
	CheckedAt      time.Time      `json:"checked_at"`
}

// DownloadedRelease 是已完成摘要校验、仍待人工预检的本地包。
type DownloadedRelease struct {
	Release   OnlineRelease `json:"release"`
	Path      string        `json:"path"`
	SizeBytes int64         `json:"size_bytes"`
	SHA256    string        `json:"sha256"`
}

// OnlineConfigured 表示是否配置了在线 feed；它不执行网络请求。
func (m *Manager) OnlineConfigured() bool {
	return m != nil && strings.TrimSpace(m.online.FeedURL) != ""
}

func normalizeOnlineConfig(config OnlineConfig) (OnlineConfig, error) {
	config.FeedURL = strings.TrimSpace(config.FeedURL)
	config.ProxyURL = strings.TrimSpace(config.ProxyURL)
	if config.FeedURL != "" {
		if err := validateOnlineURL(config.FeedURL, "在线 feed"); err != nil {
			return OnlineConfig{}, err
		}
	} else if config.ProxyURL != "" {
		return OnlineConfig{}, errors.New("在线代理必须与在线 feed 同时配置")
	}
	if config.ProxyURL != "" {
		proxy, err := url.Parse(config.ProxyURL)
		if err != nil || proxy.Host == "" || proxy.User != nil || proxy.Path != "" || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			return OnlineConfig{}, errors.New("在线更新代理必须是 HTTP、HTTPS、SOCKS5 或 SOCKS5h 地址")
		}
	}
	if config.MaxFeedBytes <= 0 {
		config.MaxFeedBytes = DefaultOnlineFeedMaxBytes
	}
	if config.MaxFeedBytes > 64<<20 {
		return OnlineConfig{}, errors.New("在线 feed 大小上限不能超过 64 MiB")
	}
	if config.MaxDownloadBytes <= 0 {
		config.MaxDownloadBytes = DefaultOnlineDownloadBytes
	}
	if config.MaxDownloadBytes > 16<<30 {
		return OnlineConfig{}, errors.New("在线更新包大小上限不能超过 16 GiB")
	}
	return config, nil
}

func validateOnlineURL(value, label string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s 必须是 HTTP/HTTPS 地址", label)
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%s 不能包含凭证或 fragment", label)
	}
	return nil
}

func validateOnlineRelease(release OnlineRelease, maxBytes int64) error {
	if !safeVersion(strings.TrimSpace(release.Version)) {
		return errors.New("在线发布版本无效")
	}
	if strings.TrimSpace(release.OS) == "" || strings.TrimSpace(release.Arch) == "" {
		return errors.New("在线发布包缺少目标平台")
	}
	if err := validateOnlineURL(release.URL, "在线发布包"); err != nil {
		return err
	}
	if !validSHA256(strings.ToLower(strings.TrimSpace(release.SHA256))) {
		return errors.New("在线发布包 SHA-256 无效")
	}
	if release.SizeBytes <= 0 || release.SizeBytes > maxBytes {
		return fmt.Errorf("在线发布包大小无效：%d，允许范围为 1-%d", release.SizeBytes, maxBytes)
	}
	return nil
}

// CheckOnlineFeed 从固定静态 feed 或 GitHub 公开预览列表读取当前平台更新；它只读网络，不写安装根。
func CheckOnlineFeed(ctx context.Context, config OnlineConfig, currentVersion string) (OnlineCheck, error) {
	config, err := normalizeOnlineConfig(config)
	if err != nil {
		return OnlineCheck{}, err
	}
	if config.FeedURL == "" {
		return OnlineCheck{}, errors.New("在线 feed 未配置")
	}
	client, err := onlineHTTPClient(config)
	if err != nil {
		return OnlineCheck{}, err
	}
	result := OnlineCheck{FeedURL: config.FeedURL, CurrentVersion: strings.TrimSpace(currentVersion), CheckedAt: time.Now().UTC()}
	feedURL, expectedVersion := config.FeedURL, ""
	if repository := githubReleaseRepository(config.FeedURL); repository != "" {
		feedURL, expectedVersion, err = resolveGitHubPreviewFeed(ctx, client, config, repository, result.CurrentVersion)
		if err != nil {
			return OnlineCheck{}, err
		}
		if feedURL == "" {
			return result, nil
		}
	}
	body, _, err := readOnlineJSON(ctx, client, feedURL, config.MaxFeedBytes)
	if err != nil {
		return OnlineCheck{}, err
	}
	var feed OnlineFeed
	if err := json.Unmarshal(body, &feed); err != nil {
		return OnlineCheck{}, fmt.Errorf("在线 feed 不是有效 JSON：%w", err)
	}
	if err := validateOnlineFeed(feed, config.MaxDownloadBytes); err != nil {
		return OnlineCheck{}, err
	}
	stableChannel := githubStableFeed(config.FeedURL)
	var selected *OnlineRelease
	for index := range feed.Releases {
		release := feed.Releases[index]
		if expectedVersion != "" && release.Version != expectedVersion {
			return OnlineCheck{}, errors.New("GitHub 发布标签与在线 feed 版本不匹配")
		}
		if stableChannel && (!githubReleaseTag.MatchString("v"+release.Version) || strings.Contains(release.Version, "-")) {
			return OnlineCheck{}, errors.New("稳定更新渠道不能包含预览版或无效版本")
		}
		if release.OS != runtime.GOOS || release.Arch != runtime.GOARCH {
			continue
		}
		if result.CurrentVersion != "" && !versionLess(result.CurrentVersion, release.Version) {
			continue
		}
		if selected == nil || versionLess(selected.Version, release.Version) {
			candidate := release
			selected = &candidate
		}
	}
	if selected != nil {
		result.Available = true
		result.Release = selected
	}
	return result, nil
}

// readOnlineJSON 让发布发现和 feed 请求共用代理、超时和响应上限，不携带 GitHub 账户令牌。
func readOnlineJSON(ctx context.Context, client *http.Client, address string, maxBytes int64) ([]byte, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("创建在线 feed 请求失败：%w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Codex-Chronicle-Updater")
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("读取在线 feed 失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return nil, nil, fmt.Errorf("在线 feed 返回 HTTP %d：%s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if response.ContentLength > maxBytes {
		return nil, nil, errors.New("在线 feed 超过大小限制")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("读取在线 feed 内容失败：%w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, nil, errors.New("在线 feed 超过大小限制")
	}
	return body, response.Header, nil
}

func validateOnlineFeed(feed OnlineFeed, maxDownloadBytes int64) error {
	if feed.FormatVersion != OnlineFeedFormatVersion {
		return fmt.Errorf("不支持的在线 feed 版本：%d", feed.FormatVersion)
	}
	if strings.TrimSpace(feed.Product) != "codex-chronicle" {
		return errors.New("在线 feed product 不匹配")
	}
	if len(feed.Releases) == 0 {
		return errors.New("在线 feed 没有发布包")
	}
	for _, release := range feed.Releases {
		if err := validateOnlineRelease(release, maxDownloadBytes); err != nil {
			return err
		}
	}
	return nil
}

// DownloadOnline 下载一次已通过 feed 校验的发布包，并以独立文件原子发布到安装根 bundles 目录。
func (m *Manager) DownloadOnline(ctx context.Context, release OnlineRelease) (DownloadedRelease, error) {
	if m == nil {
		return DownloadedRelease{}, errors.New("更新管理器未初始化")
	}
	if err := ctx.Err(); err != nil {
		return DownloadedRelease{}, err
	}
	m.mu.Lock()
	configSnapshot := m.online
	m.mu.Unlock()
	config, err := normalizeOnlineConfig(configSnapshot)
	if err != nil {
		return DownloadedRelease{}, err
	}
	if err := validateOnlineRelease(release, config.MaxDownloadBytes); err != nil {
		return DownloadedRelease{}, err
	}
	if release.OS != runtime.GOOS || release.Arch != runtime.GOARCH {
		return DownloadedRelease{}, fmt.Errorf("在线发布包目标平台 %s/%s 与当前 %s/%s 不匹配", release.OS, release.Arch, runtime.GOOS, runtime.GOARCH)
	}
	client, err := onlineHTTPClient(config)
	if err != nil {
		return DownloadedRelease{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, release.URL, nil)
	if err != nil {
		return DownloadedRelease{}, fmt.Errorf("创建在线下载请求失败：%w", err)
	}
	request.Header.Set("Accept", "application/octet-stream, application/zip, application/gzip, application/x-tar")
	response, err := client.Do(request)
	if err != nil {
		return DownloadedRelease{}, fmt.Errorf("下载在线发布包失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return DownloadedRelease{}, fmt.Errorf("在线发布包返回 HTTP %d：%s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if response.ContentLength > config.MaxDownloadBytes {
		return DownloadedRelease{}, errors.New("在线发布包超过大小限制")
	}
	id, err := newID("download")
	if err != nil {
		return DownloadedRelease{}, err
	}
	bundlesRoot := filepath.Join(m.root, BundlesDirName)
	if err := ensureDirectory(bundlesRoot); err != nil {
		return DownloadedRelease{}, err
	}
	path := filepath.Join(bundlesRoot, id+".archive")
	temporary := path + ".tmp"
	lease, err := acquireUpdateLock(temporary + ".lock")
	if err != nil {
		return DownloadedRelease{}, err
	}
	defer func() {
		_ = lease.Close()
		_ = os.Remove(temporary + ".lock")
	}()
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return DownloadedRelease{}, fmt.Errorf("创建在线下载临时文件失败：%w", err)
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(temporary)
			_ = os.Remove(path)
		}
	}()
	hash := sha256.New()
	limited := io.LimitReader(response.Body, config.MaxDownloadBytes+1)
	bytesRead, copyErr := io.CopyBuffer(io.MultiWriter(file, hash), limited, make([]byte, 1<<20))
	if copyErr != nil {
		return DownloadedRelease{}, fmt.Errorf("写入在线发布包失败：%w", copyErr)
	}
	if bytesRead > config.MaxDownloadBytes {
		return DownloadedRelease{}, errors.New("在线发布包超过大小限制")
	}
	if err := file.Sync(); err != nil {
		return DownloadedRelease{}, fmt.Errorf("同步在线发布包失败：%w", err)
	}
	if err := file.Close(); err != nil {
		return DownloadedRelease{}, fmt.Errorf("关闭在线发布包失败：%w", err)
	}
	if release.SizeBytes != bytesRead {
		return DownloadedRelease{}, fmt.Errorf("在线发布包大小不匹配：收到 %d，期望 %d", bytesRead, release.SizeBytes)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(digest, release.SHA256) {
		return DownloadedRelease{}, errors.New("在线发布包 SHA-256 不匹配")
	}
	if err := os.Rename(temporary, path); err != nil {
		return DownloadedRelease{}, fmt.Errorf("发布在线下载文件失败：%w", err)
	}
	cleanup = false
	if err := syncDirectory(bundlesRoot); err != nil {
		_ = os.Remove(path)
		return DownloadedRelease{}, fmt.Errorf("同步在线下载目录失败：%w", err)
	}
	return DownloadedRelease{Release: release, Path: path, SizeBytes: bytesRead, SHA256: digest}, nil
}

func onlineHTTPClient(config OnlineConfig) (*http.Client, error) {
	client, err := archivepkg.NewProxyHTTPClient(config.ProxyURL, config.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("创建在线更新 HTTP transport 失败：%w", err)
	}
	if config.HTTPClient != nil {
		return client, nil
	}
	client.Timeout = 2 * time.Minute
	return client, nil
}

// CheckOnline 检查管理器配置的在线 feed；不会下载或切换版本。
func (m *Manager) CheckOnline(ctx context.Context) (OnlineCheck, error) {
	if m == nil {
		return OnlineCheck{}, errors.New("更新管理器未初始化")
	}
	m.mu.Lock()
	state, err := m.readState()
	config := m.online
	m.mu.Unlock()
	if err != nil {
		return OnlineCheck{}, err
	}
	return CheckOnlineFeed(ctx, config, state.ActiveVersion)
}

// PrepareOnline 下载当前 feed 选出的版本并生成离线更新计划；版本切换仍由 ActivateByID 显式完成。
func (m *Manager) PrepareOnline(ctx context.Context, allowDowngrade bool) (Plan, DownloadedRelease, error) {
	check, err := m.CheckOnline(ctx)
	if err != nil {
		return Plan{}, DownloadedRelease{}, err
	}
	if !check.Available || check.Release == nil {
		return Plan{}, DownloadedRelease{}, errors.New("在线 feed 没有高于当前版本的可用更新")
	}
	downloaded, err := m.DownloadOnline(ctx, *check.Release)
	if err != nil {
		return Plan{}, DownloadedRelease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		_ = os.Remove(downloaded.Path)
		return Plan{}, DownloadedRelease{}, err
	}
	defer lock.Close()
	if err := m.recoverLocked(ctx); err != nil {
		_ = os.Remove(downloaded.Path)
		return Plan{}, DownloadedRelease{}, err
	}
	plan, err := m.prepareArchiveLocked(ctx, downloaded.Path, allowDowngrade)
	if err != nil {
		_ = os.Remove(downloaded.Path)
		return Plan{}, DownloadedRelease{}, err
	}
	// 下载摘要只绑定字节；还须核对签名清单中的版本身份，避免 feed 与实际候选不一致。
	if plan.Version != downloaded.Release.Version || (downloaded.Release.BuildID != "" && plan.Manifest.BuildID != downloaded.Release.BuildID) {
		_ = os.Remove(m.planPath(plan.ID))
		_ = os.RemoveAll(plan.BundleRoot)
		_ = os.Remove(downloaded.Path)
		return Plan{}, DownloadedRelease{}, errors.New("在线发布信息与包内清单的版本或构建标识不一致")
	}
	return plan, downloaded, nil
}
