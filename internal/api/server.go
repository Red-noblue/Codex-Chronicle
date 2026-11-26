// 中文说明：提供绑定固定扫描根的本地 REST 边界；源 .codex 默认只读，写入仅经显式人工确认和隔离目标。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/archive"
	"codex-chronicle.local/internal/auth"
	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/compat/authorizedlive"
	copypkg "codex-chronicle.local/internal/copy"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
	maintenancepkg "codex-chronicle.local/internal/maintenance"
	"codex-chronicle.local/internal/operation"
	pendingpkg "codex-chronicle.local/internal/pending"
	"codex-chronicle.local/internal/platform"
	projectpkg "codex-chronicle.local/internal/project"
	resourcepkg "codex-chronicle.local/internal/resource"
	"codex-chronicle.local/internal/store"
	summarypkg "codex-chronicle.local/internal/summary"
	updatepkg "codex-chronicle.local/internal/update"
	workspacepkg "codex-chronicle.local/internal/workspace"
)

// Server 是绑定到一个固定扫描根的本地只读 API 服务。
type Server struct {
	version                string
	drainMu                sync.Mutex
	draining               bool
	activeRequests         int
	updateLastActivity     time.Time
	updateAutomaticRestart func(context.Context, string) error
	scanner                application.Scanner
	root                   string
	archive                *store.Store
	workspaceMu            sync.RWMutex
	workspaceStore         *workspacepkg.FileStore
	workspaceReg           workspacepkg.Registry
	workspaceCurrent       string
	workspaceArchives      map[string]*store.Store
	executionMu            sync.Mutex
	executionReviews       map[string]*executionReview
	executionConfirmations map[string]*executionConfirmation
	executionTasks         map[string]*executionTask
	executionActive        string
	executionLoadErrors    []string
	restrictedPreviewMu    sync.Mutex
	batchReviewMu          sync.RWMutex
	batchReview            *batchOperationReview
	tasks                  *application.TaskManager
	summaryTasks           *application.SummaryTaskManager
	summaryRegistry        summarypkg.Registry
	lifecycle              *LifecycleOptions
	remoteConfigMu         sync.RWMutex
	githubSettings         *githubSettingsState
	cloudMu                sync.Mutex
	cloud                  *cloudBackupState
	archiveCatalogMu       sync.Mutex
	archiveCatalogCache    map[string]archiveRemoteSnapshot
	registry               compat.Registry
	updatePackageCheck     updatepkg.HealthCheck
	remoteDownloadMu       sync.Mutex
	remoteDownloads        map[string]*remoteDownloadPlan
	remoteRestoreMu        sync.Mutex
	remoteRestores         map[string]*remoteRestorePlan
	localRestoreMu         sync.Mutex
	localRestorePlans      map[string]*localArchiveRestorePlan
	remoteDeletesMu        sync.Mutex
	remoteDeletes          map[string]*remoteDeletePlan
	maintenanceMu          sync.Mutex
	maintenancePlans       map[string]maintenancepkg.Plan
	cleanupMu              sync.Mutex
	cleanupSettingsMu      sync.Mutex
	cleanupPlans           map[string]maintenancepkg.CleanupPlan
	cleanupReportMu        sync.RWMutex
	cleanupReport          maintenancepkg.CleanupReport
	cleanupReportError     string
	cleanupScannedAt       time.Time
	cleanupCancel          context.CancelFunc
	pendingMu              sync.Mutex
	pendingStores          map[string]*pendingpkg.Store
	resourceMu             sync.RWMutex
	resourceBudget         resourcepkg.Budget
	indexTaskMu            sync.Mutex
	indexTask              searchIndexTask
	indexTaskCancel        context.CancelFunc
	indexTaskDone          chan struct{}
	trashSettings          *platform.TrashSettingsStore
	trashSettingsErr       error
	uiPreferencesMu        sync.Mutex
	uiPreferencesPath      string
	compatibilityMu        sync.Mutex
	compatibilityPath      string
	sourcePolicies         map[string]sourcePolicy
	sourceChanges          map[string]int
	trashProbeMu           sync.Mutex
	authManager            *auth.Manager
	copyTasks              *copypkg.Manager
	liveCopyTasks          *copypkg.LiveManager
	updater                *updatepkg.Manager
	updateRestart          func(context.Context, string) error
	recoveryMu             sync.RWMutex
	recoveryCandidates     []operation.RecoveryCandidate
	recoveryErrors         []string
	recoveryScannedAt      time.Time
}

// LifecycleOptions 是显式注入的写执行配置；默认 nil，保证 daemon 只读。
// FixtureOnly 为 true 时还会拒绝任何路径组件为 .codex 的源根。
type LifecycleOptions struct {
	AuthorizedLive                 *authorizedlive.Config
	Enabled                        bool
	FixtureOnly                    bool
	Descriptor                     compat.Descriptor
	ControlPlaneMutation           compat.ControlPlaneMutation
	ArchiveRoot                    string
	JournalRoot                    string
	TrashRoot                      string
	CleanupRoot                    string
	TrashProvider                  platform.TrashProvider
	UseSystemTrash                 bool
	RequireDestinationConfirmation bool
	ActivityOptions                *activity.Options
	CleanupOpenFileProbe           func(context.Context, string) (bool, bool, string)
	// CleanupScanInterval 只定期刷新只读候选，不会自动执行移动、删除或恢复。
	CleanupScanInterval  time.Duration
	GitHubAuth           *archive.GitHubDeviceAuthService
	GitHubRepositories   archive.GitHubRepositoryCatalog
	RemoteArchive        archive.RemoteArchiveProvider
	RemoteArchiveID      string
	RemoteArchiveCatalog archive.RemoteArchiveCatalog
	RemoteRecoveryRoot   string
}

// NewServer 创建 API 服务。root 为空时仍可提供健康和能力查询，但不开放 inventory。
func NewServer(root string) *Server {
	return NewServerWithStore(root, nil)
}

// NewServerWithStore 创建绑定独立 archive.db 的本地 API 服务。
func NewServerWithStore(root string, archive *store.Store) *Server {
	return NewServerWithOptions(root, archive, nil)
}

// NewServerWithOptions 创建本地 API；只有显式提供并启用 LifecycleOptions 才注册实际写执行能力。
func NewServerWithOptions(root string, archive *store.Store, lifecycleOptions *LifecycleOptions) *Server {
	return NewServerWithOptionsAndRegistry(root, archive, lifecycleOptions, compat.DefaultRegistry())
}

// NewServerWithOptionsAndRegistry 创建绑定显式兼容 registry 的本地 API，供正式 adapter 和二次开发注入。
func NewServerWithOptionsAndRegistry(root string, archive *store.Store, lifecycleOptions *LifecycleOptions, registry compat.Registry) *Server {
	scanner := application.NewScanner()
	budget, _ := (resourcepkg.Budget{}).Normalize()
	trashSettings, trashSettingsErr := platform.DefaultTrashSettingsStore()
	copyPlanRoot := ""
	if strings.TrimSpace(root) != "" {
		copyPlanRoot = filepath.Join(filepath.Dir(filepath.Clean(root)), ".codex-chronicle-copy-plans")
	} else if archive != nil && archive.Path() != ":memory:" {
		copyPlanRoot = filepath.Join(filepath.Dir(archive.Path()), ".codex-chronicle-copy-plans")
	}
	copyTasks := copypkg.NewManagerWithPlanRoot(copyPlanRoot)
	summaryRegistry := summarypkg.DefaultRegistry()
	server := &Server{scanner: scanner, root: root, archive: archive, tasks: application.NewTaskManager(scanner), summaryTasks: application.NewSummaryTaskManager(scanner, summaryRegistry), summaryRegistry: summaryRegistry, copyTasks: copyTasks, liveCopyTasks: copypkg.NewLiveManager(), lifecycle: lifecycleOptions, registry: registry, remoteDownloads: make(map[string]*remoteDownloadPlan), remoteRestores: make(map[string]*remoteRestorePlan), localRestorePlans: make(map[string]*localArchiveRestorePlan), remoteDeletes: make(map[string]*remoteDeletePlan), maintenancePlans: make(map[string]maintenancepkg.Plan), cleanupPlans: make(map[string]maintenancepkg.CleanupPlan), pendingStores: make(map[string]*pendingpkg.Store), workspaceArchives: make(map[string]*store.Store), resourceBudget: budget, trashSettings: trashSettings, trashSettingsErr: trashSettingsErr}
	server.executionReviews = make(map[string]*executionReview)
	server.executionConfirmations = make(map[string]*executionConfirmation)
	server.executionTasks = make(map[string]*executionTask)
	server.loadExecutionTasks()
	server.initializeLegacyWorkspace(archive)
	server.refreshRecoveryCatalog(context.Background())
	server.refreshCleanupCatalog(context.Background())
	server.startCleanupScanner()
	return server
}

// Close 停止后台候选扫描器；不会删除或修改任何 registry、源目录或生成物。
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.indexTaskMu.Lock()
	if s.indexTaskCancel != nil {
		s.indexTaskCancel()
	}
	done := s.indexTaskDone
	s.indexTaskMu.Unlock()
	if done != nil {
		<-done
	}
	s.closeCloudBackups()
	if s.cleanupCancel != nil {
		s.cleanupCancel()
		s.cleanupCancel = nil
	}
	if s.githubSettings != nil && s.githubSettings.auth != nil {
		s.githubSettings.auth.Close()
	}
}

func (s *Server) startCleanupScanner() {
	if s == nil || s.lifecycle == nil || s.lifecycle.CleanupScanInterval <= 0 || strings.TrimSpace(s.cleanupRoot()) == "" {
		return
	}
	interval := s.lifecycle.CleanupScanInterval
	if interval < time.Second {
		interval = time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cleanupCancel = cancel
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshCleanupCatalog(ctx)
			}
		}
	}()
}

// SetSummaryRegistry 在 daemon 开始接受请求前替换摘要 provider；未注入时仍只有本地 provider。
func (s *Server) SetSummaryRegistry(registry summarypkg.Registry) {
	if s == nil {
		return
	}
	s.summaryRegistry = registry
	s.summaryTasks = application.NewSummaryTaskManager(s.scanner, registry)
}

// SetResourceBudget 在 daemon 启动完成、开始接受请求前设置派生数据预算。
func (s *Server) SetResourceBudget(budget resourcepkg.Budget) error {
	normalized, err := budget.Normalize()
	if err != nil {
		return err
	}
	s.resourceMu.Lock()
	s.resourceBudget = normalized
	s.resourceMu.Unlock()
	if normalized.MemoryLimitBytes > 0 {
		debug.SetMemoryLimit(normalized.MemoryLimitBytes)
	}
	return nil
}

// SetAuthManager 启用远程网页访问认证；应在 daemon 开始监听前调用。
func (s *Server) SetAuthManager(manager *auth.Manager) {
	s.authManager = manager
}

// SetUpdateManager 注入独立安装根的离线更新管理器；未注入时不开放更新写入口。
func (s *Server) SetUpdateManager(manager *updatepkg.Manager) {
	if s == nil {
		return
	}
	s.updater = manager
	if manager != nil {
		manager.SetArtifactRegistrar(s.cleanupArtifactRegistrar())
	}
}

// SetUpdatePackageCheck 在实际进程入口注入候选包隔离启动检查；测试用的纯数据 bundle 不执行程序。
func (s *Server) SetUpdatePackageCheck(check updatepkg.HealthCheck) {
	s.updatePackageCheck = check
}

// SetUpdateRestart 仅由受桌面启动器管理的后台注入；手动运行的开发服务仍由部署者控制重启。
func (s *Server) SetUpdateRestart(restart func(context.Context, string) error) {
	s.updateRestart = restart
}

func (s *Server) SetAutomaticUpdateRestart(restart func(context.Context, string) error) {
	s.updateAutomaticRestart = restart
}

// SetTrashSettingsStore 注入用户级回收站设置存储；应在 daemon 开始接受请求前调用。
func (s *Server) SetTrashSettingsStore(settings *platform.TrashSettingsStore) {
	if s == nil {
		return
	}
	s.trashSettings = settings
	s.trashSettingsErr = nil
}

func (s *Server) resourceBudgetSnapshot() resourcepkg.Budget {
	s.resourceMu.RLock()
	defer s.resourceMu.RUnlock()
	return s.resourceBudget
}

func (s *Server) resourcePaths() resourcepkg.Paths {
	workspace := s.currentWorkspace()
	paths := resourcepkg.Paths{Persistent: []string{archivePath(workspace.Archive), s.workspaceRegistryPath()}}
	if workspace.Archive != nil {
		paths.Persistent = append(paths.Persistent, workspace.Archive.SearchIndexPath())
	}
	if archive := archivePath(workspace.Archive); archive != "" {
		paths.Persistent = append(paths.Persistent, store.DefaultMigrationBackupRoot(archive))
	}
	if strings.TrimSpace(workspace.Definition.Root) != "" {
		paths.Persistent = append(paths.Persistent, copyPlanRootForWorkspace(workspace.Definition.Root))
		paths.Persistent = append(paths.Persistent, pendingpkg.DefaultRoot(workspace.Definition.Root, archivePath(workspace.Archive)))
	}
	if s.updater != nil {
		paths.Persistent = append(paths.Persistent, s.updater.Root())
	}
	if s.lifecycle != nil {
		paths.Persistent = append(paths.Persistent, s.lifecycle.ArchiveRoot)
		cleanupRoot := s.lifecycle.CleanupRoot
		if strings.TrimSpace(cleanupRoot) == "" && strings.TrimSpace(s.lifecycle.JournalRoot) != "" {
			cleanupRoot = filepath.Join(s.lifecycle.JournalRoot, "cleanup-registry")
		}
		paths.Temporary = append(paths.Temporary, s.lifecycle.JournalRoot, s.lifecycle.TrashRoot, cleanupRoot, s.lifecycle.RemoteRecoveryRoot)
		if s.lifecycle.JournalRoot != "" {
			paths.Temporary = append(paths.Temporary, filepath.Join(filepath.Dir(s.lifecycle.JournalRoot), "cloud-backups"))
		}
	}
	return paths
}

func (s *Server) inspectResources(ctx context.Context) resourcepkg.Snapshot {
	return s.resourceBudgetSnapshot().Inspect(ctx, s.resourcePaths())
}

func (s *Server) checkResources(ctx context.Context) (resourcepkg.Snapshot, error) {
	return s.resourceBudgetSnapshot().Check(ctx, s.resourcePaths())
}

func (s *Server) checkResourcesAdditional(ctx context.Context, persistent, temporary int64) (resourcepkg.Snapshot, error) {
	return s.resourceBudgetSnapshot().CheckAdditional(ctx, s.resourcePaths(), persistent, temporary)
}

// NewServerWithWorkspaceConfig 创建带持久化工作区注册表的 daemon。
// configPath 位于 .codex 外部；已有 scanRoot 会作为初始工作区注册，不会复制或修改源文件。
func NewServerWithWorkspaceConfig(root string, archive *store.Store, lifecycleOptions *LifecycleOptions, configPath string) (*Server, error) {
	server := NewServerWithOptions(root, archive, lifecycleOptions)
	if strings.TrimSpace(configPath) == "" {
		return server, nil
	}
	if err := server.configureWorkspaceStore(context.Background(), configPath); err != nil {
		return nil, err
	}
	return server, nil
}

type workspaceSnapshot struct {
	Definition workspacepkg.Definition
	Archive    *store.Store
}

func (s *Server) initializeLegacyWorkspace(archive *store.Store) {
	s.workspaceReg = workspacepkg.Registry{Version: 1, Workspaces: make([]workspacepkg.Definition, 0)}
	if strings.TrimSpace(s.root) == "" {
		return
	}
	definition, err := workspacepkg.NormalizeDefinition(workspacepkg.Definition{Root: s.root, ArchiveDB: archivePath(archive)})
	if err != nil {
		return
	}
	s.workspaceReg.Workspaces = append(s.workspaceReg.Workspaces, definition)
	s.workspaceReg.CurrentID = definition.ID
	s.workspaceCurrent = definition.ID
}

