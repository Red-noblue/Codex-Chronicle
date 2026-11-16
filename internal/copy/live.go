// 中文说明：支持 Codex 运行期间的资产级活动副本；稳定资产继续发布，活动资产保留前缀或排队证据。
package copy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/maintenance"
	"codex-chronicle.local/internal/sqliteguard"
	"codex-chronicle.local/internal/store"
	_ "modernc.org/sqlite"
)

const (
	// LiveManifestName 标记一个允许包含活动资产前缀的副本。
	LiveManifestName = "codex-live-snapshot.json"
	// LiveBestEffort 是活动源读取模式的稳定外部名称。
	LiveBestEffort = "live_best_effort"
	// LiveRead 是面向读取 API 的同义名称，避免客户端绑定内部实现命名。
	LiveRead = LiveBestEffort
)

// LiveAsset 是一个资产在活动读取和发布阶段的完整性证据。
type LiveAsset struct {
	RelativePath     string                  `json:"relative_path"`
	Kind             domain.AssetKind        `json:"kind"`
	Status           domain.AssetStatus      `json:"status"`
	Consistency      string                  `json:"consistency"`
	CapturedSize     int64                   `json:"captured_size"`
	SourceSize       int64                   `json:"source_size"`
	SHA256           string                  `json:"sha256,omitempty"`
	Complete         bool                    `json:"complete"`
	Active           bool                    `json:"active"`
	PendingLine      bool                    `json:"pending_line,omitempty"`
	SQLiteAuxiliary  []string                `json:"sqlite_auxiliary,omitempty"`
	SnapshotMethod   string                  `json:"snapshot_method,omitempty"`
	Queued           bool                    `json:"queued,omitempty"`
	Diagnostics      []string                `json:"diagnostics,omitempty"`
	ActivityEvidence domain.ActivityEvidence `json:"activity_evidence,omitempty"`
}

// LiveSnapshotRequest 控制活动源的只读扫描和外部目标发布。
type LiveSnapshotRequest struct {
	SourceRoot      string
	DestinationRoot string
	ExcludePaths    []string
	ActivityOptions *activity.Options
	Progress        ProgressFunc
}

// LiveSnapshotPlan 是不创建目标目录的可审阅活动副本计划。
type LiveSnapshotPlan struct {
	Version         int              `json:"version"`
	Mode            string           `json:"mode"`
	SourceRoot      string           `json:"source_root"`
	DestinationRoot string           `json:"destination_root"`
	CreatedAt       time.Time        `json:"created_at"`
	Directories     []DirectoryEntry `json:"directories,omitempty"`
	Assets          []LiveAsset      `json:"assets"`
	ExcludedPaths   []string         `json:"excluded_paths,omitempty"`
	Activity        ActivitySummary  `json:"activity"`
	Warnings        []string         `json:"warnings,omitempty"`
}

