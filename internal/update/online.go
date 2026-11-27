// 中文说明：提供受控的在线版本检查和流式下载；下载结果仍必须经过离线清单、签名和健康检查。
package update

import (
	"context"
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
	FormatVersion int                `json:"format_version"`
	Product       string             `json:"product"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Releases      []OnlineRelease    `json:"releases"`
	UpdatesPaused bool               `json:"updates_paused,omitempty"`
	PauseReason   string             `json:"pause_reason,omitempty"`
	Withdrawn     []WithdrawnVersion `json:"withdrawn_versions,omitempty"`
	HeldReleases  []OnlineRelease    `json:"held_releases,omitempty"`
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
	UpdatesPaused  bool           `json:"updates_paused,omitempty"`
	PauseReason    string         `json:"pause_reason,omitempty"`
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
	feed, err := readOnlineFeed(ctx, config, currentVersion)
	if err != nil {
		return OnlineCheck{}, err
	}
	result := OnlineCheck{FeedURL: config.FeedURL, CurrentVersion: strings.TrimSpace(currentVersion), CheckedAt: time.Now().UTC(), UpdatesPaused: feed.UpdatesPaused, PauseReason: feed.PauseReason}
	if feed.UpdatesPaused {
		return result, nil
	}
	for index := range feed.Releases {
		release := feed.Releases[index]
		if release.OS != runtime.GOOS || release.Arch != runtime.GOARCH || withdrawnReason(feed, release.Version) != "" {
			continue
		}
		if result.CurrentVersion != "" && !versionLess(result.CurrentVersion, release.Version) {
			continue
		}
		if result.Release == nil || versionLess(result.Release.Version, release.Version) {
			candidate := release
			result.Release = &candidate
		}
	}
	result.Available = result.Release != nil
	return result, nil
}

func readOnlineFeed(ctx context.Context, config OnlineConfig, currentVersion string) (OnlineFeed, error) {
	config, err := normalizeOnlineConfig(config)
	if err != nil {
		return OnlineFeed{}, err
	}
	if config.FeedURL == "" {
		return OnlineFeed{}, errors.New("在线 feed 未配置")
	}
	client, err := onlineHTTPClient(config)
	if err != nil {
		return OnlineFeed{}, err
	}
	feedURL, expectedVersion := config.FeedURL, ""
	if repository := githubReleaseRepository(config.FeedURL); repository != "" {
		feedURL, expectedVersion, err = resolveGitHubPreviewFeed(ctx, client, config, repository, currentVersion)
		if err != nil {
			return OnlineFeed{}, err
		}
		if feedURL == "" {
			return OnlineFeed{}, nil
		}
	}
	body, _, err := readOnlineJSON(ctx, client, feedURL, config.MaxFeedBytes)
	if err != nil {
		return OnlineFeed{}, err
	}
	var feed OnlineFeed
	if err := json.Unmarshal(body, &feed); err != nil {
		return OnlineFeed{}, fmt.Errorf("在线 feed 不是有效 JSON：%w", err)
	}
	if err := validateOnlineFeed(feed, config.MaxDownloadBytes); err != nil {
		return OnlineFeed{}, err
	}
	for _, release := range append(append([]OnlineRelease(nil), feed.Releases...), feed.HeldReleases...) {
		if expectedVersion != "" && release.Version != expectedVersion {
			return OnlineFeed{}, errors.New("GitHub 发布标签与在线 feed 版本不匹配")
		}
		if githubStableFeed(config.FeedURL) && (!githubReleaseTag.MatchString("v"+release.Version) || strings.Contains(release.Version, "-")) {
			return OnlineFeed{}, errors.New("稳定更新渠道不能包含预览版或无效版本")
		}
	}
	return feed, nil
}

// readOnlineJSON 让发布发现和 feed 请求共用代理、超时和响应上限，不携带 GitHub 账户令牌。
func readOnlineJSON(ctx context.Context, client *http.Client, address string, maxBytes int64) ([]byte, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("创建在线 feed 请求失败：%w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Codex-Chronicle-Updater")
	request.Header.Set("Cache-Control", "no-cache")
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
	if len(feed.Releases) == 0 && len(feed.HeldReleases) == 0 {
		return errors.New("在线 feed 没有发布包")
	}
	if len(feed.HeldReleases) != 0 {
		if len(feed.Releases) != 0 {
			return errors.New("在线 feed 不能同时提供和隐藏发布包")
		}
		for _, release := range feed.HeldReleases {
			if !feed.UpdatesPaused && withdrawnReason(feed, release.Version) == "" {
				return errors.New("隐藏的发布包缺少暂停或撤回策略")
			}
		}
	}
	if len(feed.Withdrawn) > 1024 || len(feed.PauseReason) > 2000 {
		return errors.New("在线更新策略超过大小限制")
	}
	seen := map[string]bool{}
	for _, item := range feed.Withdrawn {
		if !safeVersion(item.Version) || seen[item.Version] || len(item.Reason) > 2000 {
			return errors.New("撤回版本列表包含无效或重复条目")
		}
		seen[item.Version] = true
	}
	for _, release := range feed.Releases {
		if err := validateOnlineRelease(release, maxDownloadBytes); err != nil {
			return err
		}
	}
	for _, release := range feed.HeldReleases {
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
	lease, err := acquireUpdateLock(filepath.Join(m.root, ".download.lock"))
	if err != nil {
		return DownloadedRelease{}, err
	}
	defer lease.Close()
	return m.downloadOnline(ctx, release)
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
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.ResponseHeaderTimeout = 30 * time.Second
	}
	return client, nil
}

// CheckOnline 检查管理器配置的在线 feed；不会下载或切换版本。
func (m *Manager) CheckOnline(ctx context.Context) (check OnlineCheck, err error) {
	if m == nil {
		return OnlineCheck{}, errors.New("更新管理器未初始化")
	}
	finish := BeginDiagnostic(m.root, "check", "")
	defer func() { finish(err) }()
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
	return m.prepareOnline(ctx, allowDowngrade, false)
}

func (m *Manager) prepareOnline(ctx context.Context, allowDowngrade, automatic bool) (prepared Plan, downloadedRelease DownloadedRelease, err error) {
	if m == nil {
		return Plan{}, DownloadedRelease{}, errors.New("更新管理器未初始化")
	}
	finish := BeginDiagnostic(m.root, "prepare_online", "")
	defer func() { finish(err) }()
	// 下载与计划登记共用租约，避免清理恰好发生在文件下载完成、计划尚未持久化之间。
	lease, err := acquireUpdateLock(filepath.Join(m.root, ".download.lock"))
	if err != nil {
		return Plan{}, DownloadedRelease{}, err
	}
	defer lease.Close()
	check, err := m.CheckOnline(ctx)
	if err != nil {
		return Plan{}, DownloadedRelease{}, err
	}
	if check.UpdatesPaused {
		return Plan{}, DownloadedRelease{}, fmt.Errorf("%w：%s", ErrOnlineUpdatesPaused, check.PauseReason)
	}
	if !check.Available || check.Release == nil {
		return Plan{}, DownloadedRelease{}, errors.New("在线 feed 没有高于当前版本的可用更新")
	}
	downloaded, err := m.downloadOnline(ctx, *check.Release)
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
	source := &OnlineSource{FeedURL: check.FeedURL, Release: downloaded.Release}
	plan, err := m.prepareArchiveLocked(ctx, downloaded.Path, allowDowngrade, automatic, source)
	if err != nil {
		_ = os.Remove(downloaded.Path)
		var pathError *os.PathError
		var spaceError *InsufficientSpaceError
		if !errors.As(err, &pathError) && !errors.As(err, &spaceError) && !errors.Is(err, ErrDiskSpaceUnavailable) && ctx.Err() == nil {
			return Plan{}, DownloadedRelease{}, fmt.Errorf("%w：%v", ErrInvalidOnlinePackage, err)
		}
		return Plan{}, DownloadedRelease{}, err
	}
	// 下载摘要只绑定字节；还须核对签名清单中的版本身份，避免 feed 与实际候选不一致。
	if plan.Version != downloaded.Release.Version || (downloaded.Release.BuildID != "" && plan.Manifest.BuildID != downloaded.Release.BuildID) {
		_ = os.Remove(m.planPath(plan.ID))
		_ = os.RemoveAll(plan.BundleRoot)
		_ = os.Remove(downloaded.Path)
		return Plan{}, DownloadedRelease{}, fmt.Errorf("%w：在线发布信息与包内清单的版本或构建标识不一致", ErrInvalidOnlinePackage)
	}
	plan.DownloadPath = downloaded.Path
	if err := writeAtomicJSON(ctx, m.planPath(plan.ID), plan); err != nil {
		return Plan{}, DownloadedRelease{}, fmt.Errorf("保存下载包与更新计划的关联失败：%w", err)
	}
	return plan, downloaded, nil
}