func (s *Server) configureWorkspaceStore(ctx context.Context, path string) error {
	workspaceStore, err := workspacepkg.NewFileStore(path)
	if err != nil {
		return err
	}
	registry, err := workspaceStore.Load(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(s.root) != "" && pathWithin(s.root, workspaceStore.Path()) {
		return fmt.Errorf("工作区注册表必须位于扫描根之外")
	}
	for _, definition := range registry.Workspaces {
		if pathWithin(definition.Root, workspaceStore.Path()) {
			return fmt.Errorf("工作区注册表必须位于所有源目录之外")
		}
	}
	if len(registry.Workspaces) == 0 && strings.TrimSpace(s.root) != "" {
		definition, normalizeErr := workspacepkg.NormalizeDefinition(workspacepkg.Definition{Root: s.root, ArchiveDB: archivePath(s.archive)})
		if normalizeErr != nil {
			return normalizeErr
		}
		registry.Workspaces = append(registry.Workspaces, definition)
		registry.CurrentID = definition.ID
		if err := workspaceStore.Save(ctx, registry); err != nil {
			return err
		}
	}
	if err := workspacepkg.Validate(ctx, &registry); err != nil {
		return err
	}
	if registry.CurrentID != "" {
		definition, ok := workspaceDefinition(registry, registry.CurrentID)
		if !ok {
			return fmt.Errorf("当前工作区不存在：%s", registry.CurrentID)
		}
		legacy := s.currentWorkspace()
		var currentArchive *store.Store
		if strings.TrimSpace(definition.ArchiveDB) != "" {
			if legacy.Archive != nil && samePath(legacy.Archive.Path(), definition.ArchiveDB) {
				currentArchive = legacy.Archive
			} else {
				currentArchive, err = store.Open(ctx, definition.ArchiveDB)
				if err != nil {
					return err
				}
			}
		} else if legacy.Archive != nil && samePath(legacy.Definition.Root, definition.Root) {
			currentArchive = legacy.Archive
		}
		s.workspaceMu.Lock()
		s.workspaceStore = workspaceStore
		s.workspaceReg = registry
		s.workspaceCurrent = definition.ID
		s.root = definition.Root
		s.archive = currentArchive
		if currentArchive != nil {
			s.workspaceArchives[definition.ID] = currentArchive
		}
		s.workspaceMu.Unlock()
		return nil
	}
	s.workspaceMu.Lock()
	s.workspaceStore = workspaceStore
	s.workspaceReg = registry
	s.workspaceCurrent = ""
	s.root = ""
	s.archive = nil
	s.workspaceMu.Unlock()
	return nil
}

func (s *Server) currentWorkspace() workspaceSnapshot {
	s.workspaceMu.RLock()
	defer s.workspaceMu.RUnlock()
	definition, _ := workspaceDefinition(s.workspaceReg, s.workspaceCurrent)
	if definition.Root == "" {
		definition.Root = s.root
	}
	return workspaceSnapshot{Definition: definition, Archive: s.archive}
}

func (s *Server) currentRoot() string {
	return s.currentWorkspace().Definition.Root
}

func (s *Server) currentArchive() *store.Store {
	return s.currentWorkspace().Archive
}

func (s *Server) workspaceDefinitions() (workspacepkg.Registry, bool) {
	s.workspaceMu.RLock()
	defer s.workspaceMu.RUnlock()
	registry := s.workspaceReg
	registry.Workspaces = append([]workspacepkg.Definition(nil), s.workspaceReg.Workspaces...)
	return registry, s.workspaceStore != nil
}

func (s *Server) workspaceRegistryPath() string {
	s.workspaceMu.RLock()
	defer s.workspaceMu.RUnlock()
	if s.workspaceStore == nil {
		return ""
	}
	return s.workspaceStore.Path()
}

func (s *Server) selectWorkspace(ctx context.Context, id string) (workspacepkg.Definition, error) {
	s.workspaceMu.Lock()
	defer s.workspaceMu.Unlock()
	if s.workspaceStore == nil {
		return workspacepkg.Definition{}, fmt.Errorf("尚未配置持久化工作区注册表")
	}
	registry := cloneWorkspaceRegistry(s.workspaceReg)
	definition, err := workspacepkg.Select(ctx, &registry, id)
	if err != nil {
		return workspacepkg.Definition{}, err
	}
	archive := s.workspaceArchives[definition.ID]
	openedArchive := false
	if archive == nil && strings.TrimSpace(definition.ArchiveDB) != "" {
		archive, err = store.Open(ctx, definition.ArchiveDB)
		if err != nil {
			return workspacepkg.Definition{}, err
		}
		openedArchive = true
	}
	if err := s.workspaceStore.Save(ctx, registry); err != nil {
		if openedArchive {
			_ = archive.Close()
		}
		return workspacepkg.Definition{}, err
	}
	s.workspaceReg = registry
	if openedArchive {
		s.workspaceArchives[definition.ID] = archive
	}
	s.workspaceCurrent = definition.ID
	s.root = definition.Root
	s.archive = archive
	return definition, nil
}

func (s *Server) addWorkspace(ctx context.Context, definition workspacepkg.Definition) (workspacepkg.Definition, error) {
	s.workspaceMu.Lock()
	defer s.workspaceMu.Unlock()
	if s.workspaceStore == nil {
		return workspacepkg.Definition{}, fmt.Errorf("尚未配置持久化工作区注册表")
	}
	registry := cloneWorkspaceRegistry(s.workspaceReg)
	item, err := workspacepkg.Add(ctx, &registry, definition)
	if err != nil {
		return workspacepkg.Definition{}, err
	}
	if pathWithin(item.Root, s.workspaceStore.Path()) {
		return workspacepkg.Definition{}, fmt.Errorf("工作区源目录不能包含注册表")
	}
	var openedArchive *store.Store
	if strings.TrimSpace(item.ArchiveDB) != "" {
		archive, openErr := store.Open(ctx, item.ArchiveDB)
		if openErr != nil {
			return workspacepkg.Definition{}, openErr
		}
		openedArchive = archive
	}
	if err := s.workspaceStore.Save(ctx, registry); err != nil {
		if openedArchive != nil {
			_ = openedArchive.Close()
		}
		return workspacepkg.Definition{}, err
	}
	s.workspaceReg = registry
	if openedArchive != nil {
		s.workspaceArchives[item.ID] = openedArchive
	}
	if s.workspaceCurrent == "" && registry.CurrentID == item.ID {
		s.workspaceCurrent = item.ID
		s.root = item.Root
		s.archive = openedArchive
	}
	return item, nil
}

func (s *Server) removeWorkspace(ctx context.Context, id string) (workspacepkg.Definition, error) {
	// 与索引任务使用同一锁顺序，避免工作区切换后将仍在构建的数据库关闭。
	s.indexTaskMu.Lock()
	defer s.indexTaskMu.Unlock()
	s.workspaceMu.Lock()
	defer s.workspaceMu.Unlock()
	if s.workspaceStore == nil {
		return workspacepkg.Definition{}, fmt.Errorf("尚未配置持久化工作区注册表")
	}
	if definition, ok := workspaceDefinition(s.workspaceReg, id); ok && s.indexTaskCancel != nil && definition.Root == s.indexTask.Root {
		return workspacepkg.Definition{}, fmt.Errorf("该工作区仍有索引任务，请先停止或等待完成")
	}
	registry := cloneWorkspaceRegistry(s.workspaceReg)
	item, err := workspacepkg.Remove(ctx, &registry, id)
	if err != nil {
		return workspacepkg.Definition{}, err
	}
	if err := s.workspaceStore.Save(ctx, registry); err != nil {
		return workspacepkg.Definition{}, err
	}
	s.workspaceReg = registry
	if archive := s.workspaceArchives[id]; archive != nil {
		_ = archive.Close()
		delete(s.workspaceArchives, id)
	}
	return item, nil
}

func cloneWorkspaceRegistry(registry workspacepkg.Registry) workspacepkg.Registry {
	registry.Workspaces = append([]workspacepkg.Definition(nil), registry.Workspaces...)
	return registry
}

func workspaceDefinition(registry workspacepkg.Registry, id string) (workspacepkg.Definition, bool) {
	for _, definition := range registry.Workspaces {
		if definition.ID == id {
			return definition, true
		}
	}
	return workspacepkg.Definition{}, false
}

func archivePath(archive *store.Store) string {
	if archive == nil {
		return ""
	}
	path := archive.Path()
	if path == ":memory:" {
		return ""
	}
	return path
}

func lifecyclePlanBytes(plan lifecycle.OperationPlan) int64 {
	var total int64
	for _, asset := range plan.Assets {
		if asset.SizeBytes > 0 {
			total += asset.SizeBytes
		}
	}
	return total
}

func samePath(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return strings.TrimSpace(left) == strings.TrimSpace(right)
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

// Handler 返回 HTTP handler；源 Codex 路径固定，写操作必须通过显式预览、确认和独立目标门禁。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.version == "" {
			healthHandler(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
	})
	mux.HandleFunc("/api/v1/capabilities", s.capabilitiesHandler)
	mux.HandleFunc("/api/v1/ui-preferences", s.uiPreferencesHandler)
	mux.HandleFunc("/api/v1/compatibility", s.compatibilityHandler)
	mux.HandleFunc("/api/v1/compatibility/detect", s.compatibilityHandler)
	mux.HandleFunc("/api/v1/resources", s.resourcesHandler)
	mux.HandleFunc("/api/v1/readiness", s.readinessHandler)
	mux.HandleFunc("/api/v1/health/preflight", s.preflightHandler)
	mux.HandleFunc("/api/v1/update/status", s.updateStatusHandler)
	mux.HandleFunc("/api/v1/update/proxy", s.updateProxyHandler)
	mux.HandleFunc("/api/v1/update/automatic", s.updateAutomaticHandler)
	mux.HandleFunc("/api/v1/update/activity", s.updateActivityHandler)
	mux.HandleFunc("/api/v1/update/check", s.updateCheckHandler)
	mux.HandleFunc("/api/v1/update/download", s.updateDownloadHandler)
	mux.HandleFunc("/api/v1/update/prepare-online", s.updatePrepareOnlineHandler)
	mux.HandleFunc("/api/v1/update/prepare", s.updatePrepareHandler)
	mux.HandleFunc("/api/v1/update/activate", s.updateActivateHandler)
	mux.HandleFunc("/api/v1/update/restart", s.updateRestartHandler)
	mux.HandleFunc("/api/v1/update/rollback", s.updateRollbackHandler)
	mux.HandleFunc("/api/v1/auth/status", s.authStatusHandler)
	mux.HandleFunc("/api/v1/auth/bind", s.authBindHandler)
	mux.HandleFunc("/api/v1/auth/login", s.authLoginHandler)
	mux.HandleFunc("/api/v1/auth/logout", s.authLogoutHandler)
	mux.HandleFunc("/api/v1/auth/sessions", s.authSessionsHandler)
	mux.HandleFunc("/api/v1/auth/sessions/revoke", s.authSessionRevokeHandler)
	mux.HandleFunc("/api/v1/auth/audit", s.authAuditHandler)
	mux.HandleFunc("/api/v1/auth/reset-binding", s.authResetBindingHandler)
	mux.HandleFunc("/api/v1/auth/reissue-binding", s.authReissueBindingHandler)
	mux.HandleFunc("/api/v1/workspaces", s.workspacesHandler)
	mux.HandleFunc("/api/v1/workspaces/discovery", s.workspaceDiscoveryHandler)
	mux.HandleFunc("/api/v1/workspaces/", s.workspaceActionHandler)
	mux.HandleFunc("/api/v1/inventory/refresh", s.inventoryRefreshHandler)
	mux.HandleFunc("/api/v1/inventory", s.inventoryHandler)
	mux.HandleFunc("/api/v1/analytics", s.analyticsHandler)
	mux.HandleFunc("/api/v1/maintenance", s.maintenanceHandler)
	mux.HandleFunc("/api/v1/maintenance/preview", s.maintenancePreviewHandler)
	mux.HandleFunc("/api/v1/maintenance/execute", s.maintenanceExecuteHandler)
	mux.HandleFunc("/api/v1/maintenance/recover", s.maintenanceRecoverHandler)
	mux.HandleFunc("/api/v1/cleanup", s.cleanupHandler)
	mux.HandleFunc("/api/v1/cleanup/register", s.cleanupRegisterHandler)
	mux.HandleFunc("/api/v1/cleanup/retention", s.cleanupRetentionHandler)
	mux.HandleFunc("/api/v1/cleanup/settings", s.cleanupSettingsHandler)
	mux.HandleFunc("/api/v1/cleanup/preview", s.cleanupPreviewHandler)
	mux.HandleFunc("/api/v1/cleanup/execute", s.cleanupExecuteHandler)
	mux.HandleFunc("/api/v1/cleanup/recover", s.cleanupRecoverHandler)
	mux.HandleFunc("/api/v1/activity", s.activityHandler)
	mux.HandleFunc("/api/v1/conversations", s.conversationsHandler)
	mux.HandleFunc("/api/v1/conversations/", s.conversationDetailHandler)
	mux.HandleFunc("/api/v1/summaries", s.summariesHandler)
	mux.HandleFunc("/api/v1/summaries/", s.summaryTaskHandler)
	mux.HandleFunc("/api/v1/search", s.searchHandler)
	mux.HandleFunc("/api/v1/operations/pending", s.pendingOperationsHandler)
	mux.HandleFunc("/api/v1/operations/pending/", s.pendingOperationsHandler)
	mux.HandleFunc("/api/v1/operations/confirmations", s.executionConfirmationHandler)
	mux.HandleFunc("/api/v1/operations/executions", s.executionTasksHandler)
	mux.HandleFunc("/api/v1/operations/executions/", s.executionTasksHandler)
	mux.HandleFunc("/api/v1/operations/preview", s.operationPreviewHandler)
	mux.HandleFunc("/api/v1/operations/execute", s.operationExecuteHandler)
	mux.HandleFunc("/api/v1/operations/recovery", s.operationRecoveryListHandler)
	mux.HandleFunc("/api/v1/operations/recover", s.operationRecoveryExecuteHandler)
	mux.HandleFunc("/api/v1/operations/batch/preview", s.batchOperationPreviewHandler)
	mux.HandleFunc("/api/v1/operations/batch/preview/latest", s.latestBatchOperationPreviewHandler)
	mux.HandleFunc("/api/v1/operations/batch/execute", s.batchOperationExecuteHandler)
	mux.HandleFunc("/api/v1/archives/local/restore/", s.localArchiveRestoreHandler)
	mux.HandleFunc("/api/v1/archives/local", s.localArchiveCatalogHandler)
	mux.HandleFunc("/api/v1/backups", s.cloudBackupsHandler)
	mux.HandleFunc("/api/v1/backups/", s.cloudBackupsHandler)
	mux.HandleFunc("/api/v1/archive-center", s.archiveCenterHandler)
	mux.HandleFunc("/api/v1/archive-center/", s.archiveCenterHandler)
	mux.HandleFunc("/api/v1/platform/trash", s.trashDetectionHandler)
	mux.HandleFunc("/api/v1/platform/capabilities", s.platformCapabilitiesHandler)
	mux.HandleFunc("/api/v1/platform/trash/settings", s.trashSettingsHandler)
	mux.HandleFunc("/api/v1/operations/destinations", s.operationDestinationsHandler)
	mux.HandleFunc("/api/v1/platform/trash/probe", s.trashProbeHandler)
	mux.HandleFunc("/api/v1/operations/destinations/confirm", s.confirmOperationDestinationsHandler)
	mux.HandleFunc("/api/v1/github/auth/start", s.githubAuthStartHandler)
	mux.HandleFunc("/api/v1/github/auth/cli", s.githubCLIAuthHandler)
	mux.HandleFunc("/api/v1/github/account", s.githubAccountHandler)
	mux.HandleFunc("/api/v1/github/settings", s.githubSettingsHandler)
	mux.HandleFunc("/api/v1/github/connection-test", s.githubConnectionTestHandler)
	mux.HandleFunc("/api/v1/github/auth/status", s.githubAuthStatusHandler)
	mux.HandleFunc("/api/v1/github/auth/revoke", s.githubAuthRevokeHandler)
	mux.HandleFunc("/api/v1/github/archives", s.githubArchivesHandler)
	mux.HandleFunc("/api/v1/github/archives/", s.githubArchiveDetailHandler)
	// 通用远端归档入口；保留 GitHub 路径以兼容已有客户端。
	mux.HandleFunc("/api/v1/archives", s.remoteArchivesHandler)
	mux.HandleFunc("/api/v1/archives/", s.remoteArchiveDetailHandler)
	mux.HandleFunc("/api/v1/github/repositories", s.githubRepositoriesHandler)
	mux.HandleFunc("/api/v1/github/archive-target", s.githubArchiveTargetHandler)
	mux.HandleFunc("/api/v1/tasks/scan", s.scanTaskHandler)
	mux.HandleFunc("/api/v1/tasks/", s.taskHandler)
	mux.HandleFunc("/api/v1/copies", s.copyHandler)
	mux.HandleFunc("/api/v1/copies/verify", s.copyVerifyHandler)
	mux.HandleFunc("/api/v1/copies/", s.copyTaskHandler)
	mux.HandleFunc("/api/v1/live-copies", s.liveCopyHandler)
	mux.HandleFunc("/api/v1/live-copies/", s.liveCopyTaskHandler)
	mux.HandleFunc("/api/v1/catalog/sync", s.catalogSyncHandler)
	mux.HandleFunc("/api/v1/search-index", s.searchIndexHandler)
	mux.HandleFunc("/api/v1/search-index/", s.searchIndexHandler)
	mux.HandleFunc("/api/v1/source-range", s.sourceRangeHandler)
	if s.authManager != nil {
		return s.drainMiddleware(s.authMiddleware(s.remoteConfigurationGuard(s.sourcePolicyMiddleware(mux))))
	}
	return s.drainMiddleware(s.remoteConfigurationGuard(s.sourcePolicyMiddleware(mux)))
}

func healthHandler(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": "0.0.0-dev",
	})
}

func (s *Server) capabilitiesHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	descriptor := s.lifecycleDescriptor()
	remoteInfo, remoteProviderConfigured := s.remoteArchiveInfo()
	var remoteInfoPayload any
	if remoteProviderConfigured {
		remoteInfoPayload = remoteInfo
	}
	remoteConfigured := remoteProviderConfigured
	if selector := s.githubArchiveSelector(); selector != nil {
		_, remoteConfigured, _ = selector.SelectedArchiveRepository(request.Context())
	} else {
		remoteConfigured = s.remoteArchiveProvider() != nil || s.githubArchiveCatalog() != nil
	}
	var authStatus any
	if s.authManager != nil {
		authStatus = s.authManager.Status()
	}
	realEnabled := s.lifecycle != nil && s.lifecycle.AuthorizedLive != nil && s.executionAllowed(s.currentRoot())
	allConversations := realEnabled && s.lifecycle.AuthorizedLive.AllConversations
	writeJSON(writer, http.StatusOK, map[string]any{
		"api_version":               "v1",
		"source_policy":             s.sourcePolicyFor(s.currentRoot()).Mode,
		"read_only":                 !(s.lifecycleWriteEnabled() || allConversations),
		"write_enabled":             s.lifecycleWriteEnabled() || allConversations,
		"fixture_only":              s.lifecycle != nil && s.lifecycle.FixtureOnly,
		"confirmed_execution":       true,
		"execution_batch_size":      executionBatchSize,
		"restricted_real_execution": map[string]any{"enabled": realEnabled && !allConversations, "allowed_source_ids": authorizedlive.AllowedIDs(), "actions": []string{"trash"}},
		"real_conversation_execution": map[string]any{
			"enabled": allConversations, "scope": "selected_idle_conversations",
			"actions": []string{"trash", "permanent_delete"}, "platform": "macOS",
			"daemon_compatibility": "protocol_checked", "requires_known_schema": true,
			"schema_versions": []string{"0.166", "0.167", "0.171"}, "requires_preview_and_confirmation": true,
		},
		"remote_archive_configured":     remoteConfigured,
		"remote_archive_recovery":       s.githubArchiveRecovery() != nil,
		"remote_archive_delete":         s.githubArchiveDeletion() != nil,
		"remote_archive":                remoteInfoPayload,
		"remote_archive_provider_ready": remoteProviderConfigured,
		"github_repository_configured":  s.githubRepositoryCatalog() != nil,
		"remote_auth_configured":        s.authManager != nil,
		"remote_auth":                   authStatus,
		"summary": map[string]any{
			"available": true,
			"providers": s.summaryRegistry.IDs(),
			"persisted": false,
		},
		"update": map[string]any{
			"available":         s.updater != nil,
			"online_configured": s.updater != nil && s.updater.OnlineConfigured(),
			"source_read_only":  true,
			"restart_required":  true,
		},
		"adapter": descriptor,
	})
}

// resourcesHandler 返回本项目派生数据的预算采样；源 .codex 不计入空间统计。
func (s *Server) resourcesHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	snapshot := s.inspectResources(request.Context())
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only": true,
		"budget":    snapshot,
	})
}

// readinessHandler 返回发布门槛报告；它不会改变任何 adapter 或写入权限。
func (s *Server) readinessHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	writeJSON(writer, http.StatusOK, s.readinessReport(request.Context()))
}

type workspaceRequest struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Root      string `json:"root"`
	ArchiveDB string `json:"archive_db,omitempty"`
}

// workspaceDiscoveryHandler 只读识别服务所在设备的固定 Codex 路径，并关联已有工作区，不自动切换源。
func (s *Server) workspaceDiscoveryHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	candidates, err := workspacepkg.DiscoverCodexHomes(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	registry, persisted := s.workspaceDefinitions()
	type discoveredWorkspace struct {
		workspacepkg.CodexHomeCandidate
		WorkspaceID string `json:"workspace_id,omitempty"`
		Current     bool   `json:"current"`
	}
	items := make([]discoveredWorkspace, 0, len(candidates))
	for _, candidate := range candidates {
		item := discoveredWorkspace{CodexHomeCandidate: candidate}
		for _, definition := range registry.Workspaces {
			if samePath(candidate.Root, definition.Root) {
				item.WorkspaceID = definition.ID
				item.Current = definition.ID == registry.CurrentID
				break
			}
		}
		items = append(items, item)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "scope": "service_device", "persisted": persisted, "candidates": items})
}

// workspacesHandler 管理工作区注册表；切换只改变当前 daemon 的读取上下文，不删除源文件。
func (s *Server) workspacesHandler(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		registry, persisted := s.workspaceDefinitions()
		current := s.currentWorkspace()
		writeJSON(writer, http.StatusOK, map[string]any{
			"read_only":       true,
			"persisted":       persisted,
			"registry_path":   s.workspaceRegistryPath(),
			"current_id":      s.workspaceCurrentID(),
			"current":         current.Definition,
			"workspaces":      registry.Workspaces,
			"workspace_count": len(registry.Workspaces),
		})
	case http.MethodPost:
		request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
		var input workspaceRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			writeError(writer, http.StatusBadRequest, "工作区请求不是有效 JSON")
			return
		}
		item, err := s.addWorkspace(request.Context(), workspacepkg.Definition{ID: input.ID, Name: input.Name, Root: input.Root, ArchiveDB: input.ArchiveDB})
		if err != nil {
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "workspace": item})
	default:
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET 或 POST")
	}
}

// workspaceActionHandler 处理选择和从注册表移除工作区；移除不会删除对应目录或 archive.db。
func (s *Server) workspaceActionHandler(writer http.ResponseWriter, request *http.Request) {
	suffix := strings.TrimPrefix(request.URL.Path, "/api/v1/workspaces/")
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		writeError(writer, http.StatusBadRequest, "工作区操作路径无效")
		return
	}
	id, action := parts[0], parts[1]
	switch action {
	case "select":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		item, err := s.selectWorkspace(request.Context(), id)
		if err != nil {
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "blocked": false, "workspace": item})
	case "remove":
		if request.Method != http.MethodDelete {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 DELETE")
			return
		}
		item, err := s.removeWorkspace(request.Context(), id)
		if err != nil {
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "removed": item})
	default:
		writeError(writer, http.StatusNotFound, "未知工作区操作")
	}
}

func (s *Server) workspaceCurrentID() string {
	s.workspaceMu.RLock()
	defer s.workspaceMu.RUnlock()
	return s.workspaceCurrent
}

