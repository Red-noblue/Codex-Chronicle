// 中文说明：管理离线安装包的清单校验、版本切换、健康检查和失败回滚；不读取或修改真实 .codex。
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-chronicle.local/internal/maintenance"
)

const (
	ManifestName    = "codex-chronicle-release.json"
	StateName       = "install-state.json"
	JournalName     = "install-journal.json"
	PlanDirName     = "plans"
	BundlesDirName  = "bundles"
	VersionDir      = "versions"
	ManifestVersion = 1
)

// FileEntry 是离线安装包中一个普通文件的摘要；路径始终相对 bundle 根目录。
type FileEntry struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Mode      uint32 `json:"mode"`
}

// Manifest 是离线安装包的签名材料前置校验清单。签名校验可由上游发布工具另行叠加。
type Manifest struct {
	FormatVersion int                `json:"format_version"`
	Version       string             `json:"version"`
	BuildID       string             `json:"build_id,omitempty"`
	OS            string             `json:"os"`
	Arch          string             `json:"arch"`
	CreatedAt     time.Time          `json:"created_at"`
	Files         []FileEntry        `json:"files"`
	Signature     *ManifestSignature `json:"signature,omitempty"`
}

// Plan 是绑定 bundle 清单和安装根的持久化更新计划。
type Plan struct {
	ID              string        `json:"id"`
	BundleRoot      string        `json:"bundle_root"`
	DownloadPath    string        `json:"download_path,omitempty"`
	InstallRoot     string        `json:"install_root"`
	Version         string        `json:"version"`
	PreviousVersion string        `json:"previous_version,omitempty"`
	ManifestSHA256  string        `json:"manifest_sha256"`
	FileCount       int           `json:"file_count"`
	TotalBytes      int64         `json:"total_bytes"`
	AllowDowngrade  bool          `json:"allow_downgrade"`
	Automatic       bool          `json:"automatic,omitempty"`
	Online          *OnlineSource `json:"online,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	Manifest        Manifest      `json:"manifest"`
}

// State 是安装根的当前版本指针；它通过原子替换写入，不依赖软链接。
type State struct {
	InstalledVersion        string        `json:"installed_version,omitempty"`
	ActiveVersion           string        `json:"active_version,omitempty"`
	PreviousVersion         string        `json:"previous_version,omitempty"`
	LastOperationID         string        `json:"last_operation_id,omitempty"`
	LastStatus              string        `json:"last_status,omitempty"`
	LastError               string        `json:"last_error,omitempty"`
	ConfirmedAt             *time.Time    `json:"confirmed_at,omitempty"`
	CleanupAfter            *time.Time    `json:"cleanup_after,omitempty"`
	CleanupRetentionMinutes int           `json:"cleanup_retention_minutes,omitempty"`
	RejectedVersion         string        `json:"rejected_version,omitempty"`
	DeferredVersion         string        `json:"deferred_version,omitempty"`
	Online                  *OnlineSource `json:"online,omitempty"`
	UpdatedAt               time.Time     `json:"updated_at"`
}

// Status 是 UI/API 可展示的安装状态。
type Status struct {
	InstallRoot       string            `json:"install_root"`
	State             State             `json:"state"`
	Versions          []string          `json:"versions"`
	PendingPlanIDs    []string          `json:"pending_plan_ids,omitempty"`
	RecoveryRequired  bool              `json:"recovery_required"`
	RecoveryMessage   string            `json:"recovery_message,omitempty"`
	ReadOnlySource    bool              `json:"read_only_source"`
	SignatureRequired bool              `json:"signature_required"`
	TrustedKeyCount   int               `json:"trusted_key_count"`
	OnlineConfigured  bool              `json:"online_configured"`
	OnlineFeedURL     string            `json:"online_feed_url,omitempty"`
	OnlineProxyURL    string            `json:"online_proxy_url,omitempty"`
	LastCleanup       *CleanupResult    `json:"last_cleanup,omitempty"`
	Automatic         AutomaticStatus   `json:"automatic"`
	Download          *DownloadProgress `json:"download,omitempty"`
	LastDiagnostic    *DiagnosticEvent  `json:"last_diagnostic,omitempty"`
}

// SetOnlineProxy 更新并持久化仅供在线更新使用的代理地址。
func (m *Manager) SetOnlineProxy(proxyURL string) error {
	if m == nil {
		return errors.New("更新管理器未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	config := m.online
	config.ProxyURL = strings.TrimSpace(proxyURL)
	validated, err := normalizeOnlineConfig(config)
	if err != nil {
		return err
	}
	if err := saveOnlineProxy(filepath.Join(m.root, "online-proxy.json"), validated.ProxyURL); err != nil {
		return fmt.Errorf("保存在线更新代理失败：%w", err)
	}
	m.online = validated
	return nil
}

// Result 是一次版本切换或回滚的可验证结果。
type Result struct {
	OperationID     string    `json:"operation_id"`
	Action          string    `json:"action"`
	ActiveVersion   string    `json:"active_version"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	VersionRoot     string    `json:"version_root"`
	Verified        bool      `json:"verified"`
	HealthChecked   bool      `json:"health_checked"`
	CompletedAt     time.Time `json:"completed_at"`
}

// HealthCheck 在切换前验证候选版本；失败时旧版本指针保持不变。
type HealthCheck func(context.Context, string) error

// Manager 负责一个明确指定的安装根；它不负责下载、解压或停止正在运行的 daemon。
type Manager struct {
	mu                sync.Mutex
	root              string
	trustedPublicKeys [][]byte
	requireSignature  bool
	online            OnlineConfig
	diskSpaceCheck    func(string, int64, string) error
	downloadProgress  *DownloadProgress
	downloadCancel    context.CancelCauseFunc
	downloadAutomatic bool
	registerArtifact  func(context.Context, maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error)
}

// NewManager 创建更新管理器并建立其控制目录。安装根不能是 .codex 或符号链接。
func NewManager(root string) (*Manager, error) {
	return NewManagerWithOptions(root, ManagerOptions{})
}