// LiveSnapshotResult 描述部分发布是否成功以及哪些资产需要后续重试。
type LiveSnapshotResult struct {
	Published       bool        `json:"published"`
	Mode            string      `json:"mode"`
	SourceRoot      string      `json:"source_root"`
	DestinationRoot string      `json:"destination_root"`
	ManifestPath    string      `json:"manifest_path,omitempty"`
	CopiedAssets    int         `json:"copied_assets"`
	QueuedAssets    int         `json:"queued_assets"`
	Complete        bool        `json:"complete"`
	Assets          []LiveAsset `json:"assets,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
}

type liveManifest struct {
	Version        int              `json:"version"`
	Kind           string           `json:"kind"`
	Mode           string           `json:"mode"`
	CreatedAt      time.Time        `json:"created_at"`
	SourceRootName string           `json:"source_root_name"`
	Directories    []DirectoryEntry `json:"directories,omitempty"`
	Assets         []LiveAsset      `json:"assets"`
	ExcludedPaths  []string         `json:"excluded_paths,omitempty"`
	Warnings       []string         `json:"warnings,omitempty"`
}

// PrepareLive 对每个资产分别建立活动读取证据，不因单个 writer 使整个工作区失败。
func PrepareLive(ctx context.Context, request LiveSnapshotRequest) (LiveSnapshotPlan, error) {
	if err := ctx.Err(); err != nil {
		return LiveSnapshotPlan{}, err
	}
	source, destination, err := validateRoots(request.SourceRoot, request.DestinationRoot)
	if err != nil {
		return LiveSnapshotPlan{}, err
	}
	excluded, err := normalizeExcludedPaths(request.ExcludePaths)
	if err != nil {
		return LiveSnapshotPlan{}, err
	}
	plan := LiveSnapshotPlan{
		Version: 1, Mode: LiveBestEffort, SourceRoot: source, DestinationRoot: destination,
		CreatedAt: time.Now().UTC(), Directories: make([]DirectoryEntry, 0), Assets: make([]LiveAsset, 0),
		ExcludedPaths: append([]string(nil), excluded...),
	}
	err = walkSource(ctx, source, plan.ExcludedPaths, func(relative string, info os.FileInfo, path string) error {
		if relative == "." {
			return nil
		}
		if info.IsDir() {
			plan.Directories = append(plan.Directories, DirectoryEntry{RelativePath: relative, Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UTC()})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("活动副本遇到不支持的特殊文件：%s", relative)
		}
		if isSQLiteAuxiliary(relative) {
			if !containsPath(plan.ExcludedPaths, relative) {
				plan.ExcludedPaths = append(plan.ExcludedPaths, relative)
			}
			return nil
		}
		asset, captureErr := captureLiveAsset(ctx, relative, path)
		if captureErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			asset = LiveAsset{
				RelativePath: relative,
				Kind:         activityKind(relative),
				Status:       domain.StatusBlockedForMutation,
				Consistency:  "capture_error",
				Active:       true,
				Queued:       true,
				Diagnostics:  []string{"活动读取未能形成稳定捕获：" + captureErr.Error()},
			}
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("资产 %s 暂时无法捕获，已排队等待重试", relative))
		}
		plan.Assets = append(plan.Assets, asset)
		progress(request.Progress, Progress{Phase: "live-prepare", RelativePath: relative, FilesDone: int64(len(plan.Assets))})
		return nil
	}, func(relative string, _ fs.FileMode) error {
		if !containsPath(plan.ExcludedPaths, relative) {
			plan.ExcludedPaths = append(plan.ExcludedPaths, relative)
		}
		return nil
	})
	if err != nil {
		return plan, err
	}
	sortLivePlan(&plan)
	activityReport, err := probeLiveActivity(ctx, source, plan.Assets, request.ActivityOptions)
	if err != nil {
		return plan, err
	}
	plan.Activity = summarizeActivity(activityReport)
	applyLiveActivity(&plan.Assets, activityReport)
	if !activityReport.ProcessProbeAvailable {
		plan.Warnings = append(plan.Warnings, "无法确认打开文件进程；活动资产按不确定状态处理")
	}
	if plan.Activity.LockFileCount > 0 {
		plan.Warnings = append(plan.Warnings, "发现 writer lock；活动资产可阅读或生成前缀，但不允许破坏性写入")
	}
	if len(plan.ExcludedPaths) > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("已排除 %d 个运行时或 SQLite 旁路项", len(plan.ExcludedPaths)))
	}
	return plan, nil
}

// ExecuteLive 将稳定资产和可验证活动前缀发布为部分副本；冲突资产进入 queued 状态。
func ExecuteLive(ctx context.Context, plan LiveSnapshotPlan, options ExecuteOptions) (result LiveSnapshotResult, err error) {
	if err := validateLivePlan(plan); err != nil {
		return LiveSnapshotResult{}, err
	}
	parent := filepath.Dir(plan.DestinationRoot)
	createdParents, err := ensureDirectoryPath(parent)
	if err != nil {
		return LiveSnapshotResult{}, err
	}
	stage, err := createStaging(parent, filepath.Base(plan.DestinationRoot))
	if err != nil {
		removeEmptyDirectories(createdParents)
		return LiveSnapshotResult{}, err
	}
	var stagingManifest maintenance.ArtifactManifest
	if options.RegisterArtifact != nil {
		registered, registerErr := options.RegisterArtifact(ctx, maintenance.ArtifactManifest{
			Kind: maintenance.ArtifactStaging, TargetPath: stage, CreatedAt: time.Now().UTC(), Owner: "live-copy",
			Source: "活动副本 staging", Rebuildable: true, LeaseUntil: time.Now().UTC().Add(12 * time.Hour), Risk: "high",
			RetentionReason: "等待活动副本发布或中断恢复后再判断", WorkspaceRoot: plan.SourceRoot,
		})
		if registerErr != nil {
			_ = os.RemoveAll(stage)
			removeEmptyDirectories(createdParents)
			return LiveSnapshotResult{}, fmt.Errorf("登记活动副本 staging 失败：%w", registerErr)
		}
		stagingManifest = registered
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
			removeEmptyDirectories(createdParents)
		}
	}()
	for _, directory := range plan.Directories {
		path, joinErr := safeJoin(stage, directory.RelativePath)
		if joinErr != nil {
			return LiveSnapshotResult{}, joinErr
		}
		if mkdirErr := os.MkdirAll(path, 0o700); mkdirErr != nil {
			return LiveSnapshotResult{}, fmt.Errorf("创建活动副本目录失败：%w", mkdirErr)
		}
	}
	assets := append([]LiveAsset(nil), plan.Assets...)
	result = LiveSnapshotResult{Mode: LiveBestEffort, SourceRoot: plan.SourceRoot, DestinationRoot: plan.DestinationRoot, Assets: assets, CreatedAt: time.Now().UTC()}
	for index := range assets {
		if err := ctx.Err(); err != nil {
			return LiveSnapshotResult{}, err
		}
		asset := &assets[index]
		if asset.Queued || asset.Consistency == string(catalog.LiveRewriteConflict) || asset.Consistency == string(catalog.LiveTruncated) {
			if asset.Status == domain.StatusStable {
				asset.Status = domain.StatusBlockedForMutation
			}
			asset.Queued = true
			asset.Complete = false
			asset.Diagnostics = append(asset.Diagnostics, "源资产在活动读取中不具备可验证前缀，等待后续重试")
			result.QueuedAssets++
			continue
		}
		var copyErr error
		if asset.Kind == domain.AssetKindSQLite {
			copyErr = copyLiveSQLite(ctx, plan.SourceRoot, stage, asset.RelativePath)
		} else {
			var copyState livePrefixCopyState
			copyState, copyErr = copyLivePrefix(ctx, plan.SourceRoot, stage, *asset)
			if copyErr == nil && copyState.SourceChanged {
				asset.Active = true
				asset.Complete = false
				if copyState.Consistency != "" {
					asset.Consistency = copyState.Consistency
				}
				if copyState.Diagnostic != "" {
					asset.Diagnostics = append(asset.Diagnostics, copyState.Diagnostic)
				}
			}
		}
		if copyErr != nil {
			asset.Status = domain.StatusBlockedForMutation
			asset.Queued = true
			asset.Complete = false
			asset.Diagnostics = append(asset.Diagnostics, "活动副本发布时源资产发生变化："+copyErr.Error())
			result.QueuedAssets++
			continue
		}
		result.CopiedAssets++
		progress(options.Progress, Progress{Phase: "live-copy", RelativePath: asset.RelativePath, FilesDone: int64(result.CopiedAssets), FilesTotal: int64(len(assets))})
	}
	manifest := liveManifest{Version: 1, Kind: LiveManifestName, Mode: LiveBestEffort, CreatedAt: time.Now().UTC(), SourceRootName: filepath.Base(plan.SourceRoot), Directories: plan.Directories, Assets: assets, ExcludedPaths: plan.ExcludedPaths, Warnings: plan.Warnings}
	if err := writeLiveManifest(stage, manifest); err != nil {
		return LiveSnapshotResult{}, err
	}
	if err := syncDirectory(stage); err != nil {
		return LiveSnapshotResult{}, err
	}
	if err := ensureDestinationAbsent(plan.DestinationRoot); err != nil {
		return LiveSnapshotResult{}, err
	}
	if err := os.Rename(stage, plan.DestinationRoot); err != nil {
		return LiveSnapshotResult{}, fmt.Errorf("发布活动副本失败：%w", err)
	}
	published = true
	if options.RegisterArtifact != nil {
		stagingManifest.TargetPath = plan.DestinationRoot
		stagingManifest.State = "published"
		stagingManifest.Rebuildable = false
		stagingManifest.LeaseUntil = time.Time{}
		stagingManifest.RetentionReason = "活动副本已发布；等待用户明确过期后处理"
		if _, updateErr := options.RegisterArtifact(ctx, stagingManifest); updateErr != nil {
			return LiveSnapshotResult{}, fmt.Errorf("更新活动副本生成物登记失败：%w", updateErr)
		}
	}
	_ = syncDirectory(parent)
	result.Published = true
	result.ManifestPath = filepath.Join(plan.DestinationRoot, LiveManifestName)
	result.Assets = assets
	result.Complete = result.QueuedAssets == 0 && allLiveAssetsComplete(assets)
	return result, nil
}

func captureLiveAsset(ctx context.Context, relative, path string) (LiveAsset, error) {
	kind := activityKind(relative)
	if kind == domain.AssetKindSQLite {
		state, err := captureLiveSQLite(ctx, path)
		if err != nil {
			return LiveAsset{}, err
		}
		return LiveAsset{RelativePath: relative, Kind: kind, Status: domain.StatusStable, Consistency: "sqlite_online_backup", CapturedSize: state.size, SourceSize: state.size, Complete: true, SnapshotMethod: "sqlite_stable_clone_online_backup", SQLiteAuxiliary: sqliteguard.ExistingAuxiliaryFiles(path)}, nil
	}
	if kind == domain.AssetKindJSONL || kind == domain.AssetKindSessionIndex {
		snapshot, err := catalog.ScanLiveJSONL(ctx, path)
		if err != nil {
			return LiveAsset{}, err
		}
		return liveAssetFromState(relative, kind, snapshot.State), nil
	}
	state, err := catalog.CaptureLiveFile(ctx, path)
	if err != nil {
		return LiveAsset{}, err
	}
	return liveAssetFromState(relative, kind, state), nil
}

func liveAssetFromState(relative string, kind domain.AssetKind, state catalog.LiveFileState) LiveAsset {
	status := domain.StatusStable
	switch state.Consistency {
	case catalog.LiveAppending:
		status = domain.StatusGrowing
	case catalog.LivePendingLine:
		status = domain.StatusPendingJSONLLine
	case catalog.LiveRewriteConflict, catalog.LiveTruncated:
		status = domain.StatusSourceChangedDuringProbe
	}
	return LiveAsset{RelativePath: relative, Kind: kind, Status: status, Consistency: string(state.Consistency), CapturedSize: state.CapturedBytes, SourceSize: state.SourceSizeBytes, SHA256: state.SHA256, Complete: state.Complete, Active: state.Active, PendingLine: state.PendingLine, Diagnostics: append([]string(nil), state.Diagnostics...)}
}

func captureLiveSQLite(ctx context.Context, path string) (fileState, error) {
	snapshot, err := cloneLiveSQLite(ctx, path)
	if err != nil {
		return fileState{}, err
	}
	defer snapshot.cleanup()
	dsn, _, err := sqliteguard.LiveReadOnlyDSN(snapshot.databasePath, 1000)
	if err != nil {
		return fileState{}, err
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fileState{}, fmt.Errorf("打开 WAL 感知 SQLite 失败：%w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.PingContext(ctx); err != nil {
		return fileState{}, fmt.Errorf("读取 WAL 感知 SQLite 失败：%w", err)
	}
	transaction, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fileState{}, fmt.Errorf("开始 SQLite 只读事务失败：%w", err)
	}
	var schemaVersion int
	if err := transaction.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&schemaVersion); err != nil {
		_ = transaction.Rollback()
		return fileState{}, fmt.Errorf("读取 SQLite schema_version 失败：%w", err)
	}
	_ = schemaVersion
	if err := transaction.Commit(); err != nil {
		return fileState{}, fmt.Errorf("结束 SQLite 只读事务失败：%w", err)
	}
	return fileState{size: snapshot.sourceSize}, nil
}

type fileState struct{ size int64 }

func probeLiveActivity(ctx context.Context, root string, assets []LiveAsset, options *activity.Options) (activity.Report, error) {
	targets := make([]activity.Target, 0, len(assets))
	for _, asset := range assets {
		targets = append(targets, activity.Target{RelativePath: asset.RelativePath, Kind: asset.Kind})
	}
	return activity.Probe(ctx, root, targets, activityOptionsOrDefault(options))
}

func activityOptionsOrDefault(options *activity.Options) activity.Options {
	if options == nil {
		return activity.DefaultOptions()
	}
	return *options
}

func applyLiveActivity(assets *[]LiveAsset, report activity.Report) {
	if assets == nil {
		return
	}
	byPath := make(map[string]activity.AssetResult, len(report.Results))
	for _, result := range report.Results {
		byPath[result.RelativePath] = result
	}
	for index := range *assets {
		asset := &(*assets)[index]
		result, ok := byPath[asset.RelativePath]
		if !ok {
			continue
		}
		asset.ActivityEvidence = result.Evidence
		if result.Status == domain.StatusStable && !result.MutationBlocked && result.ActivityStatus == domain.StatusStable {
			asset.Status = domain.StatusStable
			continue
		}
		if result.ActivityStatus != "" {
			asset.Status = result.ActivityStatus
		} else {
			asset.Status = result.Status
		}
		asset.Active = true
		asset.Diagnostics = append(asset.Diagnostics, fmt.Sprintf("活动探测状态：%s；%s", result.Status, result.Evidence.Reason))
		if asset.Kind != domain.AssetKindSQLite || asset.SnapshotMethod != "sqlite_stable_clone_online_backup" || result.Status == domain.StatusUnknown || result.Status == domain.StatusConflict {
			asset.Complete = false
		}
		if asset.Consistency == string(catalog.LiveStable) {
			asset.Consistency = "active_process_or_lock"
		}
	}
}

// livePrefixCopyState 描述前缀复制窗口内源文件是否继续变化。
type livePrefixCopyState struct {
	SourceChanged bool
	Consistency   string
	Diagnostic    string
}

// copyLivePrefix 只复制计划已经捕获的字节范围，并在复制前后复核源文件状态。
// 追加可以保留已捕获前缀，但必须将资产标记为活动；截断、替换或内容改写直接排队。
func copyLivePrefix(ctx context.Context, sourceRoot, stage string, asset LiveAsset) (livePrefixCopyState, error) {
	source, err := safeJoin(sourceRoot, asset.RelativePath)
	if err != nil {
		return livePrefixCopyState{}, err
	}
	destination, err := safeJoin(stage, asset.RelativePath)
	if err != nil {
		return livePrefixCopyState{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return livePrefixCopyState{}, err
	}
	before, err := os.Lstat(source)
	if err != nil {
		return livePrefixCopyState{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return livePrefixCopyState{}, fmt.Errorf("活动源文件不是普通文件：%s", asset.RelativePath)
	}
	if before.Size() < asset.CapturedSize {
		return livePrefixCopyState{}, errors.New("活动源文件在复制前已截断")
	}
	input, err := os.Open(source)
	if err != nil {
		return livePrefixCopyState{}, err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return livePrefixCopyState{}, err
	}
	remove := true
	defer func() {
		_ = output.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	hash := sha256.New()
	read, err := io.CopyN(io.MultiWriter(output, hash), input, asset.CapturedSize)
	if err != nil && !(errors.Is(err, io.EOF) && read == asset.CapturedSize) {
		return livePrefixCopyState{}, err
	}
	if read != asset.CapturedSize || hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return livePrefixCopyState{}, errors.New("捕获前缀哈希或长度发生变化")
	}
	if err := output.Sync(); err != nil {
		return livePrefixCopyState{}, err
	}
	after, err := os.Lstat(source)
	if err != nil {
		return livePrefixCopyState{}, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() {
		return livePrefixCopyState{}, fmt.Errorf("活动源文件复制后不再是普通文件：%s", asset.RelativePath)
	}
	if !os.SameFile(before, after) || after.Size() < asset.CapturedSize {
		return livePrefixCopyState{}, errors.New("活动源文件在复制期间被替换或截断")
	}
	changed := before.Size() > asset.CapturedSize || after.Size() > asset.CapturedSize || !before.ModTime().Equal(after.ModTime())
	remove = false
	if err := ctx.Err(); err != nil {
		return livePrefixCopyState{}, err
	}
	if !changed {
		return livePrefixCopyState{}, nil
	}
	state := livePrefixCopyState{SourceChanged: true, Consistency: "active_append", Diagnostic: "源文件在活动副本复制窗口内继续变化，当前副本只包含已捕获前缀"}
	if before.Size() == asset.CapturedSize && after.Size() == asset.CapturedSize {
		state.Consistency = "active_changed"
		state.Diagnostic = "源文件修改时间在活动副本复制窗口内变化，当前副本只包含已捕获前缀"
	}
	return state, nil
}

func copyLiveSQLite(ctx context.Context, sourceRoot, stage, relative string) (err error) {
	source, err := safeJoin(sourceRoot, relative)
	if err != nil {
		return err
	}
	destination, err := safeJoin(stage, relative)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	snapshot, err := cloneLiveSQLite(ctx, source)
	if err != nil {
		return err
	}
	defer snapshot.cleanup()
	dsn, _, err := sqliteguard.LiveReadOnlyDSN(snapshot.databasePath, 2000)
	if err != nil {
		return err
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			removeLiveSQLiteArtifacts(destination)
		}
	}()
	if err := store.OnlineBackup(ctx, database, destination); err != nil {
		return fmt.Errorf("SQLite online backup 失败：%w", err)
	}
	cleanup = false
	return nil
}

func removeLiveSQLiteArtifacts(path string) {
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		_ = os.Remove(candidate)
	}
}

// liveSQLiteSnapshot 是隔离的 SQLite WAL 读取输入；其临时目录只供当前读取使用。
// 该对象不是可发布的文件快照，发布前仍必须由 SQLite online backup 生成目标数据库。
type liveSQLiteSnapshot struct {
	directory    string
	databasePath string
	sourceSize   int64
}

func (snapshot liveSQLiteSnapshot) cleanup() {
	if snapshot.directory != "" {
		_ = os.RemoveAll(snapshot.directory)
	}
}

type liveSQLiteFileState struct {
	info    os.FileInfo
	size    int64
	modTime time.Time
}

// cloneLiveSQLite 在不打开源 SQLite 的前提下复制主库和 WAL，并在复制窗口前后复核字节边界。
// SHM 只作为源活动证据读取，不复制其协调页；临时目录中的 SQLite 会自行重建本地 SHM
// 并通过 WAL-aware 只读连接验证数据，最终结果再经 online backup 发布。
// 读取发生变化时短暂重试；持续写入最终会作为单个 queued 资产返回给上层。
func cloneLiveSQLite(ctx context.Context, path string) (liveSQLiteSnapshot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return liveSQLiteSnapshot{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return liveSQLiteSnapshot{}, err
		}
		files, states, err := liveSQLiteSourceFiles(abs)
		if err != nil {
			return liveSQLiteSnapshot{}, err
		}
		cloneFiles := liveSQLiteCloneFiles(files)
		directory, err := os.MkdirTemp("", ".codex-chronicle-live-sqlite-*")
		if err != nil {
			return liveSQLiteSnapshot{}, fmt.Errorf("创建 SQLite 活动临时副本失败：%w", err)
		}
		failed := false
		for _, sourceFile := range cloneFiles {
			destination := filepath.Join(directory, filepath.Base(sourceFile))
			if err := copyLiveSQLiteFile(ctx, sourceFile, destination, states[sourceFile].size); err != nil {
				failed = true
				break
			}
		}
		if !failed {
			currentFiles, currentStates, currentErr := liveSQLiteSourceFiles(abs)
			if currentErr == nil && sameLiveSQLiteFileSet(cloneFiles, liveSQLiteCloneFiles(currentFiles)) && sameLiveSQLiteStates(liveSQLiteSelectedStates(states, cloneFiles), liveSQLiteSelectedStates(currentStates, cloneFiles)) {
				return liveSQLiteSnapshot{directory: directory, databasePath: filepath.Join(directory, filepath.Base(abs)), sourceSize: states[abs].size}, nil
			}
		}
		_ = os.RemoveAll(directory)
		if err := ctx.Err(); err != nil {
			return liveSQLiteSnapshot{}, err
		}
	}
	return liveSQLiteSnapshot{}, errors.New("SQLite 源文件在活动写入期间无法形成稳定字节副本")
}

func liveSQLiteCloneFiles(files []string) []string {
	cloneFiles := make([]string, 0, len(files))
	for _, file := range files {
		if strings.HasSuffix(strings.ToLower(file), "-shm") {
			continue
		}
		cloneFiles = append(cloneFiles, file)
	}
	return cloneFiles
}

func liveSQLiteSelectedStates(states map[string]liveSQLiteFileState, files []string) map[string]liveSQLiteFileState {
	selected := make(map[string]liveSQLiteFileState, len(files))
	for _, file := range files {
		if state, ok := states[file]; ok {
			selected[file] = state
		}
	}
	return selected
}

func liveSQLiteSourceFiles(path string) ([]string, map[string]liveSQLiteFileState, error) {
	mainState, err := liveSQLiteFileStateOf(path)
	if err != nil {
		return nil, nil, err
	}
	files := []string{path}
	states := map[string]liveSQLiteFileState{path: mainState}
	for _, auxiliary := range sqliteguard.ExistingAuxiliaryFiles(path) {
		state, stateErr := liveSQLiteFileStateOf(auxiliary)
		if stateErr != nil {
			return nil, nil, stateErr
		}
		files = append(files, auxiliary)
		states[auxiliary] = state
	}
	return files, states, nil
}

func liveSQLiteFileStateOf(path string) (liveSQLiteFileState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return liveSQLiteFileState{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return liveSQLiteFileState{}, fmt.Errorf("SQLite 活动文件必须是普通文件：%s", path)
	}
	return liveSQLiteFileState{info: info, size: info.Size(), modTime: info.ModTime()}, nil
}

func copyLiveSQLiteFile(ctx context.Context, source, destination string, size int64) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = output.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	reader := &liveContextReader{ctx: ctx, reader: io.LimitReader(input, size)}
	read, err := io.Copy(output, reader)
	if err != nil {
		return err
	}
	if read != size {
		return fmt.Errorf("SQLite 活动文件读取长度变化：%s", source)
	}
	if err := output.Sync(); err != nil {
		return err
	}
	remove = false
	return nil
}

type liveContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *liveContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func sameLiveSQLiteFileSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameLiveSQLiteStates(left, right map[string]liveSQLiteFileState) bool {
	if len(left) != len(right) {
		return false
	}
	for path, before := range left {
		after, ok := right[path]
		if !ok || before.size != after.size || !before.modTime.Equal(after.modTime) || !os.SameFile(before.info, after.info) {
			return false
		}
	}
	return true
}

func writeLiveManifest(root string, manifest liveManifest) error {
	path := filepath.Join(root, LiveManifestName)
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	encodeErr := json.NewEncoder(file).Encode(manifest)
	if encodeErr == nil {
		encodeErr = file.Sync()
	}
	closeErr := file.Close()
	if encodeErr != nil {
		_ = os.Remove(temporary)
		return encodeErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return closeErr
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func validateLivePlan(plan LiveSnapshotPlan) error {
	if plan.Mode != LiveBestEffort {
		return fmt.Errorf("未知活动副本模式：%s", plan.Mode)
	}
	if strings.TrimSpace(plan.SourceRoot) == "" || strings.TrimSpace(plan.DestinationRoot) == "" {
		return errors.New("活动副本计划缺少源目录或目标目录")
	}
	if filepath.Base(plan.SourceRoot) != ".codex" {
		return errors.New("活动副本源目录必须是 .codex")
	}
	if hasPathComponent(plan.DestinationRoot, ".codex") || pathWithin(plan.SourceRoot, plan.DestinationRoot) || pathWithin(plan.DestinationRoot, plan.SourceRoot) {
		return errors.New("活动副本目标不能位于 .codex 路径内或与源目录重叠")
	}
	if err := ensureSourceRootSafe(plan.SourceRoot); err != nil {
		return err
	}
	if err := ensureDestinationAbsent(plan.DestinationRoot); err != nil {
		return err
	}
	return nil
}

func sortLivePlan(plan *LiveSnapshotPlan) {
	sort.Slice(plan.Directories, func(i, j int) bool { return plan.Directories[i].RelativePath < plan.Directories[j].RelativePath })
	sort.Slice(plan.Assets, func(i, j int) bool { return plan.Assets[i].RelativePath < plan.Assets[j].RelativePath })
	sort.Strings(plan.ExcludedPaths)
}

func allLiveAssetsComplete(assets []LiveAsset) bool {
	for _, asset := range assets {
		if asset.Queued || !asset.Complete {
			return false
		}
	}
	return true
}