func (s *Server) inventoryHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	workspace := s.currentWorkspace()
	root := workspace.Definition.Root
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读扫描根目录")
		return
	}
	refreshRequested, refreshErr := parseOptionalBool(request.URL.Query().Get("refresh"))
	if refreshErr != nil {
		writeError(writer, http.StatusBadRequest, "refresh 参数必须是 true 或 false")
		return
	}
	var snapshot application.InventorySnapshot
	if refreshRequested {
		task := s.tasks.StartInventoryWithOptions(root, application.ScanOptions{})
		current, found, err := s.tasks.Snapshot(request.Context(), root, true)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		if !found {
			writeJSON(writer, http.StatusAccepted, map[string]any{
				"read_only": true, "root": root, "snapshot_status": "missing", "stale": false,
				"refresh_requested": true, "scan_task_id": task.ID, "task": task,
			})
			return
		}
		snapshot = current
		snapshot.Metadata.RefreshRequested = true
		snapshot.Metadata.ScanTaskID = task.ID
	} else {
		result, reportErr := s.readOnlyReportWithOptionsAndFreshness(request, application.ScanOptions{}, true)
		if reportErr.status != 0 {
			writeError(writer, reportErr.status, reportErr.message)
			return
		}
		snapshot = result.snapshot
	}
	report := snapshot.Report
	analytics := application.BuildAnalytics(report)
	adapter := compat.ResolveInventory(report, s.registry)
	maintenanceSource := report
	allFileSizes := fileSizeMap(report.Files)
	report.Files = filterFiles(report.Files, request.URL.Query().Get("q"), request.URL.Query().Get("status"))
	fileTotal := len(report.Files)
	conversationTotal := len(report.Conversations)
	conversationFilter, filterErr := parseConversationFilter(request.URL.Query(), "conversation_")
	if filterErr != nil {
		writeError(writer, http.StatusBadRequest, filterErr.Error())
		return
	}
	filteredConversations := make([]domain.ConversationSummary, 0, conversationTotal)
	for _, conversation := range report.Conversations {
		if conversationMatchesFilter(conversation, conversationFilter, allFileSizes) {
			filteredConversations = append(filteredConversations, conversation)
		}
	}
	filteredConversations = conversationListItems(filteredConversations)
	conversationTotal = len(filteredConversations)
	offset, limit, paged, pageErr := parsePage(request)
	if pageErr != nil {
		writeError(writer, http.StatusBadRequest, pageErr.Error())
		return
	}
	var maintenance maintenancepkg.Report
	if paged {
		maintenance = maintenancepkg.BuildReportPage(maintenanceSource, 0, maintenancePageSize)
		report.Files = pageFiles(report.Files, offset, limit)
		filteredConversations = pageConversations(filteredConversations, offset, limit)
	} else {
		maintenance = maintenancepkg.BuildReport(maintenanceSource)
	}
	response := map[string]any{
		"schema_version":          report.SchemaVersion,
		"mode":                    copypkg.LiveRead,
		"root":                    report.Root,
		"read_only":               report.ReadOnly,
		"generated_at":            report.GeneratedAt,
		"files":                   report.Files,
		"conversations":           filteredConversations,
		"errors":                  report.Errors,
		"file_total":              fileTotal,
		"file_offset":             offset,
		"file_limit":              limit,
		"conversation_total":      conversationTotal,
		"analytics":               analytics,
		"maintenance":             maintenance,
		"adapter":                 adapter,
		"snapshot_status":         snapshot.Metadata.Status,
		"stale":                   snapshot.Metadata.Stale,
		"scan_task_id":            snapshot.Metadata.ScanTaskID,
		"last_successful_scan_at": snapshot.Metadata.LastSuccessfulScanAt,
		"refresh_requested":       snapshot.Metadata.RefreshRequested,
	}
	if snapshot.Metadata.FreshnessCheckError != "" {
		response["freshness_check_error"] = snapshot.Metadata.FreshnessCheckError
	}
	writeJSON(writer, http.StatusOK, response)
	return
}

// inventoryRefreshHandler 启动指定工作区的后台增量扫描；已有扫描任务会被复用。
func (s *Server) inventoryRefreshHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	root := s.currentRoot()
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读扫描根目录")
		return
	}
	task := s.tasks.StartInventoryWithOptions(root, application.ScanOptions{})
	snapshot, found, err := s.tasks.Snapshot(request.Context(), root, false)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	metadata := snapshot.Metadata
	metadata.RefreshRequested = true
	metadata.ScanTaskID = task.ID
	response := map[string]any{"read_only": true, "root": root, "task": task, "snapshot_status": metadata.Status, "stale": metadata.Stale, "refresh_requested": true, "scan_task_id": metadata.ScanTaskID}
	if found {
		response["generated_at"] = snapshot.Report.GeneratedAt
		response["last_successful_scan_at"] = metadata.LastSuccessfulScanAt
	}
	writeJSON(writer, http.StatusAccepted, response)
}

func (s *Server) analyticsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	report, reportErr := s.readOnlyReportWithOptionsAndFreshness(request, application.ScanOptions{}, true)
	if reportErr.status != 0 {
		writeError(writer, reportErr.status, reportErr.message)
		return
	}
	filter, err := parseConversationFilter(request.URL.Query(), "")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	all := application.BuildAnalytics(report.report)
	result := all
	filtered := filter != (conversationFilter{})
	if filtered {
		scoped := report.report
		scoped.Conversations = make([]domain.ConversationSummary, 0)
		scoped.Files = make([]domain.FileInventory, 0)
		paths := make(map[string]struct{})
		fileSizes := fileSizeMap(report.report.Files)
		for _, conversation := range report.report.Conversations {
			if !conversationMatchesFilter(conversation, filter, fileSizes) {
				continue
			}
			scoped.Conversations = append(scoped.Conversations, conversation)
			for _, path := range append(append([]string{}, conversation.RolloutPaths...), conversation.RelatedAssetPaths...) {
				paths[path] = struct{}{}
			}
		}
		for _, file := range report.report.Files {
			if _, exists := paths[file.RelativePath]; exists {
				scoped.Files = append(scoped.Files, file)
			}
		}
		result = application.BuildAnalytics(scoped)
	}
	from, to := "", ""
	if len(all.Daily) > 0 {
		from, to = all.Daily[0].Date, all.Daily[len(all.Daily)-1].Date
	}
	dated := 0
	for _, point := range result.Daily {
		dated += point.Conversations
	}
	writeJSON(writer, http.StatusOK, struct {
		application.AnalyticsReport
		Filtered                 bool   `json:"filtered"`
		Stale                    bool   `json:"stale"`
		SnapshotStatus           string `json:"snapshot_status"`
		SourceFileCount          int    `json:"source_file_count"`
		SourceTotalBytes         int64  `json:"source_total_bytes"`
		AvailableFrom            string `json:"available_from"`
		AvailableTo              string `json:"available_to"`
		UndatedConversationCount int    `json:"undated_conversation_count"`
	}{
		AnalyticsReport: result, Filtered: filtered,
		Stale: report.snapshot.Metadata.Stale, SnapshotStatus: report.snapshot.Metadata.Status,
		SourceFileCount: all.FileCount, SourceTotalBytes: all.TotalBytes,
		AvailableFrom: from, AvailableTo: to, UndatedConversationCount: result.ConversationCount - dated,
	})
}

func (s *Server) maintenanceHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	report, reportErr := s.readOnlyReportWithOptionsAndFreshness(request, application.ScanOptions{}, true)
	if reportErr.status != 0 {
		writeError(writer, reportErr.status, reportErr.message)
		return
	}
	offset, limit, paged, pageErr := parsePage(request)
	if pageErr != nil {
		writeError(writer, http.StatusBadRequest, pageErr.Error())
		return
	}
	query := request.URL.Query()
	category, eligibility, order, scope := query.Get("category"), query.Get("eligibility"), query.Get("sort"), query.Get("scope")
	if !maintenanceReportChoice(category, "temporary", "cache", "log", "sqlite_auxiliary", "repair_jsonl_tail", "orphan") ||
		!maintenanceReportChoice(eligibility, "eligible", "blocked") || !maintenanceReportChoice(order, "size", "path", "newest", "oldest") || !maintenanceReportChoice(scope, "ungrouped") {
		writeError(writer, http.StatusBadRequest, "维护分类、状态或排序不受支持")
		return
	}
	if !paged {
		offset, limit = 0, 0
	}
	directory := query.Get("directory")
	if directory != "" {
		if _, err := maintenancepkg.ResolveSelection(report.report, nil, []string{directory}); err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
	}
	result := maintenancepkg.BuildReportWithOptions(report.report, maintenancepkg.ReportOptions{Offset: offset, Limit: limit, Query: query.Get("q"), Category: category, Eligibility: eligibility, Sort: order, Directory: directory, Scope: scope})
	writeJSON(writer, http.StatusOK, struct {
		maintenancepkg.Report
		WriteEnabled bool `json:"write_enabled"`
		Stale        bool `json:"stale"`
	}{result, s.maintenanceFixtureEnabledForRoot(report.report.Root), report.snapshot.Metadata.Stale})
}

func maintenanceReportChoice(value string, allowed ...string) bool {
	if value == "" {
		return true
	}
	for _, choice := range allowed {
		if value == choice {
			return true
		}
	}
	return false
}

type maintenancePreviewRequest struct {
	Paths       []string `json:"paths"`
	Directories []string `json:"directories"`
}

type maintenanceExecuteRequest struct {
	OperationID string `json:"operation_id"`
}

// maintenancePreviewHandler 生成维护/修复影响清单；它不会写源目录或项目控制面。
func (s *Server) maintenancePreviewHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	workspace := s.currentWorkspace()
	root := workspace.Definition.Root
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读 inventory 扫描根目录")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input maintenancePreviewRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "维护预览请求不是有效 JSON")
		return
	}
	report, err := s.scanner.ScanWithoutGraphs(request.Context(), root)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	selection, selectionErr := maintenancepkg.ResolveSelection(report, input.Paths, input.Directories)
	if selectionErr != nil {
		writeError(writer, http.StatusBadRequest, selectionErr.Error())
		return
	}
	if len(selection.Paths) == 0 {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "所选范围没有可清理文件，受保护和状态异常的文件已保留", "selection": selection})
		return
	}
	activityReport, activityErr := s.probeMaintenanceActivity(request.Context(), report, selection.Paths)
	activityResponse := maintenanceActivityResponse(activityReport, len(selection.Directories) > 0)
	if activityErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": activityErr.Error(), "maintenance": maintenancepkg.BuildReportPage(report, 0, maintenancePageSize), "activity": activityResponse, "selection": selection})
		return
	}
	applyMaintenanceActivity(&report, activityReport)
	fixtureOnly := s.lifecycle != nil && s.lifecycle.Enabled && s.lifecycle.FixtureOnly && !pathHasCodexComponent(root)
	plan, planErr := maintenancepkg.BuildPlan(request.Context(), root, report, maintenancepkg.PlanOptions{SelectedPaths: selection.Paths, FixtureOnly: fixtureOnly})
	if planErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": planErr.Error(), "maintenance": maintenancepkg.BuildReportPage(report, 0, maintenancePageSize), "activity": activityResponse, "plan": plan, "selection": selection})
		return
	}
	if !plan.WriteEnabled {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": plan.BlockReason, "maintenance": maintenancepkg.BuildReportPage(report, 0, maintenancePageSize), "activity": activityResponse, "plan": plan, "selection": selection})
		return
	}
	s.maintenanceMu.Lock()
	s.maintenancePlans[plan.OperationID] = plan
	s.maintenanceMu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "blocked": false, "maintenance": maintenancepkg.BuildReportPage(report, 0, maintenancePageSize), "activity": activityResponse, "plan": plan, "selection": selection})
}

// maintenanceActivityResponse 目录批量预览只返回异常证据摘要；完整文件指纹仍保留在计划中，执行门禁使用未裁剪的探测结果。
func maintenanceActivityResponse(report activity.Report, directorySelection bool) any {
	if !directorySelection {
		return report
	}
	blocked := 0
	results := make([]activity.AssetResult, 0)
	for _, result := range report.Results {
		if result.MutationBlocked || result.Status != domain.StatusStable {
			blocked++
			if len(results) < 20 {
				results = append(results, result)
			}
		}
	}
	return map[string]any{"root": report.Root, "target_count": len(report.Results), "blocked_count": blocked,
		"process_probe_available": report.ProcessProbeAvailable, "lock_probe_available": report.LockProbeAvailable,
		"results": results, "details_truncated": blocked > len(results)}
}

// maintenanceExecuteHandler 只执行已由同一 daemon 预览并缓存的 fixture 计划。
func (s *Server) maintenanceExecuteHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	workspace := s.currentWorkspace()
	root := workspace.Definition.Root
	if !s.maintenanceFixtureEnabledForRoot(root) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "维护写入仅允许显式 fixture 模式；真实 .codex 始终只读"})
		return
	}
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读 inventory 扫描根目录")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input maintenanceExecuteRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || !validMaintenanceOperationID(input.OperationID) {
		writeError(writer, http.StatusBadRequest, "维护执行必须提供有效 operation_id")
		return
	}
	s.maintenanceMu.Lock()
	plan, ok := s.maintenancePlans[input.OperationID]
	s.maintenanceMu.Unlock()
	if !ok {
		writeJSON(writer, http.StatusNotFound, map[string]any{"read_only": true, "blocked": true, "error": "维护计划不存在或已过期，请重新预览"})
		return
	}
	if filepath.Clean(plan.Root) != filepath.Clean(root) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "工作区已切换，维护计划不再适用于当前工作区，请重新预览"})
		return
	}
	s.trashProbeMu.Lock()
	defer s.trashProbeMu.Unlock()
	for _, asset := range plan.Assets {
		if asset.Action == maintenancepkg.ActionIsolate {
			if err := s.requireOperationDestinations(request.Context(), operationPreviewRequest{Trash: true}); err != nil {
				writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
				return
			}
			break
		}
	}
	maintenanceBytes := int64(0)
	for _, asset := range plan.Assets {
		maintenanceBytes += asset.SizeBytes
	}
	if _, budgetErr := s.checkResourcesAdditional(request.Context(), 0, maintenanceBytes); budgetErr != nil {
		writeJSON(writer, http.StatusInsufficientStorage, map[string]any{"read_only": true, "blocked": true, "error": budgetErr.Error(), "plan": plan, "budget": s.inspectResources(request.Context())})
		return
	}
	report, err := s.scanner.ScanWithoutGraphs(request.Context(), root)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	paths := make([]string, 0, len(plan.Assets))
	for _, asset := range plan.Assets {
		paths = append(paths, asset.RelativePath)
	}
	activityReport, activityErr := s.probeMaintenanceActivity(request.Context(), report, paths)
	if activityErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": activityErr.Error(), "activity": activityReport, "plan": plan})
		return
	}
	applyMaintenanceActivity(&report, activityReport)
	for _, asset := range plan.Assets {
		for _, file := range report.Files {
			if file.RelativePath == asset.RelativePath && file.Status != domain.StatusStable && asset.Action == maintenancepkg.ActionIsolate {
				writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": fmt.Sprintf("候选 %s 活动状态为 %s，拒绝执行", asset.RelativePath, file.Status), "activity": activityReport, "plan": plan})
				return
			}
		}
	}
	trash, trashErr := s.maintenanceTrashProvider(request.Context(), root)
	if trashErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": trashErr.Error(), "activity": activityReport, "plan": plan})
		return
	}
	result, executeErr := maintenancepkg.Execute(request.Context(), plan, maintenancepkg.ExecuteOptions{JournalRoot: s.lifecycle.JournalRoot, TrashRoot: s.lifecycle.TrashRoot, Trash: trash, RegisterArtifact: s.cleanupArtifactRegistrar()})
	if result.JournalPath != "" {
		s.tasks.InvalidateInventory(root)
	}
	if executeErr != nil {
		status := http.StatusConflict
		if !result.NeedsReview {
			status = http.StatusInternalServerError
		}
		writeJSON(writer, status, map[string]any{"read_only": false, "blocked": result.NeedsReview, "error": executeErr.Error(), "activity": activityReport, "result": result, "plan": plan})
		return
	}
	s.maintenanceMu.Lock()
	delete(s.maintenancePlans, input.OperationID)
	s.maintenanceMu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "blocked": false, "activity": activityReport, "result": result, "plan": plan})
}

// maintenanceRecoverHandler 根据持久化 journal 恢复未完成的 fixture 维护操作。
func (s *Server) maintenanceRecoverHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if !s.maintenanceFixtureEnabled() || s.lifecycle == nil || strings.TrimSpace(s.lifecycle.JournalRoot) == "" {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "blocked": true, "error": "维护恢复仅允许配置独立 JournalRoot 的 fixture 模式"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input maintenanceExecuteRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || !validMaintenanceOperationID(input.OperationID) {
		writeError(writer, http.StatusBadRequest, "维护恢复必须提供有效 operation_id")
		return
	}
	root := s.currentRoot()
	trash, trashErr := s.maintenanceTrashProvider(request.Context(), root)
	if trashErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": trashErr.Error()})
		return
	}
	journalPath := filepath.Join(s.lifecycle.JournalRoot, input.OperationID+".journal.json")
	result, recoverErr := maintenancepkg.RecoverWithOptions(request.Context(), journalPath, maintenancepkg.RecoverOptions{Trash: trash, RegisterArtifact: s.cleanupArtifactRegistrar()})
	if recoverErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": false, "blocked": result.NeedsReview, "error": recoverErr.Error(), "result": result})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "blocked": false, "result": result})
}

func (s *Server) maintenanceFixtureEnabled() bool {
	return s.maintenanceFixtureEnabledForRoot(s.currentRoot())
}

func (s *Server) maintenanceFixtureEnabledForRoot(root string) bool {
	return s.lifecycle != nil && s.lifecycle.Enabled && s.lifecycle.FixtureOnly && !pathHasCodexComponent(root)
}

func (s *Server) maintenanceTrashProvider(ctx context.Context, sourceRoot string) (platform.TrashProvider, error) {
	if s.lifecycle == nil {
		return nil, fmt.Errorf("未配置维护生命周期")
	}
	if s.lifecycle.TrashProvider != nil {
		return s.lifecycle.TrashProvider, nil
	}
	if s.lifecycle.UseSystemTrash {
		return s.systemTrashProvider(ctx, sourceRoot)
	}
	return nil, nil
}

func (s *Server) probeMaintenanceActivity(ctx context.Context, report domain.InventoryReport, paths []string) (activity.Report, error) {
	allowed := make(map[string]domain.FileInventory, len(report.Files))
	for _, file := range report.Files {
		allowed[file.RelativePath] = file
	}
	targets := make([]activity.Target, 0, len(paths))
	for _, rawPath := range paths {
		path := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(rawPath))))
		file, ok := allowed[path]
		if !ok {
			return activity.Report{}, fmt.Errorf("维护路径必须来自当前 inventory：%s", path)
		}
		targets = append(targets, activity.Target{RelativePath: path, Kind: file.Kind})
	}
	options := activity.DefaultOptions()
	if s.lifecycle != nil && s.lifecycle.FixtureOnly {
		// 合成 fixture 不代表真实 Codex；没有 lsof 的平台仍允许回归文件变化和 lock 证据。
		options.Conservative = false
	}
	if s.lifecycle != nil && s.lifecycle.ActivityOptions != nil {
		options = *s.lifecycle.ActivityOptions
	}
	return activity.Probe(ctx, report.Root, targets, options)
}

func applyMaintenanceActivity(report *domain.InventoryReport, probe activity.Report) {
	statuses := make(map[string]domain.AssetStatus, len(probe.Results))
	for _, result := range probe.Results {
		statuses[result.RelativePath] = result.Status
	}
	for index := range report.Files {
		if status, ok := statuses[report.Files[index].RelativePath]; ok {
			report.Files[index].Status = status
			for _, result := range probe.Results {
				if result.RelativePath == report.Files[index].RelativePath && result.MutationBlocked && status == domain.StatusStable {
					report.Files[index].Status = domain.StatusBlockedForMutation
					break
				}
			}
		}
	}
}

func validMaintenanceOperationID(value string) bool {
	return len(value) == len("maintenance-")+32 && strings.HasPrefix(value, "maintenance-") && !strings.ContainsAny(value, `/\\.`)
}