// NewManagerWithOptions 创建带供应链信任策略的更新管理器。
func NewManagerWithOptions(root string, options ManagerOptions) (*Manager, error) {
	abs, err := safeRoot(root)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{VersionDir, PlanDirName, BundlesDirName, partialDownloadsDir} {
		path := filepath.Join(abs, name)
		if err := ensureDirectory(path); err != nil {
			return nil, fmt.Errorf("创建更新控制目录失败：%w", err)
		}
	}
	keys := make([][]byte, 0, len(options.TrustedPublicKeys))
	for _, key := range options.TrustedPublicKeys {
		if len(key) != ed25519.PublicKeySize {
			return nil, errors.New("更新信任公钥长度无效")
		}
		keys = append(keys, append([]byte(nil), key...))
	}
	if options.RequireSignature && len(keys) == 0 {
		return nil, errors.New("强制签名模式必须配置至少一个信任公钥")
	}
	online, err := normalizeOnlineConfig(options.Online)
	if err != nil {
		return nil, err
	}
	// 离线验签和启动确认不使用网络，不能因保存的代理设置而要求配置更新源。
	if online.FeedURL != "" {
		proxyPath := filepath.Join(abs, "online-proxy.json")
		if storedProxy, loadErr := loadOnlineProxy(proxyPath); loadErr != nil {
			return nil, loadErr
		} else if storedProxy != nil {
			online.ProxyURL = *storedProxy
			online, err = normalizeOnlineConfig(online)
			if err != nil {
				return nil, err
			}
		}
	}
	return &Manager{root: abs, trustedPublicKeys: keys, requireSignature: options.RequireSignature, online: online}, nil
}

// Root 返回安装根的绝对路径。
func (m *Manager) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