// activityHandler 对整个扫描根或指定 conversation 的物理资产做一次短窗口只读探测。
// 探测结果只用于诊断和执行前复核，不会创建锁、探针文件或修改源目录。
func (s *Server) activityHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	report, reportErr := s.readOnlyReportWithoutGraphs(request)
	if reportErr.status != 0 {
		writeError(writer, reportErr.status, reportErr.message)
		return
	}
	targetFiles := report.report.Files
	conversationID := strings.TrimSpace(request.URL.Query().Get("conversation_id"))
	if conversationID != "" {
		_, assets, _, found := conversationAssets(report.report, conversationID)
		if !found {
			writeError(writer, http.StatusNotFound, "未找到指定对话")
			return
		}
		targetFiles = assets
	}
	if rawPaths, ok := request.URL.Query()["path"]; ok && len(rawPaths) > 0 {
		allowed := make(map[string]domain.FileInventory, len(report.report.Files))
		for _, file := range report.report.Files {
			allowed[file.RelativePath] = file
		}
		selected := make([]domain.FileInventory, 0, len(rawPaths))
		for _, rawPath := range rawPaths {
			file, exists := allowed[strings.TrimSpace(rawPath)]
			if !exists {
				writeError(writer, http.StatusBadRequest, "path 必须来自当前只读 inventory")
				return
			}
			selected = append(selected, file)
		}
		targetFiles = selected
	}
	interval, parseErr := parseOptionalPositiveInt(request.URL.Query().Get("interval_ms"), 250, 5000)
	if parseErr != nil {
		writeError(writer, http.StatusBadRequest, "interval_ms 参数无效")
		return
	}
	conservative := true
	if raw := strings.TrimSpace(request.URL.Query().Get("conservative")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "conservative 参数必须是 true 或 false")
			return
		}
		conservative = parsed
	}
	options := activity.DefaultOptions()
	options.Interval = time.Duration(interval) * time.Millisecond
	options.Conservative = conservative
	if raw := strings.TrimSpace(request.URL.Query().Get("process")); raw == "false" {
		options.DisableProcessProbe = true
	}
	windowValue := request.URL.Query().Get("windows")
	if strings.TrimSpace(windowValue) == "" {
		windowValue = request.URL.Query().Get("window_count")
	}
	windowCount, parseWindowErr := parseOptionalPositiveInt(windowValue, 1, 16)
	if parseWindowErr != nil {
		writeError(writer, http.StatusBadRequest, "windows 参数无效")
		return
	}
	targets := make([]activity.Target, 0, len(targetFiles))
	for _, file := range targetFiles {
		targets = append(targets, activity.Target{RelativePath: file.RelativePath, Kind: file.Kind})
	}
	probe, err := activity.ProbeWindows(request.Context(), report.report.Root, targets, options, windowCount)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	lifecycleBlocked := false
	readable := true
	for _, result := range probe.Results {
		lifecycleBlocked = lifecycleBlocked || result.MutationBlocked || result.Evidence.LifecycleBlocked
		readable = readable && result.Evidence.Readable
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only":             true,
		"root":                  report.report.Root,
		"conversation_id":       conversationID,
		"snapshot_status":       report.snapshot.Metadata.Status,
		"snapshot_stale":        report.snapshot.Metadata.Stale,
		"snapshot_generated_at": report.report.GeneratedAt,
		"readable":              readable,
		"lifecycle_blocked":     lifecycleBlocked,
		"activity":              probe,
	})
}

func (s *Server) conversationsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	report, reportErr := s.readOnlyReportWithOptionsAndFreshness(request, application.ScanOptions{}, true)
	if reportErr.status != 0 {
		writeError(writer, reportErr.status, reportErr.message)
		return
	}
	conversationFilter, filterErr := parseConversationFilter(request.URL.Query(), "")
	if filterErr != nil {
		writeError(writer, http.StatusBadRequest, filterErr.Error())
		return
	}
	fileSizes := fileSizeMap(report.report.Files)
	items := make([]domain.ConversationSummary, 0, len(report.report.Conversations))
	for _, conversation := range report.report.Conversations {
		if !conversationMatchesFilter(conversation, conversationFilter, fileSizes) {
			continue
		}
		items = append(items, conversation)
	}
	items = conversationListItems(items)
	allCount := len(items)
	offset, limit, _, pageErr := parsePage(request)
	if pageErr != nil {
		writeError(writer, http.StatusBadRequest, pageErr.Error())
		return
	}
	items = pageConversations(items, offset, limit)
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only":       true,
		"mode":            copypkg.LiveRead,
		"root":            report.report.Root,
		"generated_at":    report.report.GeneratedAt,
		"snapshot_status": report.snapshot.Metadata.Status,
		"stale":           report.snapshot.Metadata.Stale,
		"items":           items,
		"total":           allCount,
		"offset":          offset,
		"limit":           limit,
	})
}

func (s *Server) conversationDetailHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	suffix := strings.TrimPrefix(request.URL.Path, "/api/v1/conversations/")
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) == 2 && parts[1] == "asset-graph" {
		s.conversationAssetGraphHandler(writer, request, parts[0])
		return
	}
	if len(parts) >= 2 && parts[1] == "reader" {
		if len(parts) == 2 {
			s.conversationReaderHandler(writer, request, parts[0], false)
			return
		}
		if len(parts) == 3 && parts[2] == "search" {
			s.conversationReaderHandler(writer, request, parts[0], true)
			return
		}
		writeError(writer, http.StatusBadRequest, "阅读路径无效")
		return
	}
	if len(parts) == 2 && parts[1] == "events" {
		s.conversationEventsHandler(writer, request, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "summary" {
		s.conversationSummaryHandler(writer, request, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "project" {
		s.conversationProjectHandler(writer, request, parts[0])
		return
	}
	if len(parts) != 1 {
		writeError(writer, http.StatusBadRequest, "对话路径无效")
		return
	}
	encodedID := parts[0]
	conversationID, err := url.PathUnescape(encodedID)
	if err != nil || strings.TrimSpace(conversationID) == "" {
		writeError(writer, http.StatusBadRequest, "对话 ID 无效")
		return
	}
	lightweight := strings.EqualFold(strings.TrimSpace(request.URL.Query().Get("graph")), "none")
	var report reportResult
	var reportErr reportError
	if lightweight {
		report, reportErr = s.readOnlyReportWithOptionsAndFreshness(request, application.ScanOptions{}, true)
	} else {
		report, reportErr = s.readOnlyReportWithOptionsAndFreshness(request, application.ScanOptions{IncludeAssetGraphs: true}, true)
	}
	if reportErr.status != 0 {
		writeError(writer, reportErr.status, reportErr.message)
		return
	}
	for _, conversation := range report.report.Conversations {
		if conversation.ID != conversationID {
			continue
		}
		if lightweight {
			conversation.AssetGraph = nil
		}
		assetPaths := append([]string{}, conversation.RolloutPaths...)
		assetPaths = append(assetPaths, conversation.RelatedAssetPaths...)
		files := make([]domain.FileInventory, 0, len(assetPaths))
		paths := make(map[string]struct{}, len(assetPaths))
		for _, path := range assetPaths {
			paths[path] = struct{}{}
		}
		for _, file := range report.report.Files {
			if _, ok := paths[file.RelativePath]; ok {
				files = append(files, file)
			}
		}
		if strings.EqualFold(strings.TrimSpace(request.URL.Query().Get("files")), "summary") {
			files = fileListItems(files)
		}
		if lightweight || strings.EqualFold(strings.TrimSpace(request.URL.Query().Get("conversation")), "summary") {
			conversation = conversationListItems([]domain.ConversationSummary{conversation})[0]
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"read_only": true, "mode": copypkg.LiveRead, "snapshot_status": report.snapshot.Metadata.Status, "stale": report.snapshot.Metadata.Stale, "conversation": conversation, "files": files,
		})
		return
	}
	writeError(writer, http.StatusNotFound, "未找到该对话")
}

// conversationAssetGraphHandler 按页返回 canonical 资产图；它只读扫描源目录，不改变图哈希和完整性判断。
func (s *Server) conversationAssetGraphHandler(writer http.ResponseWriter, request *http.Request, encodedID string) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	conversationID, err := url.PathUnescape(encodedID)
	if err != nil || strings.TrimSpace(conversationID) == "" {
		writeError(writer, http.StatusBadRequest, "对话 ID 无效")
		return
	}
	report, reportErr := s.readOnlyReportWithOptionsAndFreshness(request, application.ScanOptions{}, true)
	if reportErr.status != 0 {
		writeError(writer, reportErr.status, reportErr.message)
		return
	}
	var conversation *domain.ConversationSummary
	for index := range report.report.Conversations {
		if report.report.Conversations[index].ID == conversationID {
			conversation = &report.report.Conversations[index]
			break
		}
	}
	if conversation == nil {
		writeError(writer, http.StatusNotFound, "未找到该对话的资产图")
		return
	}
	fullGraph := application.BuildConversationAssetGraph(report.report.Root, report.report.Files, *conversation)
	nodeOffset, edgeOffset, limit, pageErr := parseAssetGraphPage(request)
	if pageErr != nil {
		writeError(writer, http.StatusBadRequest, pageErr.Error())
		return
	}
	graph := pageAssetGraph(fullGraph, nodeOffset, edgeOffset, limit)
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only":       true,
		"conversation_id": conversationID,
		"asset_graph":     graph,
		"node_total":      len(fullGraph.Nodes),
		"node_offset":     nodeOffset,
		"node_limit":      limit,
		"node_has_more":   nodeOffset+len(graph.Nodes) < len(fullGraph.Nodes),
		"edge_total":      len(fullGraph.Edges),
		"edge_offset":     edgeOffset,
		"edge_limit":      limit,
		"edge_has_more":   edgeOffset+len(graph.Edges) < len(fullGraph.Edges),
		"has_more":        nodeOffset+len(graph.Nodes) < len(fullGraph.Nodes) || edgeOffset+len(graph.Edges) < len(fullGraph.Edges),
	})
}

// conversationSummaryHandler 按需生成单个对话摘要；结果不写入源目录或 archive.db。
func (s *Server) conversationSummaryHandler(writer http.ResponseWriter, request *http.Request, encodedID string) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	conversationID, err := url.PathUnescape(encodedID)
	if err != nil || strings.TrimSpace(conversationID) == "" {
		writeError(writer, http.StatusBadRequest, "对话 ID 无效")
		return
	}
	root := s.currentRoot()
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读扫描根目录")
		return
	}
	providerID := strings.TrimSpace(request.URL.Query().Get("provider"))
	maxEvents, parseErr := parseOptionalPositiveInt(request.URL.Query().Get("max_events"), 8, 50)
	if parseErr != nil {
		writeError(writer, http.StatusBadRequest, "max_events 参数无效")
		return
	}
	maxChars, parseErr := parseOptionalPositiveInt(request.URL.Query().Get("max_chars"), 1800, 12000)
	if parseErr != nil {
		writeError(writer, http.StatusBadRequest, "max_chars 参数无效")
		return
	}
	if s.summaryTasks == nil {
		writeError(writer, http.StatusServiceUnavailable, "摘要任务管理器未初始化")
		return
	}
	result, err := s.summaryTasks.GenerateNow(request.Context(), root, conversationID, providerID, maxEvents, maxChars)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "未找到指定对话") {
			status = http.StatusNotFound
		}
		writeError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "persisted": false, "result": result})
}

// conversationProjectHandler 按需读取对话工作目录的项目元数据；不会读取或保存项目正文。
func (s *Server) conversationProjectHandler(writer http.ResponseWriter, request *http.Request, encodedID string) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	conversationID, err := url.PathUnescape(encodedID)
	if err != nil || strings.TrimSpace(conversationID) == "" {
		writeError(writer, http.StatusBadRequest, "对话 ID 无效")
		return
	}
	reportResult, reportErr := s.readOnlyReportWithoutGraphs(request)
	if reportErr.status != 0 {
		writeError(writer, reportErr.status, reportErr.message)
		return
	}
	var conversation *domain.ConversationSummary
	for index := range reportResult.report.Conversations {
		if reportResult.report.Conversations[index].ID == conversationID {
			conversation = &reportResult.report.Conversations[index]
			break
		}
	}
	if conversation == nil {
		writeError(writer, http.StatusNotFound, "未找到该对话")
		return
	}
	maxFiles, parseErr := parseOptionalPositiveInt(request.URL.Query().Get("max_files"), projectpkg.DefaultMaxFiles, 100000)
	if parseErr != nil {
		writeError(writer, http.StatusBadRequest, "max_files 参数无效")
		return
	}
	maxBytes, parseErr := parseOptionalInt64(request.URL.Query().Get("max_bytes"), projectpkg.DefaultMaxBytes)
	if parseErr != nil {
		writeError(writer, http.StatusBadRequest, "max_bytes 参数无效")
		return
	}
	projectReport, scanErr := projectpkg.Scan(request.Context(), conversation.ID, conversation.CWD, projectpkg.Options{MaxFiles: maxFiles, MaxBytes: maxBytes})
	if scanErr != nil {
		if errors.Is(scanErr, context.Canceled) {
			writeError(writer, http.StatusRequestTimeout, "项目扫描已取消")
			return
		}
		writeError(writer, http.StatusInternalServerError, scanErr.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "project": projectReport})
}

type summaryBatchRequest struct {
	ConversationIDs []string `json:"conversation_ids"`
	ProviderID      string   `json:"provider_id,omitempty"`
	MaxEvents       int      `json:"max_events,omitempty"`
	MaxChars        int      `json:"max_chars,omitempty"`
}

// summariesHandler 创建批量摘要任务；任务只保留在当前 daemon 内存中。
func (s *Server) summariesHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if s.summaryTasks == nil {
		writeError(writer, http.StatusServiceUnavailable, "摘要任务管理器未初始化")
		return
	}
	root := s.currentRoot()
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读扫描根目录")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input summaryBatchRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "批量摘要请求不是有效 JSON")
		return
	}
	task, err := s.summaryTasks.Start(root, input.ConversationIDs, input.ProviderID, input.MaxEvents, input.MaxChars)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "persisted": false, "task": task})
}

// summaryTaskHandler 返回批量摘要任务、SSE 进度或取消请求。
func (s *Server) summaryTaskHandler(writer http.ResponseWriter, request *http.Request) {
	if s.summaryTasks == nil {
		writeError(writer, http.StatusServiceUnavailable, "摘要任务管理器未初始化")
		return
	}
	suffix := strings.TrimPrefix(request.URL.Path, "/api/v1/summaries/")
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" || strings.ContainsAny(parts[0], `/\\.`) {
		writeError(writer, http.StatusBadRequest, "摘要任务 ID 无效")
		return
	}
	id := parts[0]
	if len(parts) > 1 && parts[1] == "events" {
		if request.Method != http.MethodGet {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
			return
		}
		s.summaryTaskEventsHandler(writer, request, id)
		return
	}
	if len(parts) > 1 && parts[1] == "cancel" {
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		if !s.summaryTasks.Cancel(id) {
			writeError(writer, http.StatusNotFound, "摘要任务不存在")
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "cancel_requested": true, "task_id": id})
		return
	}
	if len(parts) != 1 || request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	task, ok := s.summaryTasks.Get(id)
	if !ok {
		writeError(writer, http.StatusNotFound, "摘要任务不存在")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "persisted": false, "task": task})
}

func (s *Server) summaryTaskEventsHandler(writer http.ResponseWriter, request *http.Request, id string) {
	if _, ok := s.summaryTasks.Get(id); !ok {
		writeError(writer, http.StatusNotFound, "摘要任务不存在")
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "当前服务器不支持 SSE")
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, exists := s.summaryTasks.Get(id)
		if !exists {
			return
		}
		data, err := json.Marshal(map[string]any{"read_only": true, "persisted": false, "task": task})
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(writer, "event: snapshot\ndata: %s\n\n", data)
		flusher.Flush()
		if task.Status == domain.TaskCompleted || task.Status == domain.TaskFailed || task.Status == domain.TaskCancelled {
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// conversationEventsHandler 返回一个逻辑对话的事件窗口；正文仍可通过原始字节范围按需展开。
func (s *Server) conversationEventsHandler(writer http.ResponseWriter, request *http.Request, encodedID string) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	conversationID, err := url.PathUnescape(encodedID)
	if err != nil || strings.TrimSpace(conversationID) == "" {
		writeError(writer, http.StatusBadRequest, "对话 ID 无效")
		return
	}
	report, reportErr := s.readOnlyReportWithOptionsAndFreshness(request, application.ScanOptions{}, true)
	if reportErr.status != 0 {
		writeError(writer, reportErr.status, reportErr.message)
		return
	}
	var conversation *domain.ConversationSummary
	for index := range report.report.Conversations {
		if report.report.Conversations[index].ID == conversationID {
			conversation = &report.report.Conversations[index]
			break
		}
	}
	if conversation == nil {
		writeError(writer, http.StatusNotFound, "未找到该对话")
		return
	}
	offset, limit, _, pageErr := parsePage(request)
	if pageErr != nil {
		writeError(writer, http.StatusBadRequest, pageErr.Error())
		return
	}
	if limit > 500 {
		limit = 500
	}
	items := make([]domain.ConversationEvent, 0, limit)
	total := 0
	readErrors := make([]string, 0)
	pendingLines := uint64(0)
	invalidJSONLines := uint64(0)
	sourceChanged := false
	for _, relative := range conversation.RolloutPaths {
		path, resolveErr := application.ResolveReadOnlyPath(report.report.Root, relative)
		if resolveErr != nil {
			readErrors = append(readErrors, relative+": "+resolveErr.Error())
			continue
		}
		remainingOffset := offset - total
		if remainingOffset < 0 {
			remainingOffset = 0
		}
		remainingLimit := limit - len(items)
		if remainingLimit <= 0 {
			remainingLimit = 1
		}
		before, beforeErr := os.Lstat(path)
		fileItems, fileTotal, scan, readErr := compat.ReadRolloutEventsPath(request.Context(), path, remainingOffset, remainingLimit)
		if readErr != nil {
			readErrors = append(readErrors, relative+": "+readErr.Error())
			continue
		}
		pendingLines += scan.PendingLines
		invalidJSONLines += scan.InvalidJSONLines
		if after, afterErr := os.Lstat(path); beforeErr != nil || afterErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			sourceChanged = true
		}
		total += fileTotal
		if len(items) < limit {
			items = append(items, fileItems...)
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only": true, "mode": copypkg.LiveRead, "conversation_id": conversationID, "items": items,
		"total": total, "offset": offset, "limit": limit, "errors": readErrors,
		"pending_jsonl_lines": pendingLines, "invalid_json_lines": invalidJSONLines, "source_changed_during_read": sourceChanged,
		"snapshot_status": report.snapshot.Metadata.Status, "stale": report.snapshot.Metadata.Stale,
	})
}

type operationPreviewRequest struct {
	ConversationID   string `json:"conversation_id"`
	Archive          bool   `json:"archive"`
	ArchiveLocal     bool   `json:"archive_local"`
	ArchiveRemote    bool   `json:"archive_remote"`
	Encrypted        bool   `json:"encrypted"`
	ArchivePassword  string `json:"archive_password,omitempty"`
	Trash            bool   `json:"trash"`
	PermanentDelete  bool   `json:"permanent_delete"`
	ConfirmationCode string `json:"confirmation_code"`
}

// operationPreviewHandler 与批量预览共用缓存，执行确认只引用该计划，不重新构造目标。
func (s *Server) operationPreviewHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input operationPreviewRequest
	if json.NewDecoder(request.Body).Decode(&input) != nil {
		writeError(writer, http.StatusBadRequest, "操作预览请求不是有效 JSON")
		return
	}
	response, err := s.buildBatchOperationPlans(request.Context(), batchOperationRequest{
		ConversationIDs: []string{input.ConversationID}, Archive: input.Archive, ArchiveLocal: input.ArchiveLocal, ArchiveRemote: input.ArchiveRemote, Encrypted: input.Encrypted, Trash: input.Trash, PermanentDelete: input.PermanentDelete,
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	item := response.Items[0]
	for _, candidate := range response.Items {
		if candidate.ConversationID == input.ConversationID {
			item = candidate
			break
		}
	}
	status := http.StatusOK
	if response.Blocked {
		status = http.StatusConflict
	}
	writeJSON(writer, status, map[string]any{"read_only": true, "blocked": response.Blocked, "error": item.Error,
		"preview_id": response.PreviewID, "source_root": response.SourceRoot, "plan": item.Plan, "activity": item.Activity,
		"items": response.Items, "summary": response.Summary,
		"asset_graph": item.AssetGraph, "execution_scope": response.ExecutionScope, "restricted_preview": response.RestrictedPreview, "archive_preview": response.ArchivePreview})
}

// operationExecuteHandler 保留 URL 兼容，但必须提供服务端一次性确认凭据。
func (s *Server) operationExecuteHandler(writer http.ResponseWriter, request *http.Request) {
	s.confirmedExecutionHandler(writer, request)
}

func (s *Server) lifecycleDescriptor() compat.Descriptor {
	if s.lifecycle != nil && s.lifecycle.Enabled && s.lifecycle.FixtureOnly {
		if s.lifecycle.Descriptor.ID != "" {
			return s.lifecycle.Descriptor
		}
		return compat.SyntheticFixtureDescriptor()
	}
	return compat.GenericJSONLDescriptor()
}

func (s *Server) lifecycleWriteEnabled() bool {
	if s.lifecycle == nil || !s.lifecycle.Enabled || !s.lifecycle.FixtureOnly {
		return false
	}
	descriptor := s.lifecycleDescriptor()
	for _, capability := range []compat.Capability{
		compat.CapabilityRepair,
		compat.CapabilityArchiveExport,
		compat.CapabilityDelete,
		compat.CapabilityRestore,
		compat.CapabilityRebuildIndex,
	} {
		if descriptor.AllowsWrite(capability) {
			return true
		}
	}
	return false
}

func validateLifecycleDestinations(root string, options LifecycleOptions, input operationPreviewRequest) error {
	if strings.TrimSpace(options.JournalRoot) == "" {
		return fmt.Errorf("执行生命周期操作必须配置独立 JournalRoot")
	}
	destinations := make([]struct {
		label string
		path  string
	}, 0, 3)
	destinations = append(destinations, struct {
		label string
		path  string
	}{label: "journal", path: options.JournalRoot})
	if input.Archive && input.ArchiveLocal {
		if strings.TrimSpace(options.ArchiveRoot) == "" {
			return fmt.Errorf("选择归档时必须配置独立 ArchiveRoot")
		}
		destinations = append(destinations, struct {
			label string
			path  string
		}{label: "archive", path: options.ArchiveRoot})
	}
	if input.Trash {
		if options.TrashProvider == nil && !options.UseSystemTrash && strings.TrimSpace(options.TrashRoot) == "" {
			return fmt.Errorf("选择回收站时必须配置独立 TrashRoot")
		}
		if strings.TrimSpace(options.TrashRoot) != "" {
			destinations = append(destinations, struct {
				label string
				path  string
			}{label: "trash", path: options.TrashRoot})
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("解析源根失败: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	if resolved, resolveErr := filepath.EvalSymlinks(absRoot); resolveErr == nil {
		absRoot = resolved
	}
	abs := make([]struct {
		label string
		path  string
	}, 0, len(destinations))
	for _, destination := range destinations {
		absDestination, absErr := canonicalLifecycleDestination(destination.path)
		if absErr != nil {
			return fmt.Errorf("解析 %s 目录失败: %w", destination.label, absErr)
		}
		absDestination = filepath.Clean(absDestination)
		if pathHasCodexComponent(absDestination) {
			return fmt.Errorf("%s 目录不能位于 .codex", destination.label)
		}
		if pathsOverlap(absRoot, absDestination) {
			return fmt.Errorf("%s 目录不能位于源根内或与源根重叠", destination.label)
		}
		for _, previous := range abs {
			if pathsOverlap(previous.path, absDestination) {
				return fmt.Errorf("%s 目录不能与 %s 目录重叠", destination.label, previous.label)
			}
		}
		abs = append(abs, struct {
			label string
			path  string
		}{label: destination.label, path: absDestination})
	}
	return nil
}

// canonicalLifecycleDestination 解析既有祖先，防止输出路径通过间接链接与源数据重叠。
func canonicalLifecycleDestination(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("不能确定输出目录祖先")
		}
		current = parent
	}
}

func pathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return true
	}
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false
	}
	return true
}

func conversationAssets(report domain.InventoryReport, conversationID string) (domain.ConversationSummary, []domain.FileInventory, map[string]int, bool) {
	var conversation domain.ConversationSummary
	found := false
	for _, candidate := range report.Conversations {
		if candidate.ID == conversationID {
			conversation = candidate
			found = true
			break
		}
	}
	if !found {
		return conversation, nil, nil, false
	}
	paths := make(map[string]struct{})
	for _, path := range append(append([]string{}, conversation.RolloutPaths...), conversation.RelatedAssetPaths...) {
		paths[path] = struct{}{}
	}
	assets := make([]domain.FileInventory, 0, len(paths))
	for _, file := range report.Files {
		if _, ok := paths[file.RelativePath]; ok {
			assets = append(assets, file)
		}
	}
	owners := make(map[string]int)
	for _, candidate := range report.Conversations {
		seen := make(map[string]struct{})
		for _, path := range append(append([]string{}, candidate.RolloutPaths...), candidate.RelatedAssetPaths...) {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			owners[path]++
		}
	}
	return conversation, assets, owners, true
}

// probeConversationActivity 在生成或执行生命周期计划前重新检查目标资产。
// 该探测不改变源目录；结果会绑定到本次响应，供用户审阅和后续门禁使用。
func (s *Server) probeConversationActivity(ctx context.Context, report domain.InventoryReport, conversation domain.ConversationSummary, assets []domain.FileInventory) (domain.ConversationSummary, []domain.FileInventory, activity.Report, error) {
	return s.probeConversationActivityWindows(ctx, report, conversation, assets, 1)
}

// probeConversationActivityWindows 对同一资产执行连续只读窗口探测，供暂存操作建立空闲证据。
func (s *Server) probeConversationActivityWindows(ctx context.Context, report domain.InventoryReport, conversation domain.ConversationSummary, assets []domain.FileInventory, windows int) (domain.ConversationSummary, []domain.FileInventory, activity.Report, error) {
	options := activity.DefaultOptions()
	if s.lifecycle != nil && s.lifecycle.FixtureOnly {
		// 合成 fixture 不代表真实 Codex；没有 lsof 的平台仍允许回归文件变化和 lock 证据。
		options.Conservative = false
	}
	if s.lifecycle != nil && s.lifecycle.ActivityOptions != nil {
		options = *s.lifecycle.ActivityOptions
	}
	targets := make([]activity.Target, 0, len(assets))
	seen := make(map[string]struct{}, len(assets))
	for _, file := range assets {
		if _, exists := seen[file.RelativePath]; exists {
			continue
		}
		seen[file.RelativePath] = struct{}{}
		targets = append(targets, activity.Target{RelativePath: file.RelativePath, Kind: file.Kind})
	}
	probe, err := activity.ProbeWindows(ctx, report.Root, targets, options, windows)
	if err != nil {
		return conversation, assets, probe, err
	}
	byPath := make(map[string]activity.AssetResult, len(probe.Results))
	for _, result := range probe.Results {
		byPath[result.RelativePath] = result
		activityStatus := result.Status
		if result.MutationBlocked && activityStatus == domain.StatusStable {
			activityStatus = domain.StatusBlockedForMutation
		}
		if result.Status != domain.StatusStable || result.MutationBlocked {
			conversation.Status = stricterActivityStatus(conversation.Status, activityStatus)
			conversation.Diagnostics = appendActivityDiagnostic(conversation.Diagnostics, result)
		}
	}
	for index := range assets {
		result, exists := byPath[assets[index].RelativePath]
		if !exists {
			continue
		}
		assets[index].Status = result.Status
		if result.MutationBlocked && assets[index].Status == domain.StatusStable {
			assets[index].Status = domain.StatusBlockedForMutation
		}
		assets[index].Activity = &result.Evidence
		if result.Error != "" {
			assets[index].Error = result.Error
		}
	}
	return conversation, assets, probe, nil
}

func appendActivityDiagnostic(values []domain.Diagnostic, result activity.AssetResult) []domain.Diagnostic {
	if len(values) >= 16 {
		return values
	}
	severity := domain.SeverityBlock
	if result.Status == domain.StatusStaleLock {
		severity = domain.SeverityWarning
	}
	message := "活动探测发现目标资产不适合生命周期写入"
	if result.Evidence.Reason != "" {
		message += "：" + result.Evidence.Reason
	}
	return append(values, domain.Diagnostic{
		Code:     "ACTIVITY_" + strings.ToUpper(string(result.Status)),
		Severity: severity,
		Message:  message,
		Evidence: result.RelativePath,
		ReadOnly: true,
	})
}

func stricterActivityStatus(left, right domain.AssetStatus) domain.AssetStatus {
	rank := map[domain.AssetStatus]int{
		domain.StatusStable: 0, domain.StatusSQLiteWALPresent: 1, domain.StatusWriterLockObserved: 2, domain.StatusStaleLock: 2,
		domain.StatusUnknown: 3, domain.StatusConflict: 4,
		domain.StatusActiveWriting: 5, domain.StatusGrowing: 5, domain.StatusPendingJSONLLine: 5, domain.StatusOpenByProcess: 5,
		domain.StatusSourceChangedDuringProbe: 5, domain.StatusBlockedForMutation: 6, domain.StatusUnsupported: 1,
	}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func pathHasCodexComponent(path string) bool {
	clean := filepath.Clean(path)
	for _, part := range strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return false
}

// platformCapabilitiesHandler 返回当前运行时的平台能力；探测过程不会修改源目录或回收站。
func (s *Server) platformCapabilitiesHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only":        true,
		"source_read_only": true,
		"platform":         platform.DetectCapabilities(),
	})
}

func (s *Server) trashDetectionHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	settings, err := s.loadTrashSettings(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"read_only": true, "error": err.Error()})
		return
	}
	defaultTrash := platform.DetectSystemTrash()
	trash := defaultTrash
	trash.DefaultPath = defaultTrash.CandidatePath
	if settings.CustomPath != "" {
		trash = platform.DetectTrashAtPath(defaultTrash.OS, defaultTrash.Architecture, settings.CustomPath)
		trash.DefaultPath = defaultTrash.CandidatePath
		trash.Configured = true
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only":     true,
		"trash":         trash,
		"default_trash": defaultTrash,
		"settings":      settings,
		"settings_path": s.trashSettings.Path(),
	})
}

type trashSettingsRequest struct {
	CustomPath string `json:"custom_path"`
}

func (s *Server) trashSettingsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	s.trashProbeMu.Lock()
	defer s.trashProbeMu.Unlock()
	if s.trashSettingsErr != nil || s.trashSettings == nil {
		errorMessage := "回收站设置存储不可用"
		if s.trashSettingsErr != nil {
			errorMessage = s.trashSettingsErr.Error()
		}
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"read_only": true, "blocked": true, "error": errorMessage})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input trashSettingsRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "回收站设置不是有效 JSON")
		return
	}
	if len(input.CustomPath) > 32<<10 {
		writeError(writer, http.StatusBadRequest, "回收站路径过长")
		return
	}
	workspace := s.currentWorkspace()
	if strings.TrimSpace(input.CustomPath) != "" {
		if !filepath.IsAbs(strings.TrimSpace(input.CustomPath)) {
			writeError(writer, http.StatusBadRequest, "回收站路径必须是绝对路径")
			return
		}
		customPath, err := filepath.Abs(strings.TrimSpace(input.CustomPath))
		if err != nil {
			writeError(writer, http.StatusBadRequest, "解析回收站路径失败: "+err.Error())
			return
		}
		if strings.TrimSpace(workspace.Definition.Root) != "" && pathsOverlap(workspace.Definition.Root, customPath) {
			writeError(writer, http.StatusBadRequest, "回收站路径不能与当前工作区源目录重叠")
			return
		}
		if err := s.validateTrashPathAgainstLifecycle(customPath, workspace.Definition.Root); err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
	}
	settings := platform.TrashSettings{Version: 1, CustomPath: input.CustomPath}
	if err := s.trashSettings.Save(request.Context(), settings); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	settings, err := s.loadTrashSettings(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"read_only": false, "error": err.Error()})
		return
	}
	defaultTrash := platform.DetectSystemTrash()
	trash := defaultTrash
	trash.DefaultPath = defaultTrash.CandidatePath
	if settings.CustomPath != "" {
		trash = platform.DetectTrashAtPath(defaultTrash.OS, defaultTrash.Architecture, settings.CustomPath)
		trash.DefaultPath = defaultTrash.CandidatePath
		trash.Configured = true
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "trash": trash, "default_trash": defaultTrash, "settings": settings})
}

func (s *Server) loadTrashSettings(ctx context.Context) (platform.TrashSettings, error) {
	if s.trashSettingsErr != nil {
		return platform.TrashSettings{}, s.trashSettingsErr
	}
	if s.trashSettings == nil {
		return platform.TrashSettings{}, fmt.Errorf("回收站设置存储不可用")
	}
	return s.trashSettings.Load(ctx)
}

func (s *Server) systemTrashProvider(ctx context.Context, sourceRoot string) (platform.TrashProvider, error) {
	settings, err := s.loadTrashSettings(ctx)
	if err != nil {
		return nil, err
	}
	defaultTrash := platform.DetectSystemTrash()
	if settings.CustomPath == "" {
		if err := s.validateTrashPathAgainstLifecycle(defaultTrash.CandidatePath, sourceRoot); err != nil {
			return nil, err
		}
		return platform.NewNativeSystemTrash()
	}
	detection := platform.DetectTrashAtPath(defaultTrash.OS, defaultTrash.Architecture, settings.CustomPath)
	if !detection.Supported {
		return nil, fmt.Errorf("配置的回收站路径不可用: %s", detection.Reason)
	}
	if err := s.validateTrashPathAgainstLifecycle(settings.CustomPath, sourceRoot); err != nil {
		return nil, err
	}
	return platform.NewFilesystemTrash(settings.CustomPath, detection.OS)
}

func (s *Server) validateConfiguredSystemTrash(ctx context.Context, sourceRoot string) error {
	settings, err := s.loadTrashSettings(ctx)
	if err != nil {
		return err
	}
	defaultTrash := platform.DetectSystemTrash()
	if settings.CustomPath == "" {
		return s.validateTrashPathAgainstLifecycle(defaultTrash.CandidatePath, sourceRoot)
	}
	if err := s.validateTrashPathAgainstLifecycle(settings.CustomPath, sourceRoot); err != nil {
		return err
	}
	detection := platform.DetectTrashAtPath(defaultTrash.OS, defaultTrash.Architecture, settings.CustomPath)
	if !detection.Supported {
		return fmt.Errorf("配置的回收站路径不可用: %s", detection.Reason)
	}
	return nil
}

func (s *Server) validateTrashPathAgainstLifecycle(path, sourceRoot string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("系统回收站候选路径为空")
	}
	if strings.TrimSpace(sourceRoot) != "" && pathsOverlap(sourceRoot, path) {
		return fmt.Errorf("系统回收站路径与源目录重叠")
	}
	if s == nil || s.lifecycle == nil {
		return nil
	}
	for _, destination := range []struct {
		label string
		path  string
	}{
		{label: "ArchiveRoot", path: s.lifecycle.ArchiveRoot},
		{label: "JournalRoot", path: s.lifecycle.JournalRoot},
		{label: "TrashRoot", path: s.lifecycle.TrashRoot},
		{label: "RemoteRecoveryRoot", path: s.lifecycle.RemoteRecoveryRoot},
	} {
		if strings.TrimSpace(destination.path) != "" && pathsOverlap(destination.path, path) {
			return fmt.Errorf("系统回收站路径不能与 %s 重叠", destination.label)
		}
	}
	return nil
}

// githubAuthStartHandler 开始 GitHub 设备授权；返回的会话不含 device_code 和 access_token。
func (s *Server) githubAuthStartHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	service := s.githubAuthService()
	if !service.DeviceFlowAvailable() {
		writeError(writer, http.StatusServiceUnavailable, "本版本尚未配置项目登录应用；可使用已登录的 GitHub CLI，或在高级设置中配置公开 Client ID")
		return
	}
	session, err := service.Start(request.Context())
	if err != nil {
		writeError(writer, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{
		"read_only": true, "source_read_only": true, "session": session,
	})
}

// githubAuthStatusHandler 返回设备授权状态和凭证是否存在，但不返回凭证内容。
func (s *Server) githubAuthStatusHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	service := s.githubAuthService()
	if service == nil {
		writeJSON(writer, http.StatusOK, map[string]any{
			"read_only": true, "source_read_only": true, "configured": false, "credential_available": false,
		})
		return
	}
	var session *archive.GitHubDeviceAuthSession
	sessionID := strings.TrimSpace(request.URL.Query().Get("session_id"))
	var snapshot archive.GitHubDeviceAuthSession
	var sessionErr error
	if sessionID != "" {
		snapshot, sessionErr = service.Get(sessionID)
	} else {
		snapshot, sessionErr = service.Latest()
	}
	if sessionErr == nil {
		session = &snapshot
	} else if sessionID != "" || !errors.Is(sessionErr, archive.ErrGitHubDeviceSessionNotFound) {
		if errors.Is(sessionErr, archive.ErrGitHubDeviceSessionNotFound) {
			writeError(writer, http.StatusNotFound, sessionErr.Error())
		} else {
			writeError(writer, http.StatusInternalServerError, sessionErr.Error())
		}
		return
	}
	// 先读取会话再检查凭证，避免授权恰好完成时组合出「已授权但无凭证」的瞬时状态。
	available, err := service.CredentialAvailable(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "读取 GitHub 凭证状态失败: "+err.Error())
		return
	}
	response := map[string]any{
		"read_only": true, "source_read_only": true, "configured": true, "credential_available": available,
		"device_flow_available": service.DeviceFlowAvailable(), "cli_available": s.githubSettings.cliAvailable(),
	}
	if session != nil {
		response["session"] = session
	}
	writeJSON(writer, http.StatusOK, response)
}

// githubAuthRevokeHandler 删除已保存的 GitHub 凭证，不触碰源资产和远端归档。
func (s *Server) githubAuthRevokeHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	service := s.githubAuthService()
	if service == nil {
		writeError(writer, http.StatusServiceUnavailable, "GitHub 设备授权未配置")
		return
	}
	if err := service.Revoke(request.Context()); err != nil {
		writeError(writer, http.StatusInternalServerError, "删除 GitHub 凭证失败: "+err.Error())
		return
	}
	s.invalidateRemotePreviews()
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only": true, "source_read_only": true, "credential_revoked": true,
	})
}

func (s *Server) githubAuthService() *archive.GitHubDeviceAuthService {
	if s.githubSettings != nil && s.githubSettings.editable {
		return s.githubSettings.auth
	}
	if s == nil || s.lifecycle == nil {
		return nil
	}
	return s.lifecycle.GitHubAuth
}

// remoteArchivesHandler 是 provider 无关的远端归档列表入口。
// GitHub 旧路径由 githubArchivesHandler 继续提供兼容访问。
func (s *Server) remoteArchivesHandler(writer http.ResponseWriter, request *http.Request) {
	s.githubArchivesHandler(writer, request)
}