// SetArtifactRegistrar 注入可选的生成物登记回调；nil 表示不登记更新 staging。
func (m *Manager) SetArtifactRegistrar(register func(context.Context, maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registerArtifact = register
}

// VerifyVersionRoot 使用当前管理器的供应链策略验证已发布版本目录。
func (m *Manager) VerifyVersionRoot(ctx context.Context, root string) error {
	if m == nil {
		return errors.New("更新管理器未初始化")
	}
	_, _, err := m.readAndValidateManifest(ctx, root)
	return err
}

// BuildManifest 从当前运行平台的 bundle 目录生成清单；它不会写入 bundle。
func BuildManifest(ctx context.Context, bundleRoot, version, buildID string) (Manifest, error) {
	return BuildManifestForPlatform(ctx, bundleRoot, version, buildID, runtime.GOOS, runtime.GOARCH)
}

// BuildManifestForPlatform 为指定目标平台生成清单，供交叉发布工具使用。
// targetOS 和 targetArch 只写入 manifest，不会改变文件内容或执行交叉编译。
func BuildManifestForPlatform(ctx context.Context, bundleRoot, version, buildID, targetOS, targetArch string) (Manifest, error) {
	root, err := safeExistingDirectory(bundleRoot)
	if err != nil {
		return Manifest{}, err
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return Manifest{}, errors.New("发布版本不能为空")
	}
	targetOS = strings.TrimSpace(targetOS)
	targetArch = strings.TrimSpace(targetArch)
	if targetOS == "" || targetArch == "" {
		return Manifest{}, errors.New("发布目标 OS 和架构不能为空")
	}
	manifest := Manifest{FormatVersion: ManifestVersion, Version: version, BuildID: strings.TrimSpace(buildID), OS: targetOS, Arch: targetArch, CreatedAt: time.Now().UTC(), Files: make([]FileEntry, 0)}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return fmt.Errorf("bundle 含有符号链接：%s", rel)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("bundle 含有不支持的特殊文件：%s", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		digest, size, err := hashFile(ctx, path)
		if err != nil {
			return fmt.Errorf("读取 bundle 文件 %s 失败：%w", rel, err)
		}
		if size != info.Size() {
			return fmt.Errorf("bundle 文件在读取期间发生变化：%s", rel)
		}
		manifest.Files = append(manifest.Files, FileEntry{Path: rel, SizeBytes: size, SHA256: digest, Mode: uint32(info.Mode().Perm())})
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	return manifest, nil
}

// WriteManifest 将清单原子写入 bundle 根，供离线发布工具使用。
func WriteManifest(ctx context.Context, bundleRoot string, manifest Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := safeExistingDirectory(bundleRoot)
	if err != nil {
		return err
	}
	if err := validateManifest(manifest); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(ctx, filepath.Join(root, ManifestName), append(data, '\n'), 0o600)
}

// Prepare 读取并校验离线 bundle，持久化一份待人工确认的计划；不会安装或切换版本。
func (m *Manager) Prepare(ctx context.Context, bundleRoot string, allowDowngrade bool) (Plan, error) {
	if m == nil {
		return Plan{}, errors.New("更新管理器未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return Plan{}, err
	}
	defer lock.Close()
	if err := m.recoverLocked(ctx); err != nil {
		return Plan{}, err
	}
	return m.prepareBundleLocked(ctx, bundleRoot, allowDowngrade, "")
}

// PreparePath 根据路径类型选择目录 bundle 或压缩离线包；目录调用保持与 Prepare 相同的校验。
func (m *Manager) PreparePath(ctx context.Context, bundlePath string, allowDowngrade bool) (result Plan, err error) {
	if m == nil {
		return Plan{}, errors.New("更新管理器未初始化")
	}
	finish := BeginDiagnostic(m.root, "prepare_offline", "")
	defer func() { finish(err) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return Plan{}, err
	}
	defer lock.Close()
	if err := m.recoverLocked(ctx); err != nil {
		return Plan{}, err
	}
	abs, err := filepath.Abs(filepath.Clean(bundlePath))
	if err != nil {
		return Plan{}, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return Plan{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Plan{}, errors.New("离线 bundle 不能是符号链接")
	}
	if info.IsDir() {
		return m.prepareBundleLocked(ctx, abs, allowDowngrade, "")
	}
	if !info.Mode().IsRegular() {
		return Plan{}, errors.New("离线 bundle 必须是目录或普通压缩文件")
	}
	return m.prepareArchiveLocked(ctx, abs, allowDowngrade, false, nil)
}

// PrepareArchive 解压并校验离线包，随后将解压目录绑定到持久化更新计划。
func (m *Manager) PrepareArchive(ctx context.Context, archivePath string, allowDowngrade bool) (Plan, error) {
	if m == nil {
		return Plan{}, errors.New("更新管理器未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return Plan{}, err
	}
	defer lock.Close()
	if err := m.recoverLocked(ctx); err != nil {
		return Plan{}, err
	}
	return m.prepareArchiveLocked(ctx, archivePath, allowDowngrade, false, nil)
}

func (m *Manager) prepareArchiveLocked(ctx context.Context, archivePath string, allowDowngrade, automatic bool, online *OnlineSource) (Plan, error) {
	expanded, err := archiveExpandedBytes(ctx, archivePath, DefaultExtractionLimits)
	if err != nil {
		return Plan{}, err
	}
	if err := m.requireDiskSpace(expanded, "解压更新包"); err != nil {
		return Plan{}, err
	}
	id, err := newID("plan")
	if err != nil {
		return Plan{}, err
	}
	stage := filepath.Join(m.root, BundlesDirName, ".staging-"+id)
	finishExtract := BeginDiagnostic(m.root, "extract", "")
	err = ExtractArchive(ctx, archivePath, stage, DefaultExtractionLimits)
	finishExtract(err)
	if err != nil {
		return Plan{}, fmt.Errorf("解压离线包失败：%w", err)
	}
	keepBundle := false
	defer func() {
		if !keepBundle {
			_ = os.RemoveAll(stage)
		}
	}()
	finalRoot := filepath.Join(m.root, BundlesDirName, id)
	plan, err := m.prepareBundleLocked(ctx, stage, allowDowngrade, id)
	if err != nil {
		return Plan{}, err
	}
	if err := os.Rename(stage, finalRoot); err != nil {
		return Plan{}, fmt.Errorf("保存解压后的离线 bundle 失败：%w", err)
	}
	plan.BundleRoot = finalRoot
	plan.Automatic = automatic
	plan.Online = online
	if filepath.Dir(archivePath) == filepath.Join(m.root, BundlesDirName) && generatedName(filepath.Base(archivePath), "download-", ".archive") {
		plan.DownloadPath = archivePath
	}
	if err := writeAtomicJSON(ctx, m.planPath(plan.ID), plan); err != nil {
		_ = os.RemoveAll(finalRoot)
		return Plan{}, fmt.Errorf("保存更新计划失败：%w", err)
	}
	keepBundle = true
	return plan, nil
}

func (m *Manager) prepareBundleLocked(ctx context.Context, bundleRoot string, allowDowngrade bool, requestedID string) (Plan, error) {
	bundle, err := safeExistingDirectory(bundleRoot)
	if err != nil {
		return Plan{}, err
	}
	finishVerify := BeginDiagnostic(m.root, "verify_signature", "")
	manifest, manifestHash, err := m.readAndValidateManifest(ctx, bundle)
	finishVerify(err)
	if err != nil {
		return Plan{}, err
	}
	state, err := m.readState()
	if err != nil {
		return Plan{}, err
	}
	if state.ActiveVersion != "" && state.ActiveVersion == manifest.Version && !allowDowngrade {
		return Plan{}, fmt.Errorf("安装根已经激活版本 %s；拒绝重复切换", manifest.Version)
	}
	if state.ActiveVersion != "" && !allowDowngrade && versionLess(manifest.Version, state.ActiveVersion) {
		return Plan{}, fmt.Errorf("目标版本 %s 低于当前版本 %s；需要明确允许降级", manifest.Version, state.ActiveVersion)
	}
	id := requestedID
	if id == "" {
		id, err = newID("plan")
		if err != nil {
			return Plan{}, err
		}
	}
	plan := Plan{ID: id, BundleRoot: bundle, InstallRoot: m.root, Version: manifest.Version, PreviousVersion: state.ActiveVersion, ManifestSHA256: manifestHash, FileCount: len(manifest.Files), AllowDowngrade: allowDowngrade, CreatedAt: time.Now().UTC(), Manifest: manifest}
	for _, file := range manifest.Files {
		plan.TotalBytes += file.SizeBytes
	}
	if requestedID != "" {
		return plan, nil
	}
	if err := writeAtomicJSON(ctx, m.planPath(id), plan); err != nil {
		return Plan{}, fmt.Errorf("保存更新计划失败：%w", err)
	}
	return plan, nil
}

// LoadPlan 读取持久化计划，不重新执行安装。
func (m *Manager) LoadPlan(ctx context.Context, id string) (Plan, error) {
	if m == nil {
		return Plan{}, errors.New("更新管理器未初始化")
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	if !safeID(id) {
		return Plan{}, fmt.Errorf("更新计划 ID 无效：%q", id)
	}
	var plan Plan
	if err := readJSON(m.planPath(id), &plan); err != nil {
		return Plan{}, fmt.Errorf("读取更新计划失败：%w", err)
	}
	if plan.ID != id || filepath.Clean(plan.InstallRoot) != m.root {
		return Plan{}, errors.New("更新计划不属于当前安装根")
	}
	return plan, nil
}

// ActivateByID 重新校验持久化计划并执行版本切换。
func (m *Manager) ActivateByID(ctx context.Context, id string, health HealthCheck) (Result, error) {
	plan, err := m.LoadPlan(ctx, id)
	if err != nil {
		return Result{}, err
	}
	return m.Activate(ctx, plan, health)
}

// Activate 在候选版本健康检查通过后原子更新当前版本状态。
func (m *Manager) Activate(ctx context.Context, plan Plan, health HealthCheck) (result Result, err error) {
	if m == nil {
		return Result{}, errors.New("更新管理器未初始化")
	}
	finish := BeginDiagnostic(m.root, "activate", plan.Version)
	defer func() { finish(err) }()
	if health == nil {
		return Result{}, errors.New("更新切换必须提供健康检查")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err := m.recoverLocked(ctx); err != nil {
		return Result{}, err
	}
	if plan.ID == "" || filepath.Clean(plan.InstallRoot) != m.root {
		return Result{}, errors.New("更新计划不属于当前安装根")
	}
	if err := m.validatePlanLocked(ctx, plan); err != nil {
		return Result{}, err
	}
	state, err := m.readState()
	if err != nil {
		return Result{}, err
	}
	if state.ActiveVersion == plan.Version {
		return Result{}, fmt.Errorf("版本 %s 已经激活", plan.Version)
	}
	if err := m.requireDiskSpace(plan.TotalBytes, "复制新版本"); err != nil {
		return Result{}, err
	}
	// 暂缓重启留下的未运行副本可在相同签名内容重新获准后替换，不能覆盖其他既有版本。
	if state.DeferredVersion == plan.Version {
		root := filepath.Join(m.root, VersionDir, safeVersionName(plan.Version))
		if _, err := os.Lstat(root); err == nil {
			_, digest, err := m.readAndValidateManifest(ctx, root)
			if err != nil {
				return Result{}, err
			}
			if digest != plan.ManifestSHA256 {
				return Result{}, errors.New("暂缓版本内容已变化，拒绝替换")
			}
			id, err := newID("cleanup")
			if err != nil {
				return Result{}, err
			}
			retired := filepath.Join(m.root, VersionDir, "."+id)
			if err := os.Rename(root, retired); err != nil {
				return Result{}, err
			}
			_ = os.RemoveAll(retired)
		} else if !os.IsNotExist(err) {
			return Result{}, err
		}
	}
	operationID, err := newID("update")
	if err != nil {
		return Result{}, err
	}
	journal := journal{OperationID: operationID, PlanID: plan.ID, Version: plan.Version, PreviousVersion: state.ActiveVersion, Phase: "staging", UpdatedAt: time.Now().UTC()}
	if err := writeAtomicJSON(ctx, filepath.Join(m.root, JournalName), journal); err != nil {
		return Result{}, fmt.Errorf("写入更新 journal 失败：%w", err)
	}
	versionsRoot := filepath.Join(m.root, VersionDir)
	stage := filepath.Join(versionsRoot, ".staging-"+operationID)
	if err := ensureDirectory(stage); err != nil {
		return Result{}, err
	}
	var stagingManifest maintenance.ArtifactManifest
	if m.registerArtifact != nil {
		registered, registerErr := m.registerArtifact(ctx, maintenance.ArtifactManifest{
			Kind: maintenance.ArtifactStaging, TargetPath: stage, CreatedAt: time.Now().UTC(), Owner: "update",
			OperationID: operationID, LeaseUntil: time.Now().UTC().Add(24 * time.Hour), Source: "版本切换 staging",
			Rebuildable: true, Risk: "high", RetentionReason: "等待版本切换完成或失败恢复后再判断", WorkspaceRoot: m.root,
		})
		if registerErr != nil {
			_ = os.RemoveAll(stage)
			return Result{}, fmt.Errorf("登记更新 staging 失败：%w", registerErr)
		}
		stagingManifest = registered
		journal.ArtifactID = registered.ArtifactID
		if err := writeAtomicJSON(ctx, filepath.Join(m.root, JournalName), journal); err != nil {
			_ = os.RemoveAll(stage)
			return Result{}, fmt.Errorf("更新更新 staging journal 失败：%w", err)
		}
	}
	committed := false
	failurePath := stage
	defer func() {
		if committed {
			return
		}
		preserveFailure := false
		if m.registerArtifact != nil && stagingManifest.ArtifactID != "" {
			if _, statErr := os.Lstat(failurePath); statErr == nil {
				stagingManifest.TargetPath = failurePath
				stagingManifest.Kind = maintenance.ArtifactFailedArchive
				stagingManifest.State = "failed"
				stagingManifest.LeaseUntil = time.Time{}
				stagingManifest.ExpiresAt = time.Time{}
				stagingManifest.Rebuildable = true
				stagingManifest.Risk = "high"
				stagingManifest.RetentionReason = "版本切换失败；保留失败 staging 供审计，等待 failed retention 到期"
				_, registerErr := m.registerArtifact(context.Background(), stagingManifest)
				preserveFailure = registerErr == nil
			}
		}
		if !preserveFailure {
			_ = os.RemoveAll(stage)
			if failurePath != stage && failurePath != "" {
				if _, statErr := os.Lstat(failurePath); statErr == nil {
					_ = os.RemoveAll(failurePath)
				}
			}
		}
	}()
	if err := copyBundle(ctx, plan, stage); err != nil {
		m.recordFailure(ctx, journal, err)
		return Result{}, err
	}
	if err := health(ctx, stage); err != nil {
		m.recordFailure(ctx, journal, fmt.Errorf("候选版本健康检查失败：%w", err))
		return Result{}, fmt.Errorf("候选版本健康检查失败：%w", err)
	}
	// 预检可能较久，发布方在此期间撤回版本时不能继续提交。
	if err := authorizeOnlineSource(ctx, m.online, plan.Online); err != nil {
		m.recordFailure(ctx, journal, err)
		return Result{}, err
	}
	finalRoot := filepath.Join(versionsRoot, safeVersionName(plan.Version))
	if _, err := os.Lstat(finalRoot); err == nil {
		m.recordFailure(ctx, journal, fmt.Errorf("版本目录已存在：%s", plan.Version))
		return Result{}, fmt.Errorf("版本目录已存在：%s", plan.Version)
	} else if !os.IsNotExist(err) {
		m.recordFailure(ctx, journal, err)
		return Result{}, err
	}
	// 在目录移动前记录意图；若 rename 成功后立即断电，恢复仍能定位新目录。
	journal.Phase = "publishing"
	journal.UpdatedAt = time.Now().UTC()
	if err := writeAtomicJSON(ctx, filepath.Join(m.root, JournalName), journal); err != nil {
		m.recordFailure(ctx, journal, err)
		return Result{}, err
	}
	if err := os.Rename(stage, finalRoot); err != nil {
		m.recordFailure(ctx, journal, fmt.Errorf("发布版本目录失败：%w", err))
		return Result{}, fmt.Errorf("发布版本目录失败：%w", err)
	}
	failurePath = finalRoot
	journal.Phase = "published"
	journal.UpdatedAt = time.Now().UTC()
	if err := writeAtomicJSON(ctx, filepath.Join(m.root, JournalName), journal); err != nil {
		if m.registerArtifact == nil {
			_ = os.RemoveAll(finalRoot)
		}
		m.recordFailure(ctx, journal, err)
		return Result{}, fmt.Errorf("更新发布 journal 失败：%w", err)
	}
	next := State{InstalledVersion: state.InstalledVersion, ActiveVersion: plan.Version, PreviousVersion: state.ActiveVersion, RejectedVersion: state.RejectedVersion, Online: plan.Online, CleanupRetentionMinutes: state.CleanupRetentionMinutes, LastOperationID: operationID, LastStatus: "activated", UpdatedAt: time.Now().UTC()}
	if err := m.writeState(ctx, next); err != nil {
		if m.registerArtifact == nil {
			_ = os.RemoveAll(finalRoot)
		}
		m.recordFailure(ctx, journal, err)
		return Result{}, fmt.Errorf("提交当前版本状态失败：%w", err)
	}
	// 当前版本指针已经原子提交；后续清理 journal 或登记失败不得再删除已激活版本。
	committed = true
	if err := os.Remove(m.planPath(plan.ID)); err != nil && !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("更新已提交，计划清理待恢复：%w", err)
	}
	if err := os.Remove(filepath.Join(m.root, JournalName)); err != nil && !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("清理更新 journal 失败：%w", err)
	}
	if m.registerArtifact != nil && stagingManifest.ArtifactID != "" {
		stagingManifest.TargetPath = finalRoot
		stagingManifest.State = "published"
		stagingManifest.LeaseUntil = time.Time{}
		stagingManifest.ExpiresAt = time.Time{}
		stagingManifest.Rebuildable = false
		stagingManifest.RetentionReason = "已发布版本由更新管理器生命周期管理"
		if _, registerErr := m.registerArtifact(context.Background(), stagingManifest); registerErr != nil {
			return Result{}, fmt.Errorf("更新已发布版本登记失败：%w", registerErr)
		}
	}
	return Result{OperationID: operationID, Action: "activate", ActiveVersion: next.ActiveVersion, PreviousVersion: next.PreviousVersion, VersionRoot: finalRoot, Verified: true, HealthChecked: true, CompletedAt: time.Now().UTC()}, nil
}

// Rollback 将当前版本指针切换回上一版本；旧版本目录保留供人工检查。
func (m *Manager) Rollback(ctx context.Context, health HealthCheck) (result Result, err error) {
	if m == nil {
		return Result{}, errors.New("更新管理器未初始化")
	}
	finish := BeginDiagnostic(m.root, "rollback", "")
	defer func() { finish(err) }()
	if health == nil {
		return Result{}, errors.New("回滚必须提供健康检查")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err := m.recoverLocked(ctx); err != nil {
		return Result{}, err
	}
	state, err := m.readState()
	if err != nil {
		return Result{}, err
	}
	if state.ActiveVersion == "" || state.PreviousVersion == "" {
		return Result{}, errors.New("没有可回滚的旧版本")
	}
	previousRoot := filepath.Join(m.root, VersionDir, safeVersionName(state.PreviousVersion))
	if err := ensureExistingDirectory(previousRoot); err != nil {
		return Result{}, fmt.Errorf("旧版本不可用：%w", err)
	}
	if err := health(ctx, previousRoot); err != nil {
		return Result{}, fmt.Errorf("旧版本健康检查失败：%w", err)
	}
	id, err := newID("rollback")
	if err != nil {
		return Result{}, err
	}
	next := State{InstalledVersion: state.InstalledVersion, ActiveVersion: state.PreviousVersion, PreviousVersion: state.ActiveVersion, RejectedVersion: state.ActiveVersion, CleanupRetentionMinutes: state.CleanupRetentionMinutes, LastOperationID: id, LastStatus: "rolled_back", UpdatedAt: time.Now().UTC()}
	if err := m.writeState(ctx, next); err != nil {
		return Result{}, fmt.Errorf("提交回滚状态失败：%w", err)
	}
	return Result{OperationID: id, Action: "rollback", ActiveVersion: next.ActiveVersion, PreviousVersion: next.PreviousVersion, VersionRoot: previousRoot, Verified: true, HealthChecked: true, CompletedAt: time.Now().UTC()}, nil
}

// Status 读取当前版本、待确认计划和恢复状态；它不会启动切换。
func (m *Manager) Status(ctx context.Context) (Status, error) {
	if m == nil {
		return Status{}, errors.New("更新管理器未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.readState()
	if err != nil {
		return Status{}, err
	}
	recoveryRequired, recoveryMessage := m.journalStatus()
	versions, err := m.listVersions()
	if err != nil {
		return Status{}, err
	}
	plans, err := m.listPlans()
	if err != nil {
		return Status{}, err
	}
	var cleanup CleanupResult
	if data, err := os.ReadFile(filepath.Join(m.root, "last-cleanup.json")); err == nil {
		_ = json.Unmarshal(data, &cleanup)
	}
	status := Status{InstallRoot: m.root, State: state, Versions: versions, PendingPlanIDs: plans, RecoveryRequired: recoveryRequired, RecoveryMessage: recoveryMessage, ReadOnlySource: true, SignatureRequired: m.requireSignature, TrustedKeyCount: len(m.trustedPublicKeys), OnlineConfigured: m.online.FeedURL != "", OnlineFeedURL: m.online.FeedURL, OnlineProxyURL: m.online.ProxyURL}
	status.Automatic, err = m.automaticStatus()
	status.Download = m.downloadProgress
	status.LastDiagnostic = lastDiagnosticFailure(m.root)
	if err != nil {
		return Status{}, err
	}
	if !cleanup.CompletedAt.IsZero() {
		status.LastCleanup = &cleanup
	}
	return status, nil
}

// VerifyVersionRoot 只读验证一个已发布版本目录的清单和全部文件摘要。
func VerifyVersionRoot(ctx context.Context, root string) error {
	abs, err := safeExistingDirectory(root)
	if err != nil {
		return err
	}
	_, _, err = readAndValidateManifest(ctx, abs)
	return err
}

// Recover 清理或补偿上一次进程中断留下的 staging/journal；无法判断时 fail-closed。
func (m *Manager) Recover(ctx context.Context) error {
	if m == nil {
		return errors.New("更新管理器未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return m.recoverLocked(ctx)
}

type journal struct {
	OperationID     string    `json:"operation_id"`
	PlanID          string    `json:"plan_id"`
	ArtifactID      string    `json:"artifact_id,omitempty"`
	Version         string    `json:"version"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	Phase           string    `json:"phase"`
	UpdatedAt       time.Time `json:"updated_at"`
	Error           string    `json:"error,omitempty"`
}

func (m *Manager) updateRegisteredArtifact(ctx context.Context, entry journal, target string, state string, rebuildable bool, reason string) error {
	if m == nil || m.registerArtifact == nil || entry.ArtifactID == "" || strings.TrimSpace(target) == "" {
		return nil
	}
	_, err := m.registerArtifact(ctx, maintenance.ArtifactManifest{
		Version: 1, ArtifactID: entry.ArtifactID, Kind: maintenance.ArtifactStaging, TargetPath: target,
		CreatedAt: entry.UpdatedAt, Owner: "update", OperationID: entry.OperationID, Source: "版本切换 staging",
		Rebuildable: rebuildable, Risk: "high", RetentionReason: reason, State: state,
		WorkspaceRoot: m.root,
	})
	return err
}

func (m *Manager) validatePlanLocked(ctx context.Context, plan Plan) error {
	persisted, err := m.LoadPlan(ctx, plan.ID)
	if err != nil {
		return err
	}
	if !onlineSourcesEqual(persisted.Online, plan.Online) || persisted.Automatic != plan.Automatic {
		return errors.New("更新来源已变化，更新计划失效")
	}
	if plan.Automatic && plan.Online == nil {
		return fmt.Errorf("%w：旧在线计划缺少来源记录，请重新准备更新", ErrOnlineReleaseWithdrawn)
	}
	manifest, digest, err := m.readAndValidateManifest(ctx, plan.BundleRoot)
	if err != nil {
		return err
	}
	if digest != plan.ManifestSHA256 || manifest.Version != plan.Version || !manifestsEqual(manifest, plan.Manifest) {
		return errors.New("离线 bundle 已变化，更新计划失效")
	}
	state, err := m.readState()
	if err != nil {
		return err
	}
	if state.ActiveVersion != plan.PreviousVersion {
		return errors.New("当前版本已变化，更新计划失效")
	}
	return nil
}

func copyBundle(ctx context.Context, plan Plan, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, file := range plan.Manifest.Files {
		source := filepath.Join(plan.BundleRoot, filepath.FromSlash(file.Path))
		target := filepath.Join(destination, filepath.FromSlash(file.Path))
		if err := copyVerifiedFile(ctx, source, target, file); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(filepath.Join(plan.BundleRoot, ManifestName))
	if err != nil {
		return fmt.Errorf("读取 bundle 清单失败：%w", err)
	}
	if err := writeAtomic(ctx, filepath.Join(destination, ManifestName), data, 0o600); err != nil {
		return err
	}
	return syncDirectory(destination)
}

func copyVerifiedFile(ctx context.Context, source, target string, expected FileEntry) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("读取 bundle 文件 %s 失败：%w", expected.Path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("bundle 文件不安全：%s", expected.Path)
	}
	if info.Size() != expected.SizeBytes {
		return fmt.Errorf("bundle 文件大小变化：%s", expected.Path)
	}
	if err := ensureParent(target); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(expected.Mode)&0o777)
	if err != nil {
		return fmt.Errorf("创建候选版本文件 %s 失败：%w", expected.Path, err)
	}
	hash := sha256.New()
	_, copyErr := io.CopyBuffer(io.MultiWriter(out, hash), in, make([]byte, 4<<20))
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("候选版本文件摘要不匹配：%s", expected.Path)
	}
	return nil
}

func (m *Manager) recoverLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.recoverDownloads(ctx); err != nil {
		return err
	}
	if err := m.recoverBaselines(); err != nil {
		return err
	}
	path := filepath.Join(m.root, JournalName)
	var entry journal
	if err := readJSON(path, &entry); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("更新 journal 损坏，拒绝继续：%w", err)
	}
	if !safeID(entry.OperationID) || !safeVersion(entry.Version) {
		return errors.New("更新 journal 身份无效，拒绝自动恢复")
	}
	state, err := m.readState()
	if err != nil {
		return err
	}
	finalRoot := filepath.Join(m.root, VersionDir, safeVersionName(entry.Version))
	stage := filepath.Join(m.root, VersionDir, ".staging-"+entry.OperationID)
	switch entry.Phase {
	case "staging":
		if err := m.updateRegisteredArtifact(ctx, entry, stage, "failed", true, "进程中断后的版本切换 staging；等待 failed retention 到期"); err != nil {
			return err
		}
		if m.registerArtifact == nil || entry.ArtifactID == "" {
			_ = os.RemoveAll(stage)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "publishing", "published":
		if state.ActiveVersion == entry.Version {
			if err := m.updateRegisteredArtifact(ctx, entry, finalRoot, "published", false, "已发布版本由更新管理器生命周期管理"); err != nil {
				return err
			}
			if safeID(entry.PlanID) {
				if err := os.Remove(m.planPath(entry.PlanID)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			_ = os.Remove(path)
			return nil
		}
		if err := m.updateRegisteredArtifact(ctx, entry, finalRoot, "failed", true, "发布 journal 未提交且当前版本未切换；等待 failed retention 到期"); err != nil {
			return err
		}
		if m.registerArtifact == nil || entry.ArtifactID == "" {
			_ = os.RemoveAll(finalRoot)
		}
		_ = os.RemoveAll(stage)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("更新 journal 处于未知阶段 %q，要求人工处理", entry.Phase)
	}
}

func (m *Manager) recordFailure(ctx context.Context, entry journal, failure error) {
	// 超时或退出取消的是安装尝试，补偿记录仍须落盘，避免后台持续运行时一直被自己的 journal 阻断。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	entry.Phase = "staging"
	entry.Error = failure.Error()
	entry.UpdatedAt = time.Now().UTC()
	if err := writeAtomicJSON(ctx, filepath.Join(m.root, JournalName), entry); err != nil {
		return
	}
	state, err := m.readState()
	if err == nil {
		state.LastOperationID = entry.OperationID
		state.LastStatus = "failed"
		state.LastError = failure.Error()
		state.UpdatedAt = time.Now().UTC()
		if m.writeState(ctx, state) == nil {
			_ = os.Remove(filepath.Join(m.root, JournalName))
		}
	}
}

func (m *Manager) readState() (State, error) {
	var state State
	err := readJSON(filepath.Join(m.root, StateName), &state)
	if os.IsNotExist(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("读取安装状态失败：%w", err)
	}
	if state.ActiveVersion != "" && !safeVersion(state.ActiveVersion) {
		return State{}, errors.New("安装状态中的当前版本无效")
	}
	if state.InstalledVersion != "" && !safeVersion(state.InstalledVersion) {
		return State{}, errors.New("安装状态中的基线版本无效")
	}
	if state.PreviousVersion != "" && !safeVersion(state.PreviousVersion) {
		return State{}, errors.New("安装状态中的旧版本无效")
	}
	if state.DeferredVersion != "" && !safeVersion(state.DeferredVersion) {
		return State{}, errors.New("安装状态中的暂缓版本无效")
	}
	if state.CleanupRetentionMinutes < 0 || state.CleanupRetentionMinutes > MaxCleanupRetentionMinutes {
		return State{}, ErrInvalidCleanupRetention
	}
	if state.Online != nil {
		if state.Online.Release.Version != state.ActiveVersion || validateOnlineRelease(state.Online.Release, 16<<30) != nil || validateOnlineURL(state.Online.FeedURL, "在线来源") != nil {
			return State{}, errors.New("安装状态中的在线来源无效")
		}
	}
	return state, nil
}

func (m *Manager) writeState(ctx context.Context, state State) error {
	if state.CleanupRetentionMinutes < 0 || state.CleanupRetentionMinutes > MaxCleanupRetentionMinutes {
		return ErrInvalidCleanupRetention
	}
	if state.ActiveVersion != "" && !safeVersion(state.ActiveVersion) {
		return errors.New("当前版本名称无效")
	}
	return writeAtomicJSON(ctx, filepath.Join(m.root, StateName), state)
}

func (m *Manager) planPath(id string) string {
	return filepath.Join(m.root, PlanDirName, "plan-"+id+".json")
}

func (m *Manager) listPlans() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.root, PlanDirName))
	if err != nil {
		return nil, err
	}
	plans := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "plan-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "plan-"), ".json")
		if safeID(id) {
			plans = append(plans, id)
		}
	}
	sort.Strings(plans)
	return plans, nil
}

func (m *Manager) listVersions() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.root, VersionDir))
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !safeVersion(entry.Name()) {
			continue
		}
		info, err := os.Lstat(filepath.Join(m.root, VersionDir, entry.Name()))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		versions = append(versions, entry.Name())
	}
	sort.Strings(versions)
	return versions, nil
}

func (m *Manager) journalStatus() (bool, string) {
	var entry journal
	if err := readJSON(filepath.Join(m.root, JournalName), &entry); err != nil {
		if os.IsNotExist(err) {
			return false, ""
		}
		return true, "更新 journal 无法读取：" + err.Error()
	}
	return true, fmt.Sprintf("上次更新在 %s 阶段中断，必须先恢复或人工检查", entry.Phase)
}

func readAndValidateManifest(ctx context.Context, root string) (Manifest, string, error) {
	return readAndValidateManifestWithSignature(ctx, root, false, nil)
}

func (m *Manager) readAndValidateManifest(ctx context.Context, root string) (Manifest, string, error) {
	return readAndValidateManifestWithSignature(ctx, root, m.requireSignature, m.trustedPublicKeys)
}

func readAndValidateManifestWithSignature(ctx context.Context, root string, requireSignature bool, trustedPublicKeys [][]byte) (Manifest, string, error) {
	path := filepath.Join(root, ManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("读取离线清单失败：%w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, "", fmt.Errorf("离线清单不是有效 JSON：%w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, "", err
	}
	if requireSignature {
		if err := VerifyManifestSignature(manifest, trustedPublicKeys); err != nil {
			return Manifest{}, "", fmt.Errorf("离线清单供应链校验失败：%w", err)
		}
	}
	if manifest.OS != runtime.GOOS || manifest.Arch != runtime.GOARCH {
		return Manifest{}, "", fmt.Errorf("离线包目标平台 %s/%s 与当前 %s/%s 不匹配", manifest.OS, manifest.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if err := validateBundleFiles(ctx, root, manifest); err != nil {
		return Manifest{}, "", err
	}
	digest := sha256.Sum256(data)
	return manifest, hex.EncodeToString(digest[:]), nil
}

func validateManifest(manifest Manifest) error {
	if manifest.FormatVersion != ManifestVersion {
		return fmt.Errorf("不支持的离线清单版本：%d", manifest.FormatVersion)
	}
	if strings.TrimSpace(manifest.Version) == "" || !safeVersion(manifest.Version) {
		return errors.New("离线清单版本无效")
	}
	if strings.TrimSpace(manifest.OS) == "" || strings.TrimSpace(manifest.Arch) == "" {
		return errors.New("离线清单缺少目标平台")
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		clean, err := safeRelative(file.Path)
		if err != nil || clean == ManifestName {
			return fmt.Errorf("离线清单文件路径无效：%s", file.Path)
		}
		if clean != filepath.ToSlash(file.Path) {
			return fmt.Errorf("离线清单文件路径未规范化：%s", file.Path)
		}
		if _, ok := seen[clean]; ok {
			return fmt.Errorf("离线清单存在重复文件：%s", clean)
		}
		seen[clean] = struct{}{}
		if file.SizeBytes < 0 || !validSHA256(file.SHA256) {
			return fmt.Errorf("离线清单文件摘要无效：%s", clean)
		}
	}
	if manifest.Signature != nil {
		if _, _, err := validateManifestSignatureSyntax(manifest.Signature); err != nil {
			return fmt.Errorf("离线清单签名格式无效：%w", err)
		}
	}
	return nil
}

func validateBundleFiles(ctx context.Context, root string, manifest Manifest) error {
	expected := make(map[string]FileEntry, len(manifest.Files))
	for _, file := range manifest.Files {
		expected[file.Path] = file
		path := filepath.Join(root, filepath.FromSlash(file.Path))
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("离线包缺少文件：%s", file.Path)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("离线包文件不安全：%s", file.Path)
		}
		if info.Size() != file.SizeBytes {
			return fmt.Errorf("离线包文件大小不匹配：%s", file.Path)
		}
		digest, _, err := hashFile(ctx, path)
		if err != nil || digest != file.SHA256 {
			if err != nil {
				return err
			}
			return fmt.Errorf("离线包文件摘要不匹配：%s", file.Path)
		}
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("离线包含有符号链接：%s", rel)
		}
		if _, ok := expected[rel]; !ok {
			return fmt.Errorf("离线包存在未列入清单的文件：%s", rel)
		}
		return nil
	})
}

func manifestsEqual(left, right Manifest) bool {
	leftData, _ := json.Marshal(left)
	rightData, _ := json.Marshal(right)
	return string(leftData) == string(rightData)
}

func safeRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("安装根不能为空")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if hasCodexComponent(abs) {
		return "", errors.New("安装根不能位于 .codex 路径")
	}
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("安装根必须是普通目录")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	} else if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return abs, nil
}

func safeExistingDirectory(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if hasCodexComponent(abs) {
		return "", errors.New("路径不能位于 .codex 路径")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("路径必须是普通目录")
	}
	return abs, nil
}

func ensureDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("路径不是安全目录：%s", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(path, 0o700)
}

func ensureExistingDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("版本目录不是普通目录")
	}
	return nil
}

func ensureParent(path string) error {
	parent := filepath.Dir(path)
	if err := ensureDirectory(parent); err != nil {
		return err
	}
	return nil
}

func writeAtomicJSON(ctx context.Context, path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(ctx, path, append(data, '\n'), 0o600)
}

func writeAtomic(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureParent(path); err != nil {
		return err
	}
	temporary := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := replaceFile(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func hashFile(ctx context.Context, path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	var size int64
	buffer := make([]byte, 4<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", size, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if _, err := hash.Write(buffer[:count]); err != nil {
				return "", size, err
			}
			size += int64(count)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", size, readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func newID(prefix string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(random[:]), nil
}

func safeID(value string) bool {
	return value != "" && len(value) <= 100 && filepath.Base(value) == value && !strings.ContainsAny(value, `/\\`)
}

func safeVersion(value string) bool {
	if strings.TrimSpace(value) == "" || len(value) > 120 || filepath.Base(value) != value || strings.ContainsAny(value, `/\\`) || strings.ContainsAny(value, "\x00") {
		return false
	}
	return value != "." && value != ".."
}

func safeVersionName(value string) string {
	value = strings.TrimSpace(value)
	if safeVersion(value) {
		return value
	}
	return "invalid-version"
}

func safeRelative(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return "", errors.New("文件路径必须是相对 POSIX 路径")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", errors.New("文件路径越出 bundle 根")
	}
	return clean, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func hasCodexComponent(path string) bool {
	for _, part := range strings.FieldsFunc(filepath.Clean(path), func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return false
}

func versionLess(left, right string) bool {
	left = strings.TrimPrefix(strings.TrimSpace(left), "v")
	right = strings.TrimPrefix(strings.TrimSpace(right), "v")
	left, _, _ = strings.Cut(left, "+")
	right, _, _ = strings.Cut(right, "+")
	leftParts := strings.SplitN(left, "-", 2)
	rightParts := strings.SplitN(right, "-", 2)
	leftNumbers := strings.Split(leftParts[0], ".")
	rightNumbers := strings.Split(rightParts[0], ".")
	for i := 0; i < len(leftNumbers) || i < len(rightNumbers); i++ {
		leftValue, rightValue := 0, 0
		if i < len(leftNumbers) {
			leftValue, _ = strconv.Atoi(leftNumbers[i])
		}
		if i < len(rightNumbers) {
			rightValue, _ = strconv.Atoi(rightNumbers[i])
		}
		if leftValue != rightValue {
			return leftValue < rightValue
		}
	}
	if len(leftParts) == 1 || len(rightParts) == 1 {
		return len(leftParts) > len(rightParts)
	}
	leftIDs, rightIDs := strings.Split(leftParts[1], "."), strings.Split(rightParts[1], ".")
	for i := 0; i < len(leftIDs) && i < len(rightIDs); i++ {
		l, r := leftIDs[i], rightIDs[i]
		if l == r {
			continue
		}
		lNumeric, rNumeric := numericVersionID(l), numericVersionID(r)
		if lNumeric != rNumeric {
			return lNumeric
		}
		if lNumeric && len(l) != len(r) {
			return len(l) < len(r)
		}
		return l < r
	}
	return len(leftIDs) < len(rightIDs)
}

func numericVersionID(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