// githubArchivesHandler 列出当前远端 provider 的归档清单；不下载正文。
func (s *Server) githubArchivesHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	providerInfo, providerConfigured := s.remoteArchiveInfo()
	var providerPayload any
	if providerConfigured {
		providerPayload = providerInfo
	}
	catalog := s.githubArchiveCatalog()
	if catalog == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "configured": false, "provider_configured": providerConfigured, "provider": providerPayload, "target_required": providerInfo.TargetRequired, "archives": []archive.GitHubArchiveSummary{}})
		return
	}
	if selector := s.githubArchiveSelector(); selector != nil {
		target, configured, targetErr := selector.SelectedArchiveRepository(request.Context())
		if targetErr != nil {
			writeError(writer, http.StatusInternalServerError, "读取远端归档目标失败: "+targetErr.Error())
			return
		}
		if !configured {
			credentialAvailable := false
			if auth := s.githubAuthService(); auth != nil {
				available, credentialErr := auth.CredentialAvailable(request.Context())
				if credentialErr != nil {
					writeError(writer, http.StatusInternalServerError, "读取远端凭证状态失败: "+credentialErr.Error())
					return
				}
				credentialAvailable = available
			}
			writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "configured": false, "provider_configured": providerConfigured, "provider": providerPayload, "credential_available": credentialAvailable, "target_required": providerInfo.TargetRequired, "archives": []archive.GitHubArchiveSummary{}})
			return
		}
		_ = target
	}
	if auth := s.githubAuthService(); auth != nil {
		available, credentialErr := auth.CredentialAvailable(request.Context())
		if credentialErr != nil {
			writeError(writer, http.StatusInternalServerError, "读取远端凭证状态失败: "+credentialErr.Error())
			return
		}
		if !available {
			writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "configured": true, "provider_configured": providerConfigured, "provider": providerPayload, "credential_available": false, "target_required": providerInfo.TargetRequired, "archives": []archive.GitHubArchiveSummary{}})
			return
		}
	}
	items, err := catalog.ListArchives(request.Context())
	if err != nil {
		writeError(writer, http.StatusBadGateway, "读取远端归档列表失败: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "configured": true, "provider_configured": providerConfigured, "provider": providerPayload, "credential_available": true, "target_required": providerInfo.TargetRequired, "archives": items})
}

// remoteArchiveDetailHandler 是 provider 无关的单个归档操作入口。
func (s *Server) remoteArchiveDetailHandler(writer http.ResponseWriter, request *http.Request) {
	s.githubArchiveDetailHandler(writer, request)
}

// githubArchiveDetailHandler 保留历史函数名，实际只提供当前远端 provider 的只读校验、下载和删除流程。
func (s *Server) githubArchiveDetailHandler(writer http.ResponseWriter, request *http.Request) {
	if selector := s.githubArchiveSelector(); selector != nil {
		_, configured, targetErr := selector.SelectedArchiveRepository(request.Context())
		if targetErr != nil {
			writeError(writer, http.StatusInternalServerError, "读取远端归档目标失败: "+targetErr.Error())
			return
		}
		if !configured {
			writeError(writer, http.StatusConflict, "尚未选择远端归档目标")
			return
		}
	}
	suffix := remoteArchivePathSuffix(request.URL.Path)
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) >= 2 && parts[1] == "download" {
		s.githubArchiveDownloadHandler(writer, request, parts[0])
		return
	}
	if len(parts) >= 2 && parts[1] == "restore" {
		s.remoteArchiveRestoreHandler(writer, request, parts[0])
		return
	}
	if len(parts) >= 2 && parts[1] == "delete" {
		s.githubArchiveDeleteHandler(writer, request, parts[0])
		return
	}
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	catalog := s.githubArchiveCatalog()
	if catalog == nil {
		writeError(writer, http.StatusServiceUnavailable, "远端归档 provider 未配置")
		return
	}
	if len(parts) != 2 || parts[1] != "verify" || strings.TrimSpace(parts[0]) == "" {
		writeError(writer, http.StatusBadRequest, "远端归档校验路径无效")
		return
	}
	result, err := catalog.VerifyArchive(request.Context(), parts[0])
	if err != nil {
		writeError(writer, http.StatusBadGateway, "远端归档校验失败: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "verified": true, "archive": result})
}

type remoteDownloadPlan struct {
	OperationID string
	ArchiveID   string
	Destination string
	Preview     archive.RemoteArchiveResult
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Executing   bool
}

type remoteDownloadRequest struct {
	Destination string `json:"destination,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// remoteRestorePlan 保存远端归档下载后的本地恢复预览；下载目录位于独立 ArchiveRoot 下。
type remoteRestorePlan struct {
	OperationID        string
	ArchiveID          string
	ArchivePath        string
	StagingDestination string
	Restore            operation.ArchiveRestorePlan
	Download           archive.RemoteArchiveResult
	CreatedAt          time.Time
	ExpiresAt          time.Time
	Executing          bool
}

type remoteRestoreRequest struct {
	TargetRoot        string `json:"target_root,omitempty"`
	SourceOperationID string `json:"source_operation_id,omitempty"`
	ArchivePassword   string `json:"archive_password,omitempty"`
	OperationID       string `json:"operation_id,omitempty"`
}

type remoteDeletePlan struct {
	OperationID string
	ArchiveID   string
	Preview     archive.RemoteArchiveResult
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Executing   bool
}

type remoteDeleteRequest struct {
	OperationID      string `json:"operation_id,omitempty"`
	ConfirmArchiveID string `json:"confirm_archive_id,omitempty"`
}

// githubArchiveDownloadHandler 提供两阶段远端归档恢复：preview 只校验远端，execute 才写入隔离目录。
func (s *Server) githubArchiveDownloadHandler(writer http.ResponseWriter, request *http.Request, archiveID string) {
	recovery := s.githubArchiveRecovery()
	if recovery == nil {
		writeError(writer, http.StatusServiceUnavailable, "GitHub 远端归档恢复未配置")
		return
	}
	if strings.TrimSpace(archiveID) == "" {
		writeError(writer, http.StatusBadRequest, "远端归档 ID 不能为空")
		return
	}
	suffix := remoteArchivePathSuffix(request.URL.Path)
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) != 3 || parts[1] != "download" {
		writeError(writer, http.StatusBadRequest, "远端归档恢复路径无效")
		return
	}
	switch parts[2] {
	case "preview":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.githubArchiveDownloadPreview(writer, request, recovery, archiveID)
	case "execute":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.githubArchiveDownloadExecute(writer, request, recovery, archiveID)
	default:
		writeError(writer, http.StatusBadRequest, "远端归档恢复动作无效")
	}
}

func (s *Server) githubArchiveDownloadPreview(writer http.ResponseWriter, request *http.Request, recovery archive.RemoteArchiveRecovery, archiveID string) {
	request.Body = http.MaxBytesReader(writer, request.Body, 32<<10)
	var input remoteDownloadRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "远端归档恢复预览请求不是有效 JSON")
		return
	}
	destination, err := s.validateRemoteRecoveryDestination(input.Destination, archiveID)
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	catalog := s.githubArchiveCatalog()
	if catalog == nil {
		writeError(writer, http.StatusServiceUnavailable, "GitHub 远端归档校验未配置")
		return
	}
	preview, err := catalog.VerifyArchive(request.Context(), archiveID)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "远端归档校验失败: "+err.Error())
		return
	}
	operationID, err := archive.NewGitHubArchiveID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "生成恢复操作 ID 失败: "+err.Error())
		return
	}
	now := time.Now().UTC()
	plan := &remoteDownloadPlan{OperationID: operationID, ArchiveID: archiveID, Destination: destination, Preview: preview, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	s.remoteDownloadMu.Lock()
	s.pruneRemoteDownloadPlansLocked(now)
	s.remoteDownloads[operationID] = plan
	s.remoteDownloadMu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only": true, "source_read_only": true, "blocked": false,
		"operation_id": operationID, "archive_id": archiveID, "destination": destination,
		"output_directory": filepath.Join(destination, archiveID), "expires_at": plan.ExpiresAt,
		"archive": preview,
	})
}

func (s *Server) githubArchiveDownloadExecute(writer http.ResponseWriter, request *http.Request, recovery archive.RemoteArchiveRecovery, archiveID string) {
	request.Body = http.MaxBytesReader(writer, request.Body, 32<<10)
	var input remoteDownloadRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "远端归档恢复执行请求不是有效 JSON")
		return
	}
	operationID := strings.TrimSpace(input.OperationID)
	if operationID == "" {
		writeError(writer, http.StatusBadRequest, "恢复操作 ID 不能为空")
		return
	}
	now := time.Now().UTC()
	s.remoteDownloadMu.Lock()
	s.pruneRemoteDownloadPlansLocked(now)
	plan, exists := s.remoteDownloads[operationID]
	if !exists || plan.ArchiveID != archiveID {
		s.remoteDownloadMu.Unlock()
		writeError(writer, http.StatusNotFound, "恢复预览不存在或已过期")
		return
	}
	if plan.Executing {
		s.remoteDownloadMu.Unlock()
		writeError(writer, http.StatusConflict, "恢复操作正在执行")
		return
	}
	plan.Executing = true
	destination := plan.Destination
	s.remoteDownloadMu.Unlock()

	result, err := recovery.DownloadArchive(request.Context(), archiveID, destination)
	s.remoteDownloadMu.Lock()
	delete(s.remoteDownloads, operationID)
	s.remoteDownloadMu.Unlock()
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{"read_only": false, "source_read_only": true, "blocked": true, "operation_id": operationID, "error": "远端归档恢复失败: " + err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "operation_id": operationID, "archive": result, "output_directory": filepath.Join(destination, archiveID)})
}

// remoteArchiveRestoreHandler 将远端归档接入本地恢复核心；preview 会把已校验归档下载到独立 ArchiveRoot staging。
func (s *Server) remoteArchiveRestoreHandler(writer http.ResponseWriter, request *http.Request, archiveID string) {
	recovery := s.githubArchiveRecovery()
	if recovery == nil {
		writeError(writer, http.StatusServiceUnavailable, "远端归档恢复未配置")
		return
	}
	if strings.TrimSpace(archiveID) == "" {
		writeError(writer, http.StatusBadRequest, "远端归档 ID 不能为空")
		return
	}
	suffix := remoteArchivePathSuffix(request.URL.Path)
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) != 3 || parts[1] != "restore" {
		writeError(writer, http.StatusBadRequest, "远端归档本地恢复路径无效")
		return
	}
	switch parts[2] {
	case "preview":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.remoteArchiveRestorePreview(writer, request, recovery, archiveID)
	case "execute":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.remoteArchiveRestoreExecute(writer, request, archiveID)
	default:
		writeError(writer, http.StatusBadRequest, "远端归档本地恢复动作无效")
	}
}

func (s *Server) remoteArchiveRestorePreview(writer http.ResponseWriter, request *http.Request, recovery archive.RemoteArchiveRecovery, archiveID string) {
	if s == nil || s.lifecycle == nil || !s.lifecycleWriteEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "远端归档接入本地恢复需要显式启用生命周期写执行"})
		return
	}
	if strings.TrimSpace(s.lifecycle.ArchiveRoot) == "" || strings.TrimSpace(s.lifecycle.JournalRoot) == "" {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "远端归档本地恢复必须配置独立 ArchiveRoot 和 JournalRoot"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input remoteRestoreRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "远端归档本地恢复预览请求不是有效 JSON")
		return
	}
	target, err := validateLocalRestoreTarget(input.TargetRoot)
	if err != nil || target == "" {
		if err == nil {
			err = fmt.Errorf("远端归档本地恢复必须明确提供目标目录")
		}
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	if root := s.currentRoot(); strings.TrimSpace(root) != "" && pathsOverlap(root, target) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "恢复目标不能与当前扫描根重叠"})
		return
	}
	sourceJournal, err := s.resolveLocalRestoreSourceJournal(input.SourceOperationID)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	operationID, err := lifecycle.NewRestoreOperationID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "生成远端恢复操作 ID 失败: "+err.Error())
		return
	}
	staging, err := s.remoteRestoreStagingRoot(operationID)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": err.Error()})
		return
	}
	if registerArtifact := s.cleanupArtifactRegistrar(); registerArtifact != nil {
		if _, registerErr := registerArtifact(request.Context(), maintenancepkg.ArtifactManifest{
			Kind: maintenancepkg.ArtifactStaging, TargetPath: staging, CreatedAt: time.Now().UTC(), Owner: "remote-restore",
			OperationID: operationID, LeaseUntil: time.Now().UTC().Add(30 * time.Minute), Source: "远端归档恢复 staging",
			Rebuildable: true, Risk: "high", RetentionReason: "远端归档恢复预览完成或失败后再判断", WorkspaceRoot: s.currentRoot(),
		}); registerErr != nil {
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "登记远端恢复 staging 失败: " + registerErr.Error()})
			return
		}
	}
	keepStaging := false
	defer func() {
		if !keepStaging {
			// 预览失败时 staging 没有恢复 journal 绑定，不能把无用派生归档留在本地。
			_ = os.RemoveAll(staging)
		}
	}()
	catalog := s.githubArchiveCatalog()
	if catalog == nil {
		writeError(writer, http.StatusServiceUnavailable, "远端归档校验未配置")
		return
	}
	if _, err := catalog.VerifyArchive(request.Context(), archiveID); err != nil {
		writeError(writer, http.StatusBadGateway, "远端归档校验失败: "+err.Error())
		return
	}
	download, err := recovery.DownloadArchive(request.Context(), archiveID, staging)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "远端归档下载失败: "+err.Error())
		return
	}
	archivePath := filepath.Join(staging, archiveID)
	if _, err := s.validateLocalArchivePath(archivePath); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "下载后的远端归档不在安全 ArchiveRoot 内: " + err.Error()})
		return
	}
	plan, err := operation.PreviewDownloadedArchiveRestore(request.Context(), archivePath, target, sourceJournal, input.ArchivePassword)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "archive_path": archivePath, "download": download, "error": err.Error()})
		return
	}
	if sourceJournal != "" && !samePath(plan.SourcePlan.Root, s.currentRoot()) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "archive_path": archivePath, "download": download, "error": "来源 journal 所属源根与当前工作区不一致"})
		return
	}
	now := time.Now().UTC()
	stored := &remoteRestorePlan{OperationID: plan.RestoreOperationID, ArchiveID: archiveID, ArchivePath: archivePath, StagingDestination: staging, Restore: plan, Download: download, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	s.remoteRestoreMu.Lock()
	s.pruneRemoteRestorePlansLocked(now)
	s.remoteRestores[plan.RestoreOperationID] = stored
	s.remoteRestoreMu.Unlock()
	keepStaging = true
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only": false, "source_read_only": true, "derived_write": true, "blocked": !plan.Preview.Ready,
		"restore_operation_id": plan.RestoreOperationID, "archive_id": archiveID, "archive_path": archivePath,
		"staging_destination": staging, "expires_at": stored.ExpiresAt, "download": download, "plan": plan,
	})
}

func (s *Server) remoteArchiveRestoreExecute(writer http.ResponseWriter, request *http.Request, archiveID string) {
	if s == nil || s.lifecycle == nil || !s.lifecycleWriteEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "当前 daemon 未启用生命周期写执行；默认只读"})
		return
	}
	if pathHasCodexComponent(s.currentRoot()) {
		writeJSON(writer, http.StatusForbidden, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "恢复器拒绝真实 .codex 根目录"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var input remoteRestoreRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "远端归档本地恢复执行请求不是有效 JSON")
		return
	}
	operationID := strings.TrimSpace(input.OperationID)
	if !safeRestoreOperationID(operationID) {
		writeError(writer, http.StatusBadRequest, "恢复操作 ID 无效")
		return
	}
	now := time.Now().UTC()
	s.remoteRestoreMu.Lock()
	s.pruneRemoteRestorePlansLocked(now)
	stored, exists := s.remoteRestores[operationID]
	if !exists || stored.ArchiveID != archiveID {
		s.remoteRestoreMu.Unlock()
		writeError(writer, http.StatusNotFound, "远端恢复预览不存在或已过期")
		return
	}
	if stored.Executing {
		s.remoteRestoreMu.Unlock()
		writeError(writer, http.StatusConflict, "远端恢复操作正在执行")
		return
	}
	stored.Executing = true
	plan := stored.Restore
	s.remoteRestoreMu.Unlock()
	if plan.SourceJournalPath != "" && !samePath(plan.SourcePlan.Root, s.currentRoot()) {
		s.remoteRestoreMu.Lock()
		stored.Executing = false
		s.remoteRestoreMu.Unlock()
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "执行时当前工作区已切换，拒绝恢复到错误控制面"})
		return
	}
	result, restoreErr := operation.ExecuteArchiveRestore(request.Context(), plan, s.lifecycle.JournalRoot, operation.RestoreExecutionOptions{ArchivePassword: input.ArchivePassword, ControlPlaneMutation: s.lifecycle.ControlPlaneMutation})
	s.remoteRestoreMu.Lock()
	delete(s.remoteRestores, operationID)
	s.remoteRestoreMu.Unlock()
	if restoreErr != nil {
		status := http.StatusInternalServerError
		if result.NeedsReview {
			status = http.StatusConflict
		}
		writeJSON(writer, status, map[string]any{"read_only": false, "source_read_only": true, "blocked": result.NeedsReview, "restore_operation_id": operationID, "archive_path": stored.ArchivePath, "error": restoreErr.Error(), "result": result})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "restore_operation_id": operationID, "archive_path": stored.ArchivePath, "result": result})
}

// githubArchiveDeleteHandler 提供远端归档删除的两阶段人工确认流程；不会删除本地归档或源资产。
func (s *Server) githubArchiveDeleteHandler(writer http.ResponseWriter, request *http.Request, archiveID string) {
	deletion := s.githubArchiveDeletion()
	if deletion == nil {
		writeError(writer, http.StatusServiceUnavailable, "GitHub 远端归档删除未配置")
		return
	}
	if strings.TrimSpace(archiveID) == "" {
		writeError(writer, http.StatusBadRequest, "远端归档 ID 不能为空")
		return
	}
	suffix := remoteArchivePathSuffix(request.URL.Path)
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) != 3 || parts[1] != "delete" {
		writeError(writer, http.StatusBadRequest, "远端归档删除路径无效")
		return
	}
	switch parts[2] {
	case "preview":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.githubArchiveDeletePreview(writer, request, archiveID)
	case "execute":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.githubArchiveDeleteExecute(writer, request, deletion, archiveID)
	default:
		writeError(writer, http.StatusBadRequest, "远端归档删除动作无效")
	}
}

func (s *Server) githubArchiveDeletePreview(writer http.ResponseWriter, request *http.Request, archiveID string) {
	catalog := s.githubArchiveCatalog()
	if catalog == nil {
		writeError(writer, http.StatusServiceUnavailable, "GitHub 远端归档校验未配置")
		return
	}
	preview, err := catalog.VerifyArchive(request.Context(), archiveID)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "远端归档校验失败: "+err.Error())
		return
	}
	operationID, err := archive.NewGitHubArchiveID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "生成删除操作 ID 失败: "+err.Error())
		return
	}
	now := time.Now().UTC()
	plan := &remoteDeletePlan{OperationID: operationID, ArchiveID: archiveID, Preview: preview, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	s.remoteDeletesMu.Lock()
	s.pruneRemoteDeletePlansLocked(now)
	s.remoteDeletes[operationID] = plan
	s.remoteDeletesMu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{
		"read_only": true, "source_read_only": true, "blocked": false,
		"operation_id": operationID, "archive_id": archiveID, "expires_at": plan.ExpiresAt,
		"archive": preview, "confirmation_required": archiveID,
	})
}

func (s *Server) githubArchiveDeleteExecute(writer http.ResponseWriter, request *http.Request, deletion archive.RemoteArchiveDeletion, archiveID string) {
	request.Body = http.MaxBytesReader(writer, request.Body, 32<<10)
	var input remoteDeleteRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "远端归档删除请求不是有效 JSON")
		return
	}
	operationID := strings.TrimSpace(input.OperationID)
	if operationID == "" || strings.TrimSpace(input.ConfirmArchiveID) != archiveID {
		writeError(writer, http.StatusConflict, "删除操作 ID 或归档 ID 确认不匹配")
		return
	}
	now := time.Now().UTC()
	s.remoteDeletesMu.Lock()
	s.pruneRemoteDeletePlansLocked(now)
	plan, exists := s.remoteDeletes[operationID]
	if !exists || plan.ArchiveID != archiveID {
		s.remoteDeletesMu.Unlock()
		writeError(writer, http.StatusNotFound, "删除预览不存在或已过期")
		return
	}
	if plan.Executing {
		s.remoteDeletesMu.Unlock()
		writeError(writer, http.StatusConflict, "删除操作正在执行")
		return
	}
	plan.Executing = true
	s.remoteDeletesMu.Unlock()

	if catalog := s.githubArchiveCatalog(); catalog != nil {
		if _, verifyErr := catalog.VerifyArchive(request.Context(), archiveID); verifyErr != nil {
			s.remoteDeletesMu.Lock()
			delete(s.remoteDeletes, operationID)
			s.remoteDeletesMu.Unlock()
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "operation_id": operationID, "error": "删除前远端校验失败，拒绝删除: " + verifyErr.Error()})
			return
		}
	}
	err := deletion.DeleteArchive(request.Context(), archiveID)
	s.remoteDeletesMu.Lock()
	delete(s.remoteDeletes, operationID)
	s.remoteDeletesMu.Unlock()
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{"read_only": false, "source_read_only": true, "blocked": true, "operation_id": operationID, "error": "远端归档删除失败: " + err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "operation_id": operationID, "archive_id": archiveID, "deleted": true})
}

// remoteArchivePathSuffix 同时解析通用归档路径和历史 GitHub 归档路径。
func remoteArchivePathSuffix(pathValue string) string {
	for _, prefix := range []string{"/api/v1/archives/", "/api/v1/github/archives/"} {
		if strings.HasPrefix(pathValue, prefix) {
			return strings.TrimPrefix(pathValue, prefix)
		}
	}
	return strings.TrimPrefix(pathValue, "/")
}

func (s *Server) pruneRemoteDeletePlansLocked(now time.Time) {
	for id, plan := range s.remoteDeletes {
		if plan == nil || now.After(plan.ExpiresAt) {
			delete(s.remoteDeletes, id)
		}
	}
}

func (s *Server) pruneRemoteDownloadPlansLocked(now time.Time) {
	for id, plan := range s.remoteDownloads {
		if plan == nil || now.After(plan.ExpiresAt) {
			delete(s.remoteDownloads, id)
		}
	}
}

func (s *Server) pruneRemoteRestorePlansLocked(now time.Time) {
	for id, plan := range s.remoteRestores {
		if plan == nil {
			delete(s.remoteRestores, id)
			continue
		}
		if !plan.Executing && now.After(plan.ExpiresAt) {
			// 过期计划没有 restore journal 绑定，staging 不再具备续接价值。
			_ = os.RemoveAll(plan.StagingDestination)
			delete(s.remoteRestores, id)
		}
	}
}

func (s *Server) remoteRestoreStagingRoot(operationID string) (string, error) {
	if s == nil || s.lifecycle == nil || strings.TrimSpace(s.lifecycle.ArchiveRoot) == "" {
		return "", fmt.Errorf("未配置独立 ArchiveRoot")
	}
	if !safeRestoreOperationID(operationID) {
		return "", fmt.Errorf("远端恢复操作 ID 无效")
	}
	root, err := filepath.Abs(s.lifecycle.ArchiveRoot)
	if err != nil {
		return "", fmt.Errorf("解析 ArchiveRoot 失败: %w", err)
	}
	root = filepath.Clean(root)
	if pathHasCodexComponent(root) {
		return "", fmt.Errorf("ArchiveRoot 不能位于 .codex 路径")
	}
	if current := s.currentRoot(); strings.TrimSpace(current) != "" {
		current, currentErr := filepath.Abs(current)
		if currentErr == nil && pathsOverlap(root, current) {
			return "", fmt.Errorf("ArchiveRoot 不能与当前扫描根重叠")
		}
	}
	if err := ensureExistingParentsAreSafe(filepath.Dir(root), root); err != nil {
		return "", err
	}
	staging := filepath.Join(root, ".remote-restore-staging", operationID)
	if !pathWithinOrEqual(root, staging) || pathHasCodexComponent(staging) {
		return "", fmt.Errorf("远端恢复 staging 路径不安全")
	}
	if info, statErr := os.Lstat(staging); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("远端恢复 staging 已存在且不是普通目录")
		}
		return "", fmt.Errorf("远端恢复 staging 已存在: %s", staging)
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("检查远端恢复 staging 失败: %w", statErr)
	}
	return staging, nil
}

func (s *Server) validateRemoteRecoveryDestination(value, archiveID string) (string, error) {
	if s == nil || s.lifecycle == nil || strings.TrimSpace(s.lifecycle.RemoteRecoveryRoot) == "" {
		return "", fmt.Errorf("未配置独立的远端归档恢复目录")
	}
	if strings.TrimSpace(value) == "" || strings.TrimSpace(archiveID) == "" {
		return "", fmt.Errorf("恢复目录和归档 ID 不能为空")
	}
	destination, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("解析恢复目录失败: %w", err)
	}
	recoveryRoot, err := filepath.Abs(s.lifecycle.RemoteRecoveryRoot)
	if err != nil {
		return "", fmt.Errorf("解析恢复根目录失败: %w", err)
	}
	if !pathWithinOrEqual(recoveryRoot, destination) {
		return "", fmt.Errorf("恢复目录必须位于配置的独立恢复根目录内")
	}
	root := s.currentRoot()
	if strings.TrimSpace(root) != "" {
		root, rootErr := filepath.Abs(root)
		if rootErr == nil && pathsOverlap(root, destination) {
			return "", fmt.Errorf("恢复目录不能与源扫描根重叠")
		}
	}
	if pathHasCodexComponent(destination) {
		return "", fmt.Errorf("恢复目录不能位于 .codex 路径")
	}
	if err := ensureExistingParentsAreSafe(recoveryRoot, destination); err != nil {
		return "", err
	}
	final := filepath.Join(destination, archiveID)
	if info, statErr := os.Lstat(final); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("恢复目标已存在且不是普通目录")
		}
		return "", fmt.Errorf("恢复目标已存在: %s", final)
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("检查恢复目标失败: %w", statErr)
	}
	return destination, nil
}

func ensureExistingParentsAreSafe(root, destination string) error {
	current := filepath.Clean(root)
	relative, err := filepath.Rel(current, filepath.Clean(destination))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("恢复目录越出恢复根")
	}
	if info, statErr := os.Lstat(current); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return fmt.Errorf("恢复根不是普通目录")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("检查恢复根失败: %w", statErr)
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("检查恢复目录失败: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("恢复目录包含符号链接或非目录")
		}
	}
	return nil
}

func pathWithinOrEqual(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if root == candidate {
		return true
	}
	return pathWithin(root, candidate)
}

func (s *Server) githubArchiveRecovery() archive.RemoteArchiveRecovery {
	if s.githubSettings != nil && s.githubSettings.editable {
		if s.githubSettings.provider == nil {
			return nil
		}
		return s.githubSettings.provider
	}
	if s == nil || s.lifecycle == nil {
		return nil
	}
	if recovery, ok := s.lifecycle.RemoteArchive.(archive.RemoteArchiveRecovery); ok {
		return recovery
	}
	if recovery, ok := s.lifecycle.RemoteArchiveCatalog.(archive.RemoteArchiveRecovery); ok {
		return recovery
	}
	return nil
}

// remoteArchiveInfo 返回当前注入 provider 的公开身份；没有 provider 时返回 false。
func (s *Server) remoteArchiveInfo() (archive.RemoteArchiveProviderInfo, bool) {
	if s == nil {
		return archive.RemoteArchiveProviderInfo{}, false
	}
	var candidate any = s.remoteArchiveProvider()
	if candidate == nil {
		candidate = s.githubArchiveCatalog()
	}
	describer, ok := candidate.(archive.RemoteArchiveDescriber)
	if !ok {
		if candidate == nil {
			return archive.RemoteArchiveProviderInfo{}, false
		}
		return archive.RemoteArchiveProviderInfo{
			ID: "custom", Name: "自定义远端", TargetRequired: false,
			SupportsListing:  s.githubArchiveCatalog() != nil,
			SupportsRecovery: s.githubArchiveRecovery() != nil,
			SupportsDelete:   s.githubArchiveDeletion() != nil,
		}, true
	}
	info := describer.ProviderInfo()
	if strings.TrimSpace(info.ID) == "" {
		info.ID = "custom"
	}
	if strings.TrimSpace(info.Name) == "" {
		info.Name = info.ID
	}
	return info, true
}

func (s *Server) githubArchiveDeletion() archive.RemoteArchiveDeletion {
	if s.githubSettings != nil && s.githubSettings.editable {
		if s.githubSettings.provider == nil {
			return nil
		}
		return s.githubSettings.provider
	}
	if s == nil || s.lifecycle == nil {
		return nil
	}
	if deletion, ok := s.lifecycle.RemoteArchive.(archive.RemoteArchiveDeletion); ok {
		return deletion
	}
	return nil
}

func (s *Server) githubArchiveCatalog() archive.RemoteArchiveCatalog {
	if s.githubSettings != nil && s.githubSettings.editable {
		if s.githubSettings.provider == nil {
			return nil
		}
		return s.githubSettings.provider
	}
	if s == nil || s.lifecycle == nil {
		return nil
	}
	if s.lifecycle.RemoteArchiveCatalog != nil {
		return s.lifecycle.RemoteArchiveCatalog
	}
	if catalog, ok := s.lifecycle.RemoteArchive.(archive.RemoteArchiveCatalog); ok {
		return catalog
	}
	return nil
}

func (s *Server) githubArchiveSelector() archive.GitHubArchiveSelector {
	if s.githubSettings != nil && s.githubSettings.editable {
		if s.githubSettings.provider == nil {
			return nil
		}
		return s.githubSettings.provider
	}
	if s == nil || s.lifecycle == nil {
		return nil
	}
	if selector, ok := s.lifecycle.RemoteArchive.(archive.GitHubArchiveSelector); ok {
		return selector
	}
	if selector, ok := s.lifecycle.RemoteArchiveCatalog.(archive.GitHubArchiveSelector); ok {
		return selector
	}
	return nil
}

type githubArchiveTargetRequest struct {
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
}

// githubArchiveTargetHandler 管理当前 GitHub 归档目标；只改变本地配置，不删除远端内容。
func (s *Server) githubArchiveTargetHandler(writer http.ResponseWriter, request *http.Request) {
	selector := s.githubArchiveSelector()
	if selector == nil {
		writeError(writer, http.StatusServiceUnavailable, "GitHub 归档目标管理未配置")
		return
	}
	switch request.Method {
	case http.MethodGet:
		target, configured, err := selector.SelectedArchiveRepository(request.Context())
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "读取 GitHub 归档目标失败: "+err.Error())
			return
		}
		// 保存的选择只有身份，不等于实时可见性；不把 bool 零值伪报为公开仓库。
		repository := map[string]any{"owner": target.Owner, "name": target.Name, "full_name": target.FullName}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "configured": configured, "repository": repository, "visibility_verified": false})

	case http.MethodPut:
		request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
		var input githubArchiveTargetRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			writeError(writer, http.StatusBadRequest, "GitHub 归档目标请求不是有效 JSON")
			return
		}
		if catalog := s.githubRepositoryCatalog(); catalog != nil {
			repositories, listErr := catalog.ListRepositories(request.Context())
			if listErr != nil {
				writeError(writer, http.StatusBadGateway, "校验 GitHub 归档仓库失败: "+listErr.Error())
				return
			}
			found := false
			private := false
			for _, repository := range repositories {
				if repository.Owner == strings.TrimSpace(input.Owner) && repository.Name == strings.TrimSpace(input.Repository) {
					found = true
					private = repository.Private
					break
				}
			}
			if !found {
				writeError(writer, http.StatusNotFound, "GitHub 账户中不存在指定仓库")
				return
			}
			if !private {
				writeError(writer, http.StatusConflict, "归档目标必须是私有 GitHub 仓库")
				return
			}
		}
		target, err := selector.SelectArchiveRepository(request.Context(), input.Owner, input.Repository)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "选择 GitHub 归档仓库失败: "+err.Error())
			return
		}
		if s.githubRepositoryCatalog() != nil {
			target.Private = true
		}
		s.invalidateRemotePreviews()
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "source_read_only": true, "configured": true, "repository": target})
	case http.MethodDelete:
		if err := selector.ClearArchiveRepository(request.Context()); err != nil {
			writeError(writer, http.StatusInternalServerError, "清除 GitHub 归档目标失败: "+err.Error())
			return
		}
		s.invalidateRemotePreviews()
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": false, "source_read_only": true, "configured": false, "cleared": true})
	default:
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET、PUT 或 DELETE")
	}
}

type githubRepositoryCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// githubRepositoriesHandler 列出或创建用户自己的 GitHub 仓库；源 .codex 和本地归档不受该请求修改。
func (s *Server) githubRepositoriesHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET 或 POST")
		return
	}
	catalog := s.githubRepositoryCatalog()
	if catalog == nil {
		if request.Method == http.MethodPost {
			writeError(writer, http.StatusServiceUnavailable, "GitHub 仓库管理未配置")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "configured": false, "repositories": []archive.GitHubRepositorySummary{}})
		return
	}
	if auth := s.githubAuthService(); auth != nil {
		available, credentialErr := auth.CredentialAvailable(request.Context())
		if credentialErr != nil {
			writeError(writer, http.StatusInternalServerError, "读取 GitHub 凭证状态失败: "+credentialErr.Error())
			return
		}
		if !available {
			if request.Method == http.MethodPost {
				writeError(writer, http.StatusConflict, "请先完成 GitHub 设备绑定")
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "configured": true, "credential_available": false, "repositories": []archive.GitHubRepositorySummary{}})
			return
		}
	}
	switch request.Method {
	case http.MethodGet:
		items, err := catalog.ListRepositories(request.Context())
		if err != nil {
			writeError(writer, http.StatusBadGateway, "读取 GitHub 仓库失败: "+err.Error())
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "configured": true, "repositories": items})
	case http.MethodPost:
		request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
		var input githubRepositoryCreateRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			writeError(writer, http.StatusBadRequest, "仓库创建请求不是有效 JSON")
			return
		}
		item, err := catalog.CreatePrivateRepository(request.Context(), input.Name, input.Description)
		if err != nil {
			writeError(writer, http.StatusBadGateway, "创建 GitHub 私有仓库失败: "+err.Error())
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{"read_only": false, "source_read_only": true, "created": true, "repository": item})
	}
}

func (s *Server) githubRepositoryCatalog() archive.GitHubRepositoryCatalog {
	if s.githubSettings != nil && s.githubSettings.editable {
		return s.githubSettings.repositories
	}
	if s == nil || s.lifecycle == nil {
		return nil
	}
	return s.lifecycle.GitHubRepositories
}

func (s *Server) scanTaskHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	root := s.currentRoot()
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读扫描根目录")
		return
	}
	task := s.tasks.StartInventory(root)
	writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "task": task})
}

type copyPreviewRequest struct {
	SourceRoot                   string             `json:"source_root,omitempty"`
	DestinationRoot              string             `json:"destination_root"`
	ChunkBytes                   int64              `json:"chunk_bytes,omitempty"`
	ExcludePaths                 []string           `json:"exclude_paths,omitempty"`
	AllowUnavailableProcessProbe bool               `json:"allow_unavailable_process_probe,omitempty"`
	Marker                       copypkg.MarkerSpec `json:"marker,omitempty"`
}

type copyVerifyRequest struct {
	Root string `json:"root"`
}

// copyHandler 提供稳定副本的只读计划和异步验证入口；源 .codex 永远不在此接口写入。
func (s *Server) copyHandler(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/api/v1/copies" {
		writeError(writer, http.StatusNotFound, "副本入口路径无效")
		return
	}
	if request.Method == http.MethodGet {
		root := s.currentRoot()
		s.setCopyPlanRoot(root)
		available := strings.EqualFold(filepath.Base(filepath.Clean(root)), ".codex")
		preparedTasks := []copypkg.TaskSnapshot{}
		if s.copyTasks != nil {
			preparedTasks = s.copyTasks.ListPrepared()
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "available": available, "source_root": root, "task_api": "/api/v1/copies/", "prepared_tasks": preparedTasks})
		return
	}
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET 或 POST")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 128<<10)
	var input copyPreviewRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "稳定副本预览请求不是有效 JSON")
		return
	}
	root := s.currentRoot()
	s.setCopyPlanRoot(root)
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置 .codex 扫描根目录")
		return
	}
	if !strings.EqualFold(filepath.Base(filepath.Clean(root)), ".codex") {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "稳定副本制作要求当前工作区根目录名为 .codex"})
		return
	}
	if strings.TrimSpace(input.SourceRoot) != "" && !samePath(root, input.SourceRoot) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "blocked": true, "error": "稳定副本源目录必须是当前工作区 .codex 根目录"})
		return
	}
	if strings.TrimSpace(input.DestinationRoot) == "" {
		writeError(writer, http.StatusBadRequest, "必须提供副本目标目录")
		return
	}
	var activityOptions *activity.Options
	if s.lifecycle != nil && s.lifecycle.ActivityOptions != nil {
		options := *s.lifecycle.ActivityOptions
		activityOptions = &options
	}
	task := s.copyTasks.StartPrepare(copypkg.CopyRequest{SourceRoot: root, DestinationRoot: input.DestinationRoot, ChunkBytes: input.ChunkBytes, Marker: input.Marker, ExcludePaths: input.ExcludePaths, AllowUnavailableProcessProbe: input.AllowUnavailableProcessProbe, ActivityOptions: activityOptions})
	writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "source_read_only": true, "blocked": false, "task": task})
}

type liveCopyRequest struct {
	SourceRoot      string   `json:"source_root,omitempty"`
	DestinationRoot string   `json:"destination_root"`
	ExcludePaths    []string `json:"exclude_paths,omitempty"`
}

// liveCopyHandler 提供 Codex 运行期间的资产级活动副本入口；它不会把活动 writer 当成全局错误。
func (s *Server) liveCopyHandler(writer http.ResponseWriter, request *http.Request) {
	if s.liveCopyTasks == nil {
		writeError(writer, http.StatusServiceUnavailable, "活动副本任务管理器未初始化")
		return
	}
	if request.URL.Path != "/api/v1/live-copies" {
		writeError(writer, http.StatusNotFound, "活动副本入口路径无效")
		return
	}
	if request.Method == http.MethodGet {
		root := s.currentRoot()
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "available": strings.EqualFold(filepath.Base(filepath.Clean(root)), ".codex"), "mode": copypkg.LiveBestEffort, "source_root": root, "task_api": "/api/v1/live-copies/"})
		return
	}
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET 或 POST")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 128<<10)
	var input liveCopyRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "活动副本请求不是有效 JSON")
		return
	}
	root := s.currentRoot()
	if strings.TrimSpace(root) == "" || !strings.EqualFold(filepath.Base(filepath.Clean(root)), ".codex") {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "活动副本要求当前工作区根目录名为 .codex"})
		return
	}
	if strings.TrimSpace(input.SourceRoot) != "" && !samePath(root, input.SourceRoot) {
		writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": "活动副本源目录必须是当前工作区 .codex 根目录"})
		return
	}
	if strings.TrimSpace(input.DestinationRoot) == "" {
		writeError(writer, http.StatusBadRequest, "必须提供活动副本目标目录")
		return
	}
	var activityOptions *activity.Options
	if s.lifecycle != nil && s.lifecycle.ActivityOptions != nil {
		options := *s.lifecycle.ActivityOptions
		activityOptions = &options
	}
	task := s.liveCopyTasks.StartPrepare(copypkg.LiveSnapshotRequest{SourceRoot: root, DestinationRoot: input.DestinationRoot, ExcludePaths: input.ExcludePaths, ActivityOptions: activityOptions})
	writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "source_read_only": true, "blocked": false, "mode": copypkg.LiveBestEffort, "task": task})
}

// liveCopyTaskHandler 读取、取消和确认活动副本任务；活动资产会在结果中保留 queued/partial 证据。
func (s *Server) liveCopyTaskHandler(writer http.ResponseWriter, request *http.Request) {
	if s.liveCopyTasks == nil {
		writeError(writer, http.StatusServiceUnavailable, "活动副本任务管理器未初始化")
		return
	}
	suffix := strings.TrimPrefix(request.URL.Path, "/api/v1/live-copies/")
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeError(writer, http.StatusBadRequest, "活动副本任务 ID 不能为空")
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		if request.Method != http.MethodGet {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
			return
		}
		task, ok := s.liveCopyTasks.Get(id)
		if !ok {
			writeError(writer, http.StatusNotFound, "活动副本任务不存在")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "mode": copypkg.LiveBestEffort, "task": task})
		return
	}
	if len(parts) != 2 || request.Method != http.MethodPost {
		writeError(writer, http.StatusBadRequest, "活动副本任务操作路径无效")
		return
	}
	switch parts[1] {
	case "cancel":
		if !s.liveCopyTasks.Cancel(id) {
			writeError(writer, http.StatusNotFound, "活动副本任务不存在")
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "source_read_only": true, "cancel_requested": true, "task_id": id})
	case "execute":
		task, err := s.liveCopyTasks.StartExecute(id, copypkg.ExecuteOptions{RegisterArtifact: s.cleanupArtifactRegistrar()})
		if err != nil {
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": err.Error()})
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "mode": copypkg.LiveBestEffort, "task": task})
	default:
		writeError(writer, http.StatusNotFound, "未知活动副本任务操作")
	}
}

// copyTaskHandler 读取、取消和确认稳定副本任务；execute 只接受已完成的 prepare 任务。
func (s *Server) copyTaskHandler(writer http.ResponseWriter, request *http.Request) {
	if s.copyTasks == nil {
		writeError(writer, http.StatusServiceUnavailable, "副本任务管理器未初始化")
		return
	}
	s.setCopyPlanRoot(s.currentRoot())
	suffix := strings.TrimPrefix(request.URL.Path, "/api/v1/copies/")
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeError(writer, http.StatusBadRequest, "副本任务 ID 不能为空")
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		if request.Method != http.MethodGet {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
			return
		}
		task, ok := s.copyTasks.Get(id)
		if !ok {
			writeError(writer, http.StatusNotFound, "副本任务不存在")
			return
		}
		if strings.TrimSpace(task.SourceRoot) != "" && strings.TrimSpace(s.currentRoot()) != "" && !samePath(task.SourceRoot, s.currentRoot()) {
			writeError(writer, http.StatusNotFound, "副本任务属于其他工作区")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "source_read_only": true, "task": task})
		return
	}
	if len(parts) != 2 {
		writeError(writer, http.StatusBadRequest, "副本任务操作路径无效")
		return
	}
	switch parts[1] {
	case "cancel":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		if !s.copyTasks.Cancel(id) {
			writeError(writer, http.StatusNotFound, "副本任务不存在")
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "cancel_requested": true, "task_id": id})
	case "execute":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 32<<10)
		var input struct {
			ChunkBytes int64 `json:"chunk_bytes,omitempty"`
		}
		if request.Body != nil && request.ContentLength != 0 {
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				writeError(writer, http.StatusBadRequest, "副本执行请求不是有效 JSON")
				return
			}
		}
		prepared, exists := s.copyTasks.Get(id)
		if !exists {
			writeError(writer, http.StatusNotFound, "副本任务不存在")
			return
		}
		if strings.TrimSpace(prepared.SourceRoot) != "" && strings.TrimSpace(s.currentRoot()) != "" && !samePath(prepared.SourceRoot, s.currentRoot()) {
			writeError(writer, http.StatusNotFound, "副本任务属于其他工作区")
			return
		}
		if prepared.Plan != nil {
			if _, budgetErr := s.checkResourcesAdditional(request.Context(), 0, prepared.Plan.TotalBytes); budgetErr != nil {
				writeJSON(writer, http.StatusInsufficientStorage, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": budgetErr.Error(), "resource_budget": s.inspectResources(request.Context())})
				return
			}
		}
		task, err := s.copyTasks.StartExecute(id, copypkg.ExecuteOptions{ChunkBytes: input.ChunkBytes, RegisterArtifact: s.cleanupArtifactRegistrar()})
		if err != nil {
			writeJSON(writer, http.StatusConflict, map[string]any{"read_only": true, "source_read_only": true, "blocked": true, "error": err.Error()})
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": false, "source_read_only": true, "blocked": false, "task": task})
	default:
		writeError(writer, http.StatusNotFound, "未知副本任务操作")
	}
}

func (s *Server) setCopyPlanRoot(root string) {
	if s == nil || s.copyTasks == nil || strings.TrimSpace(root) == "" {
		return
	}
	s.copyTasks.SetPlanRoot(copyPlanRootForWorkspace(root))
}

func copyPlanRootForWorkspace(root string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(root)), ".codex-chronicle-copy-plans")
}

// copyVerifyHandler 在目标副本上异步执行完整 manifest 和哈希验证。
func (s *Server) copyVerifyHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
	var input copyVerifyRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || strings.TrimSpace(input.Root) == "" {
		writeError(writer, http.StatusBadRequest, "副本验证请求必须包含 root")
		return
	}
	task := s.copyTasks.StartVerify(input.Root)
	writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "source_read_only": true, "task": task})
}

func (s *Server) taskHandler(writer http.ResponseWriter, request *http.Request) {
	if s.tasks == nil {
		writeError(writer, http.StatusServiceUnavailable, "任务管理器未初始化")
		return
	}
	suffix := strings.TrimPrefix(request.URL.Path, "/api/v1/tasks/")
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeError(writer, http.StatusBadRequest, "任务 ID 不能为空")
		return
	}
	id := parts[0]
	if len(parts) > 1 && parts[1] == "events" {
		if request.Method != http.MethodGet {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
			return
		}
		s.taskEventsHandler(writer, request, id)
		return
	}
	if len(parts) > 1 && parts[1] == "cancel" {
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		if !s.tasks.Cancel(id) {
			writeError(writer, http.StatusNotFound, "任务不存在")
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"read_only": true, "cancel_requested": true, "task_id": id})
		return
	}
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	task, ok := s.tasks.Get(id)
	if !ok {
		writeError(writer, http.StatusNotFound, "任务不存在")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "task": task})
}

func (s *Server) taskEventsHandler(writer http.ResponseWriter, request *http.Request, id string) {
	if _, ok := s.tasks.Get(id); !ok {
		writeError(writer, http.StatusNotFound, "任务不存在")
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "当前服务器不支持 SSE")
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, exists := s.tasks.Get(id)
		if !exists {
			return
		}
		data, err := json.Marshal(map[string]any{"read_only": true, "task": task})
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(writer, "event: snapshot\ndata: %s\n\n", data)
		flusher.Flush()
		if task.Status == domain.TaskCompleted || task.Status == domain.TaskFailed || task.Status == domain.TaskCancelled {
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

type reportError struct {
	status  int
	message string
}

type reportResult struct {
	report   domain.InventoryReport
	snapshot application.InventorySnapshot
}

func (s *Server) readOnlyReport(request *http.Request) (reportResult, reportError) {
	return s.readOnlyReportWithOptions(request, application.ScanOptions{IncludeAssetGraphs: true})
}

func (s *Server) readOnlyReportWithoutGraphs(request *http.Request) (reportResult, reportError) {
	return s.readOnlyReportWithOptions(request, application.ScanOptions{})
}

func (s *Server) readOnlyReportWithOptions(request *http.Request, options application.ScanOptions) (reportResult, reportError) {
	return s.readOnlyReportWithOptionsAndFreshness(request, options, false)
}

func (s *Server) readOnlyReportWithOptionsAndFreshness(request *http.Request, options application.ScanOptions, checkFreshness bool) (reportResult, reportError) {
	root := s.currentRoot()
	if strings.TrimSpace(root) == "" {
		return reportResult{}, reportError{http.StatusServiceUnavailable, "尚未配置只读扫描根目录"}
	}
	snapshot, err := s.tasks.ReadReport(request.Context(), root, options, checkFreshness)
	if err != nil {
		return reportResult{}, reportError{http.StatusBadRequest, err.Error()}
	}
	return reportResult{report: snapshot.Report, snapshot: snapshot}, reportError{}
}

type conversationFilter struct {
	Query          string
	Status         string
	CWD            string
	Model          string
	ModelExact     bool
	CWDExact       bool
	From           time.Time
	To             time.Time
	MinTokens      int64
	MaxTokens      int64
	MinBytes       int64
	MaxBytes       int64
	HasDiagnostics *bool
	NeedsAttention bool
}

func parseConversationFilter(values url.Values, prefix string) (conversationFilter, error) {
	filter := conversationFilter{
		Query: strings.TrimSpace(values.Get(prefix + "q")), Status: strings.TrimSpace(values.Get(prefix + "status")),
		CWD: strings.TrimSpace(values.Get(prefix + "cwd")), Model: strings.TrimSpace(values.Get(prefix + "model")),
	}
	var err error
	for name, target := range map[string]*bool{"model_exact": &filter.ModelExact, "cwd_exact": &filter.CWDExact} {
		if raw := strings.TrimSpace(values.Get(prefix + name)); raw != "" {
			*target, err = strconv.ParseBool(raw)
			if err != nil {
				return filter, fmt.Errorf("%s 参数必须是 true 或 false", name)
			}
		}
	}
	if raw := strings.TrimSpace(values.Get(prefix + "from")); raw != "" {
		filter.From, err = parseFilterTime(raw, false)
		if err != nil {
			return filter, fmt.Errorf("from 参数无效: %w", err)
		}
	}
	if raw := strings.TrimSpace(values.Get(prefix + "to")); raw != "" {
		filter.To, err = parseFilterTime(raw, true)
		if err != nil {
			return filter, fmt.Errorf("to 参数无效: %w", err)
		}
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && filter.From.After(filter.To) {
		return filter, fmt.Errorf("开始日期不能晚于结束日期")
	}
	filter.MinTokens, err = parseOptionalInt64(values.Get(prefix+"min_tokens"), 0)
	if err != nil {
		return filter, fmt.Errorf("min_tokens 参数无效")
	}
	filter.MaxTokens, err = parseOptionalInt64(values.Get(prefix+"max_tokens"), 0)
	if err != nil {
		return filter, fmt.Errorf("max_tokens 参数无效")
	}
	filter.MinBytes, err = parseOptionalInt64(values.Get(prefix+"min_bytes"), 0)
	if err != nil {
		return filter, fmt.Errorf("min_bytes 参数无效")
	}
	filter.MaxBytes, err = parseOptionalInt64(values.Get(prefix+"max_bytes"), 0)
	if err != nil {
		return filter, fmt.Errorf("max_bytes 参数无效")
	}
	if filter.MaxTokens > 0 && filter.MinTokens > filter.MaxTokens || filter.MaxBytes > 0 && filter.MinBytes > filter.MaxBytes {
		return filter, fmt.Errorf("筛选范围下限不能大于上限")
	}
	if raw := strings.TrimSpace(values.Get(prefix + "has_diagnostics")); raw != "" {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return filter, fmt.Errorf("has_diagnostics 参数必须是 true 或 false")
		}
		filter.HasDiagnostics = &value
	}
	if raw := strings.TrimSpace(values.Get(prefix + "attention")); raw != "" {
		filter.NeedsAttention, err = strconv.ParseBool(raw)
		if err != nil {
			return filter, fmt.Errorf("attention 参数必须是 true 或 false")
		}
	}
	return filter, nil
}

func parseFilterTime(value string, endOfDay bool) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		return parsed.Add(24*time.Hour - time.Nanosecond).UTC(), nil
	}
	return parsed.UTC(), nil
}

func parseOptionalInt64(value string, fallback int64) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("必须是非负整数")
	}
	return parsed, nil
}

func fileSizeMap(files []domain.FileInventory) map[string]int64 {
	result := make(map[string]int64, len(files))
	for _, file := range files {
		result[file.RelativePath] = file.SizeBytes
	}
	return result
}

func conversationMatches(conversation domain.ConversationSummary, query, status, cwd string) bool {
	return conversationMatchesFilter(conversation, conversationFilter{Query: query, Status: status, CWD: cwd}, nil)
}

func conversationMatchesFilter(conversation domain.ConversationSummary, filter conversationFilter, fileSizes map[string]int64) bool {
	// 待处理队列在分页前筛选，避免遗漏尚未载入会话库的非稳定或带诊断会话。
	if filter.NeedsAttention && conversation.Status == domain.StatusStable && len(conversation.Diagnostics) == 0 {
		return false
	}
	if filter.Status != "" && string(conversation.Status) != filter.Status {
		return false
	}
	if filter.CWD != "" {
		if filter.CWDExact {
			if application.AnalyticsProjectName(conversation.CWD) != filter.CWD {
				return false
			}
		} else if !strings.Contains(strings.ToLower(conversation.CWD), strings.ToLower(filter.CWD)) {
			return false
		}
	}
	if filter.Model != "" {
		if filter.ModelExact {
			if application.AnalyticsModelName(conversation.Model) != filter.Model {
				return false
			}
		} else if !strings.Contains(strings.ToLower(strings.Join([]string{conversation.Model, conversation.ModelProvider}, " ")), strings.ToLower(filter.Model)) {
			return false
		}
	}
	stamp := conversation.UpdatedAt
	if stamp.IsZero() {
		stamp = conversation.StartedAt
	}
	if !filter.From.IsZero() && (stamp.IsZero() || stamp.Before(filter.From)) {
		return false
	}
	if !filter.To.IsZero() && (stamp.IsZero() || stamp.After(filter.To)) {
		return false
	}
	tokens := conversation.TokenUsage.TotalTokens
	if filter.MinTokens > 0 && tokens < filter.MinTokens || filter.MaxTokens > 0 && tokens > filter.MaxTokens {
		return false
	}
	bytes := int64(0)
	seen := make(map[string]struct{})
	for _, path := range append(append([]string{}, conversation.RolloutPaths...), conversation.RelatedAssetPaths...) {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		bytes += fileSizes[path]
	}
	if filter.MinBytes > 0 && bytes < filter.MinBytes || filter.MaxBytes > 0 && bytes > filter.MaxBytes {
		return false
	}
	if filter.HasDiagnostics != nil && (len(conversation.Diagnostics) > 0) != *filter.HasDiagnostics {
		return false
	}
	if filter.Query == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{
		conversation.ID, conversation.SourceID, conversation.Title,
		conversation.FirstUserText, conversation.CWD, conversation.Model,
		conversation.ModelProvider, conversation.CLIVersion,
		strings.Join(append(append([]string{}, conversation.RolloutPaths...), conversation.RelatedAssetPaths...), " "),
	}, "\n"))
	return strings.Contains(haystack, strings.ToLower(filter.Query))
}

func filterFiles(files []domain.FileInventory, query, status string) []domain.FileInventory {
	query = strings.ToLower(strings.TrimSpace(query))
	status = strings.TrimSpace(status)
	filtered := make([]domain.FileInventory, 0, len(files))
	for _, file := range files {
		if status != "" && string(file.Status) != status {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(file.RelativePath), query) {
			continue
		}
		filtered = append(filtered, file)
	}
	return filtered
}

// conversationListItems 生成轻量列表投影；完整资产图和 SQLite 关系只在详情或影响预览中返回。
func conversationListItems(items []domain.ConversationSummary) []domain.ConversationSummary {
	result := make([]domain.ConversationSummary, len(items))
	for index, item := range items {
		item.Title = truncateListText(item.Title, 240)
		item.FirstUserText = truncateListText(item.FirstUserText, 2000)
		item.Relationships = nil
		item.AssetGraph = nil
		result[index] = item
	}
	return result
}

// fileListItems 生成关联文件的轻量投影；正文解析和 SQLite 关系只在按需读取时返回。
func fileListItems(items []domain.FileInventory) []domain.FileInventory {
	result := make([]domain.FileInventory, len(items))
	for index, item := range items {
		item.Activity = nil
		item.JSONL = nil
		item.Rollout = nil
		item.SessionIndex = nil
		item.SQLite = nil
		result[index] = item
	}
	return result
}

func truncateListText(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}

const maintenancePageSize = 30

func parseAssetGraphPage(request *http.Request) (nodeOffset, edgeOffset, limit int, err error) {
	values := request.URL.Query()
	legacyOffset := values.Get("offset")
	nodeOffset, err = parseOptionalNonNegativeInt(firstQueryValue(values.Get("node_offset"), legacyOffset), 0, 100000000)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("node_offset 参数无效")
	}
	edgeOffset, err = parseOptionalNonNegativeInt(firstQueryValue(values.Get("edge_offset"), legacyOffset), 0, 100000000)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("edge_offset 参数无效")
	}
	limit, err = parseOptionalPositiveInt(values.Get("limit"), 200, 500)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("limit 参数无效")
	}
	return nodeOffset, edgeOffset, limit, nil
}

func firstQueryValue(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func pageAssetGraph(graph domain.ConversationAssetGraph, nodeOffset, edgeOffset, limit int) domain.ConversationAssetGraph {
	graph.Nodes = pageGraphItems(graph.Nodes, nodeOffset, limit)
	graph.Edges = pageGraphItems(graph.Edges, edgeOffset, limit)
	return graph
}

func pageGraphItems[T any](items []T, offset, limit int) []T {
	if offset >= len(items) {
		return []T{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}

func parsePage(request *http.Request) (offset, limit int, paged bool, err error) {
	values := request.URL.Query()
	paged = values.Has("offset") || values.Has("limit")
	offset = 0
	limit = 200
	if values.Has("offset") {
		offset, err = parseOptionalNonNegativeInt(values.Get("offset"), 0, 100000000)
		if err != nil {
			return 0, 0, paged, err
		}
	}
	if values.Has("limit") {
		limit, err = parseOptionalPositiveInt(values.Get("limit"), 200, 1000)
		if err != nil {
			return 0, 0, paged, err
		}
	}
	return offset, limit, paged, nil
}

func pageFiles(files []domain.FileInventory, offset, limit int) []domain.FileInventory {
	if offset >= len(files) {
		return []domain.FileInventory{}
	}
	end := offset + limit
	if end > len(files) {
		end = len(files)
	}
	return files[offset:end]
}

func pageConversations(items []domain.ConversationSummary, offset, limit int) []domain.ConversationSummary {
	if offset >= len(items) {
		return []domain.ConversationSummary{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}

func parseOptionalNonNegativeInt(value string, fallback, max int) (int, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 || parsed > max {
		return 0, fmt.Errorf("参数必须是 0-%d 的整数", max)
	}
	return parsed, nil
}

func parseOptionalPositiveInt(value string, fallback, max int) (int, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 || parsed > max {
		return 0, fmt.Errorf("参数必须是 1-%d 的整数", max)
	}
	return parsed, nil
}

func parseOptionalBool(value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, err
	}
	return parsed, nil
}

func (s *Server) sourceRangeHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	root := s.currentRoot()
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读扫描根目录")
		return
	}
	start, err := parseNonNegativeInt(request.URL.Query().Get("start"), "start")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	length, err := parseNonNegativeInt(request.URL.Query().Get("length"), "length")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	path, err := application.ResolveReadOnlyPath(root, request.URL.Query().Get("path"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	before, beforeErr := os.Lstat(path)
	if beforeErr != nil {
		writeError(writer, http.StatusBadRequest, beforeErr.Error())
		return
	}
	chunk, err := catalog.ReadRange(request.Context(), path, start, length, catalog.DefaultMaxReadBytes)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	after, afterErr := os.Lstat(path)
	changed := afterErr != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, after)
	writer.Header().Set("X-Codex-Chronicle-Mode", copypkg.LiveRead)
	writer.Header().Set("X-Codex-Chronicle-Read-Only", "true")
	writer.Header().Set("X-Codex-Chronicle-Source-Changed", strconv.FormatBool(changed))
	if afterErr == nil {
		writer.Header().Set("X-Codex-Chronicle-Source-Size", strconv.FormatInt(after.Size(), 10))
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(chunk)
}

func (s *Server) catalogSyncHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if _, budgetErr := s.checkResources(request.Context()); budgetErr != nil {
		writeJSON(writer, http.StatusInsufficientStorage, map[string]any{"read_only": true, "blocked": true, "error": budgetErr.Error(), "budget": s.inspectResources(request.Context())})
		return
	}
	workspace := s.currentWorkspace()
	root := workspace.Definition.Root
	archive := workspace.Archive
	if archive == nil {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置独立 archive.db")
		return
	}
	if strings.TrimSpace(root) == "" {
		writeError(writer, http.StatusServiceUnavailable, "尚未配置只读扫描根目录")
		return
	}
	report, err := s.scanner.Scan(request.Context(), root)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	result, err := archive.SyncInventory(request.Context(), report)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"inventory": report,
		"sync":      result,
	})
}

func parseNonNegativeInt(value, name string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, fmt.Errorf("缺少 %s", name)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s 必须是非负整数", name)
	}
	return parsed, nil
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{
		"error":     message,
		"read_only": true,
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
