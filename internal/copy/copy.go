// 中文说明：从真实 .codex 只读制作可验证的稳定副本；源目录不写入，失败时只清理本次创建的 staging 目录。
package copy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/maintenance"
	"codex-chronicle.local/internal/sqliteguard"
)

const (
	// ManifestName 是副本根目录内的信任边界标记文件名。
	ManifestName = "codex-real-copy-only.json"
	// ManifestKind 只描述本项目生成的副本格式，不代表某一个真实 Codex schema 版本。
	ManifestKind = "codex-real-copy-v1"
	PlanVersion  = 1
	// DefaultChunkBytes 控制大文件复制的内存上限。
	DefaultChunkBytes int64 = 4 << 20
)

// MarkerSpec 允许兼容 adapter 在副本 manifest 中声明已知 schema；空值表示尚未绑定具体 adapter。
type MarkerSpec struct {
	Kind                  string `json:"kind"`
	AdapterID             string `json:"adapter_id,omitempty"`
	SchemaSHA256          string `json:"schema_sha256,omitempty"`
	SQLiteSemanticsSHA256 string `json:"sqlite_semantics_sha256,omitempty"`
}

// SQLiteRowSnapshot 是副本中控制面行身份的只读摘要；不保存任何行正文。
type SQLiteRowSnapshot struct {
	Database          string `json:"database"`
	RowCount          int64  `json:"row_count"`
	RowIdentitySHA256 string `json:"row_identity_sha256"`
	Complete          bool   `json:"complete"`
}

// SessionIndexSnapshot 是 session index 的可复核结构摘要；正文仍由原始文件哈希保护。
type SessionIndexSnapshot struct {
	RelativePath     string                     `json:"relative_path"`
	FileSHA256       string                     `json:"file_sha256"`
	LineCount        uint64                     `json:"line_count"`
	InvalidJSONLines uint64                     `json:"invalid_json_lines"`
	Entries          []domain.SessionIndexEntry `json:"entries,omitempty"`
	Complete         bool                       `json:"complete"`
}

// ControlPlaneSnapshot 汇总 SQLite schema、行身份和 session index 快照。
type ControlPlaneSnapshot struct {
	SchemaSemanticSHA256 string                 `json:"schema_semantic_sha256,omitempty"`
	SQLite               []domain.SQLiteSummary `json:"sqlite,omitempty"`
	Rows                 []SQLiteRowSnapshot    `json:"rows,omitempty"`
	SessionIndex         []SessionIndexSnapshot `json:"session_index,omitempty"`
	Complete             bool                   `json:"complete"`
	CompletenessState    string                 `json:"completeness_state"`
	Diagnostics          []string               `json:"diagnostics,omitempty"`
}

// FileEntry 是源目录中一个普通文件的不可变指纹。
type FileEntry struct {
	RelativePath string    `json:"relative_path"`
	SizeBytes    int64     `json:"size_bytes"`
	Mode         uint32    `json:"mode"`
	ModTime      time.Time `json:"mod_time"`
	Identity     string    `json:"identity,omitempty"`
	SHA256       string    `json:"sha256"`
}

// DirectoryEntry 是源目录中一个普通目录的元数据；保留空目录和权限/时间。
type DirectoryEntry struct {
	RelativePath string    `json:"relative_path"`
	Mode         uint32    `json:"mode"`
	ModTime      time.Time `json:"mod_time"`
}

// Progress 描述复制进度；回调应快速返回，不应修改源目录或目标目录。
type Progress struct {
	Phase        string `json:"phase"`
	RelativePath string `json:"relative_path,omitempty"`
	FilesDone    int64  `json:"files_done"`
	FilesTotal   int64  `json:"files_total"`
	BytesDone    int64  `json:"bytes_done"`
	BytesTotal   int64  `json:"bytes_total"`
}

// ProgressFunc 接收 Prepare/Execute/Verify 的进度事件。
type ProgressFunc func(Progress)

// ActivitySummary 保留活动探测边界证据和少量异常样本，避免 80GB 级目录把全部结果长期留在任务内存。
type ActivitySummary struct {
	SchemaVersion         int                    `json:"schema_version"`
	Root                  string                 `json:"root"`
	GeneratedAt           time.Time              `json:"generated_at"`
	IntervalMS            int                    `json:"interval_ms"`
	ProcessProbeAvailable bool                   `json:"process_probe_available"`
	ProcessProbeReason    string                 `json:"process_probe_reason,omitempty"`
	LockProbeAvailable    bool                   `json:"lock_probe_available"`
	LockProbeReason       string                 `json:"lock_probe_reason,omitempty"`
	LockFileCount         int                    `json:"lock_file_count"`
	LockIDs               []string               `json:"lock_ids,omitempty"`
	StableCount           int                    `json:"stable_count"`
	BlockedCount          int                    `json:"blocked_count"`
	Results               []activity.AssetResult `json:"results,omitempty"`
}

// CopyRequest 是稳定副本制作的只读探测参数。
type CopyRequest struct {
	SourceRoot      string
	DestinationRoot string
	ChunkBytes      int64
	Marker          MarkerSpec
	// ExcludePaths 是相对于 .codex 根的显式排除清单；目录会递归排除。
	// 调用方应使用它排除凭证、配置和其他不属于测试副本的敏感运行时文件。
	ExcludePaths []string
	// AllowUnavailableProcessProbe 仅在用户明确承担风险时允许继续；默认 false。
	AllowUnavailableProcessProbe bool
	ActivityOptions              *activity.Options
	Progress                     ProgressFunc
}

// CopyPlan 是可审阅、可序列化的稳定副本计划。Prepare 不会创建目标目录。
type CopyPlan struct {
	Version                      int                  `json:"version"`
	SourceRoot                   string               `json:"source_root"`
	DestinationRoot              string               `json:"destination_root"`
	CreatedAt                    time.Time            `json:"created_at"`
	Files                        []FileEntry          `json:"files"`
	Directories                  []DirectoryEntry     `json:"directories,omitempty"`
	ExcludedPaths                []string             `json:"excluded_paths,omitempty"`
	FileCount                    int64                `json:"file_count"`
	TotalBytes                   int64                `json:"total_bytes"`
	SourceManifestSHA256         string               `json:"source_manifest_sha256"`
	Marker                       MarkerSpec           `json:"marker"`
	Activity                     ActivitySummary      `json:"activity"`
	AllowUnavailableProcessProbe bool                 `json:"allow_unavailable_process_probe,omitempty"`
	AuxiliarySQLiteFiles         []string             `json:"auxiliary_sqlite_files,omitempty"`
	Warnings                     []string             `json:"warnings,omitempty"`
	ControlPlane                 ControlPlaneSnapshot `json:"control_plane"`
	CompletenessState            string               `json:"completeness_state"`
	CompletenessReasons          []string             `json:"completeness_reasons,omitempty"`
}

// ExecuteOptions 控制计划执行；计划本身仍绑定 Prepare 时记录的源指纹。
type ExecuteOptions struct {
	ChunkBytes int64
	Progress   ProgressFunc
	// RegisterArtifact 可选登记副本 staging；默认只读 daemon 不提供它。
	RegisterArtifact func(context.Context, maintenance.ArtifactManifest) (maintenance.ArtifactManifest, error)
}

// CopyResult 是一次完整发布或失败清理后的结果。
type CopyResult struct {
	Published                 bool      `json:"published"`
	SourceRoot                string    `json:"source_root"`
	DestinationRoot           string    `json:"destination_root"`
	ManifestPath              string    `json:"manifest_path,omitempty"`
	FileCount                 int64     `json:"file_count"`
	TotalBytes                int64     `json:"total_bytes"`
	SourceManifestSHA256      string    `json:"source_manifest_sha256"`
	DestinationManifestSHA256 string    `json:"destination_manifest_sha256,omitempty"`
	CreatedAt                 time.Time `json:"created_at"`
}

// DestinationReport 是对已发布副本的再次只读校验结果。
type DestinationReport struct {
	Root                 string               `json:"root"`
	Manifest             MarkerSpec           `json:"manifest"`
	FileCount            int64                `json:"file_count"`
	TotalBytes           int64                `json:"total_bytes"`
	SourceManifestSHA256 string               `json:"source_manifest_sha256"`
	Verified             bool                 `json:"verified"`
	ControlPlane         ControlPlaneSnapshot `json:"control_plane"`
	CompletenessState    string               `json:"completeness_state"`
}

type manifest struct {
	Version               int       `json:"version"`
	Kind                  string    `json:"kind"`
	AdapterID             string    `json:"adapter_id,omitempty"`
	SchemaSHA256          string    `json:"schema_sha256,omitempty"`
	SQLiteSemanticsSHA256 string    `json:"sqlite_semantics_sha256,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	// SourceRoot 仅用于副本内绝对 rollout_path 的受限路径映射，不改变源目录。
	SourceRoot           string               `json:"source_root,omitempty"`
	SourceRootName       string               `json:"source_root_name"`
	SourceManifestSHA256 string               `json:"source_manifest_sha256"`
	FileCount            int64                `json:"file_count"`
	TotalBytes           int64                `json:"total_bytes"`
	Files                []FileEntry          `json:"files"`
	Directories          []DirectoryEntry     `json:"directories,omitempty"`
	ExcludedPaths        []string             `json:"excluded_paths,omitempty"`
	ControlPlane         ControlPlaneSnapshot `json:"control_plane"`
	CompletenessState    string               `json:"completeness_state"`
	CompletenessReasons  []string             `json:"completeness_reasons,omitempty"`
}

// Prepare 只读扫描并哈希源 .codex，完成后再次通过活动探测和指纹校验。
func Prepare(ctx context.Context, request CopyRequest) (CopyPlan, error) {
	if err := ctx.Err(); err != nil {
		return CopyPlan{}, err
	}
	source, destination, err := validateRoots(request.SourceRoot, request.DestinationRoot)
	if err != nil {
		return CopyPlan{}, err
	}
	chunkBytes := normalizeChunkBytes(request.ChunkBytes)
	marker := normalizeMarker(request.Marker)
	plan := CopyPlan{
		Version: PlanVersion, SourceRoot: source, DestinationRoot: destination,
		CreatedAt: time.Now().UTC(), Files: make([]FileEntry, 0), Directories: make([]DirectoryEntry, 0), Marker: marker,
		AllowUnavailableProcessProbe: request.AllowUnavailableProcessProbe,
	}
	excludedPaths, err := normalizeExcludedPaths(request.ExcludePaths)
	if err != nil {
		return CopyPlan{}, err
	}
	plan.ExcludedPaths = append(plan.ExcludedPaths, excludedPaths...)
	if err := walkSource(ctx, source, plan.ExcludedPaths, func(relative string, info os.FileInfo, path string) error {
		if relative == "." {
			return nil
		}
		if info.IsDir() {
			plan.Directories = append(plan.Directories, DirectoryEntry{RelativePath: relative, Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UTC()})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("源目录包含不支持的特殊文件：%s", relative)
		}
		digest, size, hashErr := hashFile(ctx, path, chunkBytes)
		if hashErr != nil {
			return fmt.Errorf("读取源文件 %s 失败：%w", relative, hashErr)
		}
		after, statErr := os.Lstat(path)
		if statErr != nil {
			return fmt.Errorf("读取源文件 %s 结束状态失败：%w", relative, statErr)
		}
		if !sameFileState(info, after) || size != info.Size() {
			return fmt.Errorf("源文件在哈希期间发生变化：%s", relative)
		}
		entry := FileEntry{RelativePath: relative, SizeBytes: size, Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UTC(), Identity: fileIdentity(info), SHA256: digest}
		plan.Files = append(plan.Files, entry)
		plan.FileCount++
		plan.TotalBytes += size
		if isSQLiteAuxiliary(relative) {
			plan.AuxiliarySQLiteFiles = append(plan.AuxiliarySQLiteFiles, relative)
		}
		progress(request.Progress, Progress{Phase: "prepare-hash", RelativePath: relative, FilesDone: plan.FileCount, BytesDone: plan.TotalBytes})
		return nil
	}, func(relative string, _ fs.FileMode) error {
		if !containsPath(plan.ExcludedPaths, relative) {
			plan.ExcludedPaths = append(plan.ExcludedPaths, relative)
		}
		return nil
	}); err != nil {
		return CopyPlan{}, err
	}
	sortPlan(&plan)
	if len(plan.ExcludedPaths) > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("已排除 %d 个文件或目录；具体路径已写入副本计划。", len(plan.ExcludedPaths)))
	}
	if err := rejectSQLiteAuxiliaryFiles(source, plan.Files); err != nil {
		return plan, err
	}
	if len(plan.AuxiliarySQLiteFiles) > 0 {
		if err := validateSQLiteAuxiliarySet(plan.Files, plan.AuxiliarySQLiteFiles); err != nil {
			return CopyPlan{}, err
		}
		return plan, fmt.Errorf("源目录存在 SQLite WAL/SHM，严格稳定模式拒绝声称完整副本：%s", strings.Join(plan.AuxiliarySQLiteFiles, "、"))
	}
	manifestHash, err := sourceManifestHash(plan.Files, plan.Directories, plan.ExcludedPaths)
	if err != nil {
		return CopyPlan{}, err
	}
	plan.SourceManifestSHA256 = manifestHash
	activityOptions := request.ActivityOptions
	if request.AllowUnavailableProcessProbe {
		options := activity.DefaultOptions()
		if activityOptions != nil {
			options = *activityOptions
		}
		options.Conservative = false
		activityOptions = &options
	}
	activityReport, err := probeActivity(ctx, source, plan.Files, activityOptions)
	if err != nil {
		return plan, err
	}
	plan.Activity = summarizeActivity(activityReport)
	if err := validateActivity(activityReport, request.AllowUnavailableProcessProbe); err != nil {
		return plan, err
	}
	if !activityReport.ProcessProbeAvailable {
		plan.Warnings = append(plan.Warnings, "无法确认打开文件进程；本次副本依赖源前后双重哈希和锁探测")
	}
	if err := verifySource(ctx, plan, chunkBytes, request.Progress, "prepare-verify"); err != nil {
		return plan, err
	}
	plan.ControlPlane = captureControlPlaneSnapshot(ctx, source, plan.Files)
	plan.CompletenessState = plan.ControlPlane.CompletenessState
	plan.CompletenessReasons = append([]string(nil), plan.ControlPlane.Diagnostics...)
	if len(plan.CompletenessReasons) > 0 {
		plan.Warnings = append(plan.Warnings, plan.CompletenessReasons...)
	}
	if marker.Kind == ManifestKind && !plan.ControlPlane.Complete {
		plan.Warnings = append(plan.Warnings, "控制面快照不完整；副本只能用于只读审阅，mutation adapter 将拒绝写入")
	}
	return plan, nil
}

// Execute 在目标同父目录创建 staging，复制并验证全部资产后原子发布。
func Execute(ctx context.Context, plan CopyPlan, options ExecuteOptions) (result CopyResult, err error) {
	if err := validatePlan(plan); err != nil {
		return CopyResult{}, err
	}
	if err := rejectSQLiteAuxiliaryFiles(plan.SourceRoot, plan.Files); err != nil {
		return CopyResult{}, err
	}
	activityReport, activityErr := probeActivity(ctx, plan.SourceRoot, plan.Files, planRevalidationActivityOptions(plan))
	if activityErr != nil {
		return CopyResult{}, activityErr
	}
	if err := validateActivity(activityReport, plan.AllowUnavailableProcessProbe); err != nil {
		return CopyResult{}, err
	}
	chunkBytes := normalizeChunkBytes(options.ChunkBytes)
	if err := verifySource(ctx, plan, chunkBytes, options.Progress, "execute-preflight"); err != nil {
		return CopyResult{}, err
	}
	if err := ensureDestinationAbsent(plan.DestinationRoot); err != nil {
		return CopyResult{}, err
	}
	parent := filepath.Dir(plan.DestinationRoot)
	createdParents, err := ensureDirectoryPath(parent)
	if err != nil {
		return CopyResult{}, err
	}
	stage, err := createStaging(parent, filepath.Base(plan.DestinationRoot))
	if err != nil {
		removeEmptyDirectories(createdParents)
		return CopyResult{}, err
	}
	var stagingManifest maintenance.ArtifactManifest
	if options.RegisterArtifact != nil {
		registered, registerErr := options.RegisterArtifact(ctx, maintenance.ArtifactManifest{
			Kind: maintenance.ArtifactStaging, TargetPath: stage, CreatedAt: time.Now().UTC(), Owner: "copy",
			Source: "稳定副本 staging", Rebuildable: true, LeaseUntil: time.Now().UTC().Add(24 * time.Hour), Risk: "medium",
			RetentionReason: "等待副本发布或中断恢复后再判断", WorkspaceRoot: plan.SourceRoot,
		})
		if registerErr != nil {
			_ = os.RemoveAll(stage)
			removeEmptyDirectories(createdParents)
			return CopyResult{}, fmt.Errorf("登记副本 staging 失败：%w", registerErr)
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
	if err := copyToStaging(ctx, plan, stage, chunkBytes, options.Progress); err != nil {
		return CopyResult{}, err
	}
	if err := writeManifest(stage, plan); err != nil {
		return CopyResult{}, err
	}
	if err := verifyDestination(ctx, stage, plan, chunkBytes, options.Progress); err != nil {
		return CopyResult{}, err
	}
	if err := verifySource(ctx, plan, chunkBytes, options.Progress, "execute-postflight"); err != nil {
		return CopyResult{}, err
	}
	if err := syncDirectory(stage); err != nil {
		return CopyResult{}, err
	}
	if err := ensureDestinationAbsent(plan.DestinationRoot); err != nil {
		return CopyResult{}, err
	}
	if err := os.Rename(stage, plan.DestinationRoot); err != nil {
		return CopyResult{}, fmt.Errorf("发布稳定副本失败：%w", err)
	}
	published = true
	if options.RegisterArtifact != nil {
		stagingManifest.TargetPath = plan.DestinationRoot
		stagingManifest.State = "published"
		stagingManifest.Rebuildable = false
		stagingManifest.LeaseUntil = time.Time{}
		stagingManifest.RetentionReason = "稳定副本已发布；等待用户明确过期后处理"
		if _, updateErr := options.RegisterArtifact(ctx, stagingManifest); updateErr != nil {
			return CopyResult{}, fmt.Errorf("更新稳定副本生成物登记失败：%w", updateErr)
		}
	}
	if err := syncDirectory(parent); err != nil {
		return CopyResult{}, err
	}
	result = CopyResult{
		Published: true, SourceRoot: plan.SourceRoot, DestinationRoot: plan.DestinationRoot,
		ManifestPath: filepath.Join(plan.DestinationRoot, ManifestName), FileCount: plan.FileCount,
		TotalBytes: plan.TotalBytes, SourceManifestSHA256: plan.SourceManifestSHA256,
		DestinationManifestSHA256: plan.SourceManifestSHA256, CreatedAt: time.Now().UTC(),
	}
	return result, nil
}

// VerifyDestination 重新读取副本 manifest、目录结构和每个文件哈希。
func VerifyDestination(ctx context.Context, root string) (DestinationReport, error) {
	abs, err := validateDestinationRoot(root)
	if err != nil {
		return DestinationReport{}, err
	}
	marker, plan, err := readManifest(abs)
	if err != nil {
		return DestinationReport{}, err
	}
	if err := verifyDestination(ctx, abs, plan, DefaultChunkBytes, nil); err != nil {
		return DestinationReport{}, err
	}
	actualControlPlane := captureControlPlaneSnapshot(ctx, abs, plan.Files)
	if !controlPlaneSnapshotsEqual(plan.ControlPlane, actualControlPlane) {
		return DestinationReport{}, errors.New("副本控制面 schema、行身份或 session index 快照不匹配")
	}
	return DestinationReport{Root: abs, Manifest: MarkerSpec{Kind: marker.Kind, AdapterID: marker.AdapterID, SchemaSHA256: marker.SchemaSHA256, SQLiteSemanticsSHA256: marker.SQLiteSemanticsSHA256}, FileCount: marker.FileCount, TotalBytes: marker.TotalBytes, SourceManifestSHA256: marker.SourceManifestSHA256, Verified: true, ControlPlane: actualControlPlane, CompletenessState: plan.CompletenessState}, nil
}

func validateRoots(sourceRoot, destinationRoot string) (string, string, error) {
	if strings.TrimSpace(sourceRoot) == "" || strings.TrimSpace(destinationRoot) == "" {
		return "", "", errors.New("稳定副本必须同时提供源目录和目标目录")
	}
	source, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", "", fmt.Errorf("解析源目录失败：%w", err)
	}
	destination, err := filepath.Abs(destinationRoot)
	if err != nil {
		return "", "", fmt.Errorf("解析目标目录失败：%w", err)
	}
	source = filepath.Clean(source)
	destination = filepath.Clean(destination)
	if !strings.EqualFold(filepath.Base(source), ".codex") {
		return "", "", fmt.Errorf("源目录必须是名为 .codex 的真实目录：%s", source)
	}
	if err := ensureSourceRootSafe(source); err != nil {
		return "", "", err
	}
	if hasPathComponent(destination, ".codex") {
		return "", "", fmt.Errorf("目标目录不能位于 .codex 路径内：%s", destination)
	}
	if pathWithin(source, destination) || pathWithin(destination, source) {
		return "", "", errors.New("源目录和目标目录不能相互包含")
	}
	if err := ensureDestinationAbsent(destination); err != nil {
		return "", "", err
	}
	if err := ensureExistingAncestorsSafe(filepath.Dir(destination)); err != nil {
		return "", "", err
	}
	return source, destination, nil
}

func validateDestinationRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("读取副本目录失败：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("副本根必须是普通目录：%s", abs)
	}
	if hasPathComponent(abs, ".codex") {
		return "", fmt.Errorf("副本根不能位于 .codex 路径内：%s", abs)
	}
	if err := ensureExistingAncestorsSafe(abs); err != nil {
		return "", err
	}
	return abs, nil
}

func ensureSourceRootSafe(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("读取源 .codex 失败：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("源 .codex 必须是普通目录，不能是符号链接：%s", path)
	}
	if err := ensureExistingAncestorsSafe(path); err != nil {
		return err
	}
	return nil
}

func ensureDestinationAbsent(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("目标目录已存在，拒绝覆盖：%s", path)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("检查目标目录失败：%w", err)
	}
	return nil
}

func ensureExistingAncestorsSafe(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("路径祖先不能是符号链接：%s", current)
			}
			if !info.IsDir() {
				return fmt.Errorf("路径祖先不是目录：%s", current)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("检查路径祖先失败：%w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func ensureDirectoryPath(path string) ([]string, error) {
	clean := filepath.Clean(path)
	missing := make([]string, 0)
	current := clean
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("目标父路径不是安全目录：%s", current)
			}
			break
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("检查目标父路径失败：%w", err)
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	created := make([]string, 0, len(missing))
	for index := len(missing) - 1; index >= 0; index-- {
		mkdirErr := os.Mkdir(missing[index], 0o700)
		if mkdirErr != nil && !os.IsExist(mkdirErr) {
			return nil, fmt.Errorf("创建目标父目录失败：%w", mkdirErr)
		}
		info, err := os.Lstat(missing[index])
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("目标父目录创建后不安全：%s", missing[index])
		}
		if mkdirErr == nil {
			created = append(created, missing[index])
		}
	}
	return created, nil
}

func removeEmptyDirectories(paths []string) {
	for index := len(paths) - 1; index >= 0; index-- {
		entries, err := os.ReadDir(paths[index])
		if err != nil || len(entries) != 0 {
			continue
		}
		_ = os.Remove(paths[index])
	}
}

func walkSource(ctx context.Context, root string, excluded []string, visit func(string, os.FileInfo, string) error, onSkip func(string, fs.FileMode) error) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative := relativePath(path, root)
		mode := entry.Type() | info.Mode()
		if relative != "." && containsPath(excluded, relative) {
			if onSkip != nil {
				if err := onSkip(relative, mode); err != nil {
					return err
				}
			}
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if isIgnorableRuntimeEntry(relative, mode) {
			if onSkip == nil {
				return nil
			}
			if info.IsDir() {
				if err := onSkip(relative, mode); err != nil {
					return err
				}
				return fs.SkipDir
			}
			return onSkip(relative, mode)
		}
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("源目录包含符号链接：%s", filepath.ToSlash(relative))
		}
		return visit(relative, info, path)
	})
}

func verifySource(ctx context.Context, plan CopyPlan, chunkBytes int64, progressFn ProgressFunc, phase string) error {
	entries := make(map[string]FileEntry, len(plan.Files))
	for _, entry := range plan.Files {
		entries[entry.RelativePath] = entry
	}
	seenFiles := make(map[string]struct{}, len(entries))
	expectedDirs := make(map[string]DirectoryEntry, len(plan.Directories))
	for _, entry := range plan.Directories {
		expectedDirs[entry.RelativePath] = entry
	}
	seenDirs := make(map[string]struct{}, len(plan.Directories))
	expectedExcluded := make(map[string]struct{}, len(plan.ExcludedPaths))
	for _, relative := range plan.ExcludedPaths {
		expectedExcluded[relative] = struct{}{}
	}
	seenExcluded := make(map[string]struct{}, len(plan.ExcludedPaths))
	var filesDone, bytesDone int64
	err := walkSource(ctx, plan.SourceRoot, plan.ExcludedPaths, func(relative string, info os.FileInfo, path string) error {
		if relative == "." {
			return nil
		}
		if info.IsDir() {
			expected, ok := expectedDirs[relative]
			if !ok {
				return fmt.Errorf("源目录出现计划外目录：%s", relative)
			}
			if info.Mode().Perm() != os.FileMode(expected.Mode) || !info.ModTime().Equal(expected.ModTime) {
				return fmt.Errorf("源目录元数据发生变化：%s", relative)
			}
			seenDirs[relative] = struct{}{}
			return nil
		}
		expected, ok := entries[relative]
		if !ok {
			return fmt.Errorf("源目录出现计划外文件：%s", relative)
		}
		if !info.Mode().IsRegular() || !samePlanMetadata(expected, info) {
			return fmt.Errorf("源文件元数据发生变化：%s（计划 identity=%q size=%d mode=%o mtime=%s，当前 identity=%q size=%d mode=%o mtime=%s）", relative, expected.Identity, expected.SizeBytes, expected.Mode, expected.ModTime.UTC().Format(time.RFC3339Nano), fileIdentity(info), info.Size(), info.Mode().Perm(), info.ModTime().UTC().Format(time.RFC3339Nano))
		}
		digest, size, hashErr := hashFile(ctx, path, chunkBytes)
		if hashErr != nil {
			return fmt.Errorf("验证源文件 %s 失败：%w", relative, hashErr)
		}
		if size != expected.SizeBytes || digest != expected.SHA256 {
			return fmt.Errorf("源文件哈希发生变化：%s", relative)
		}
		seenFiles[relative] = struct{}{}
		filesDone++
		bytesDone += size
		progress(progressFn, Progress{Phase: phase, RelativePath: relative, FilesDone: filesDone, FilesTotal: plan.FileCount, BytesDone: bytesDone, BytesTotal: plan.TotalBytes})
		return nil
	}, func(relative string, _ fs.FileMode) error {
		if _, ok := expectedExcluded[relative]; !ok {
			return fmt.Errorf("源目录出现计划外运行时排除项：%s", relative)
		}
		seenExcluded[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seenFiles) != len(entries) || len(seenDirs) != len(plan.Directories) || len(seenExcluded) != len(expectedExcluded) {
		return fmt.Errorf("源目录文件清单与稳定副本计划不一致")
	}
	return nil
}

func copyToStaging(ctx context.Context, plan CopyPlan, stage string, chunkBytes int64, progressFn ProgressFunc) error {
	for _, directory := range plan.Directories {
		path, err := safeJoin(stage, directory.RelativePath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("创建副本目录 %s 失败：%w", directory.RelativePath, err)
		}
	}
	var filesDone, bytesDone int64
	for _, entry := range plan.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		source, err := safeJoin(plan.SourceRoot, entry.RelativePath)
		if err != nil {
			return err
		}
		target, err := safeJoin(stage, entry.RelativePath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("创建副本父目录失败：%w", err)
		}
		if err := copyFile(ctx, source, target, entry, chunkBytes); err != nil {
			return err
		}
		filesDone++
		bytesDone += entry.SizeBytes
		progress(progressFn, Progress{Phase: "copy", RelativePath: entry.RelativePath, FilesDone: filesDone, FilesTotal: plan.FileCount, BytesDone: bytesDone, BytesTotal: plan.TotalBytes})
	}
	// 目录时间必须最后设置，否则创建文件会改变父目录 mtime。
	for index := len(plan.Directories) - 1; index >= 0; index-- {
		directory := plan.Directories[index]
		path, err := safeJoin(stage, directory.RelativePath)
		if err != nil {
			return err
		}
		if err := os.Chmod(path, os.FileMode(directory.Mode)); err != nil {
			return fmt.Errorf("设置副本目录权限失败 %s：%w", directory.RelativePath, err)
		}
		if err := os.Chtimes(path, directory.ModTime, directory.ModTime); err != nil {
			return fmt.Errorf("设置副本目录时间失败 %s：%w", directory.RelativePath, err)
		}
	}
	return nil
}

func copyFile(ctx context.Context, source, target string, expected FileEntry, chunkBytes int64) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("打开源文件前读取失败 %s：%w", expected.RelativePath, err)
	}
	if !info.Mode().IsRegular() || !samePlanMetadata(expected, info) {
		return fmt.Errorf("源文件在复制前已变化：%s", expected.RelativePath)
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("打开源文件失败 %s：%w", expected.RelativePath, err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(expected.Mode))
	if err != nil {
		return fmt.Errorf("创建副本文件失败 %s：%w", expected.RelativePath, err)
	}
	hash := sha256.New()
	buffer := make([]byte, chunkBytes)
	var copied int64
	copyErr := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			read, readErr := in.Read(buffer)
			if read > 0 {
				if _, err := out.Write(buffer[:read]); err != nil {
					return err
				}
				_, _ = hash.Write(buffer[:read])
				copied += int64(read)
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}()
	if copyErr != nil {
		_ = out.Close()
		return fmt.Errorf("复制文件失败 %s：%w", expected.RelativePath, copyErr)
	}
	if copied != expected.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		_ = out.Close()
		return fmt.Errorf("复制文件校验失败：%s", expected.RelativePath)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("同步副本文件失败 %s：%w", expected.RelativePath, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("关闭副本文件失败 %s：%w", expected.RelativePath, err)
	}
	if err := os.Chmod(target, os.FileMode(expected.Mode)); err != nil {
		return fmt.Errorf("设置副本文件权限失败 %s：%w", expected.RelativePath, err)
	}
	if err := os.Chtimes(target, expected.ModTime, expected.ModTime); err != nil {
		return fmt.Errorf("设置副本文件时间失败 %s：%w", expected.RelativePath, err)
	}
	return nil
}

func writeManifest(root string, plan CopyPlan) error {
	value := manifest{
		Version: PlanVersion, Kind: plan.Marker.Kind, AdapterID: plan.Marker.AdapterID,
		SchemaSHA256: plan.Marker.SchemaSHA256, SQLiteSemanticsSHA256: plan.Marker.SQLiteSemanticsSHA256,
		CreatedAt: plan.CreatedAt, SourceRoot: plan.SourceRoot, SourceRootName: filepath.Base(plan.SourceRoot), SourceManifestSHA256: plan.SourceManifestSHA256,
		FileCount: plan.FileCount, TotalBytes: plan.TotalBytes, Files: plan.Files, Directories: plan.Directories, ExcludedPaths: plan.ExcludedPaths,
		ControlPlane: plan.ControlPlane, CompletenessState: plan.CompletenessState, CompletenessReasons: plan.CompletenessReasons,
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化副本 manifest 失败：%w", err)
	}
	encoded = append(encoded, '\n')
	temporary := filepath.Join(root, ManifestName+".tmp")
	path := filepath.Join(root, ManifestName)
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return fmt.Errorf("写入副本 manifest 失败：%w", err)
	}
	file, err := os.OpenFile(temporary, os.O_RDWR, 0)
	if err != nil {
		_ = os.Remove(temporary)
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("发布副本 manifest 失败：%w", err)
	}
	return nil
}

func readManifest(root string) (manifest, CopyPlan, error) {
	path, err := safeJoin(root, ManifestName)
	if err != nil {
		return manifest{}, CopyPlan{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return manifest{}, CopyPlan{}, fmt.Errorf("读取副本 manifest 失败：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return manifest{}, CopyPlan{}, errors.New("副本 manifest 不是普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, CopyPlan{}, err
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil {
		return manifest{}, CopyPlan{}, fmt.Errorf("副本 manifest 不是有效 JSON：%w", err)
	}
	if value.Version != PlanVersion || value.Kind != ManifestKind || value.SourceManifestSHA256 == "" {
		return manifest{}, CopyPlan{}, errors.New("副本 manifest 版本、类型或源摘要不受信任")
	}
	plan := CopyPlan{Version: value.Version, SourceRoot: "", DestinationRoot: root, CreatedAt: value.CreatedAt, Files: value.Files, Directories: value.Directories, ExcludedPaths: value.ExcludedPaths, FileCount: value.FileCount, TotalBytes: value.TotalBytes, SourceManifestSHA256: value.SourceManifestSHA256, Marker: MarkerSpec{Kind: value.Kind, AdapterID: value.AdapterID, SchemaSHA256: value.SchemaSHA256, SQLiteSemanticsSHA256: value.SQLiteSemanticsSHA256}, ControlPlane: value.ControlPlane, CompletenessState: value.CompletenessState, CompletenessReasons: value.CompletenessReasons}
	return value, plan, nil
}

func verifyDestination(ctx context.Context, root string, plan CopyPlan, chunkBytes int64, progressFn ProgressFunc) error {
	entries := make(map[string]FileEntry, len(plan.Files))
	for _, entry := range plan.Files {
		entries[entry.RelativePath] = entry
	}
	seen := make(map[string]struct{}, len(entries))
	expectedDirs := make(map[string]DirectoryEntry, len(plan.Directories))
	seenDirs := make(map[string]struct{}, len(plan.Directories))
	for _, directory := range plan.Directories {
		expectedDirs[directory.RelativePath] = directory
	}
	var filesDone, bytesDone int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		relative := relativePath(path, root)
		if relative == ManifestName || relative == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("副本包含符号链接：%s", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			if _, ok := expectedDirs[relative]; !ok {
				return fmt.Errorf("副本包含计划外目录：%s", relative)
			}
			seenDirs[relative] = struct{}{}
			return nil
		}
		expected, ok := entries[relative]
		if !ok {
			return fmt.Errorf("副本包含计划外文件：%s", relative)
		}
		if !info.Mode().IsRegular() || info.Size() != expected.SizeBytes {
			return fmt.Errorf("副本文件元数据不匹配：%s", relative)
		}
		digest, size, hashErr := hashFile(ctx, path, chunkBytes)
		if hashErr != nil {
			return hashErr
		}
		if size != expected.SizeBytes || digest != expected.SHA256 {
			return fmt.Errorf("副本文件哈希不匹配：%s", relative)
		}
		seen[relative] = struct{}{}
		filesDone++
		bytesDone += size
		progress(progressFn, Progress{Phase: "verify-destination", RelativePath: relative, FilesDone: filesDone, FilesTotal: plan.FileCount, BytesDone: bytesDone, BytesTotal: plan.TotalBytes})
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(entries) || len(seenDirs) != len(expectedDirs) {
		return errors.New("副本文件清单不完整")
	}
	hash, err := sourceManifestHash(plan.Files, plan.Directories, plan.ExcludedPaths)
	if err != nil {
		return err
	}
	if hash != plan.SourceManifestSHA256 {
		return errors.New("副本计划源摘要无法重算")
	}
	return nil
}

func controlPlaneSnapshotsEqual(expected, actual ControlPlaneSnapshot) bool {
	left, leftErr := json.Marshal(expected)
	right, rightErr := json.Marshal(actual)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func probeActivity(ctx context.Context, root string, files []FileEntry, options *activity.Options) (activity.Report, error) {
	targets := make([]activity.Target, 0, len(files))
	for _, file := range files {
		targets = append(targets, activity.Target{RelativePath: file.RelativePath, Kind: activityKind(file.RelativePath)})
	}
	probeOptions := activity.DefaultOptions()
	if options != nil {
		probeOptions = *options
	}
	report, err := activity.Probe(ctx, root, targets, probeOptions)
	if err != nil {
		return report, fmt.Errorf("源目录活动探测失败：%w", err)
	}
	return report, nil
}

func validateActivity(report activity.Report, allowUnavailableProcessProbe bool) error {
	if !report.ProcessProbeAvailable && !allowUnavailableProcessProbe {
		return fmt.Errorf("无法确认源目录是否被进程打开：%s", report.ProcessProbeReason)
	}
	if !report.LockProbeAvailable {
		return fmt.Errorf("无法读取源目录 writer lock：%s", report.LockProbeReason)
	}
	if report.LockFileCount > 0 {
		lockDetail := strings.Join(report.LockIDs, "、")
		if lockDetail == "" {
			lockDetail = "ID 不可用"
		}
		return fmt.Errorf("源目录存在 %d 个 writer lock（%s），拒绝制作稳定副本", report.LockFileCount, lockDetail)
	}
	for _, result := range report.Results {
		if result.Status != domain.StatusStable {
			return fmt.Errorf("源资产 %s 活动状态为 %s：%s", result.RelativePath, result.Status, result.Evidence.Reason)
		}
	}
	return nil
}

func summarizeActivity(report activity.Report) ActivitySummary {
	summary := ActivitySummary{
		SchemaVersion: report.SchemaVersion, Root: report.Root, GeneratedAt: report.GeneratedAt, IntervalMS: report.IntervalMS,
		ProcessProbeAvailable: report.ProcessProbeAvailable, ProcessProbeReason: report.ProcessProbeReason,
		LockProbeAvailable: report.LockProbeAvailable, LockProbeReason: report.LockProbeReason, LockFileCount: report.LockFileCount,
		LockIDs: append([]string(nil), report.LockIDs...),
		Results: make([]activity.AssetResult, 0, 32),
	}
	for _, result := range report.Results {
		if result.Status == domain.StatusStable && !result.MutationBlocked {
			summary.StableCount++
			continue
		}
		summary.BlockedCount++
		if len(summary.Results) < 32 {
			summary.Results = append(summary.Results, result)
		}
	}
	return summary
}

func hashFile(ctx context.Context, path string, chunkBytes int64) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, chunkBytes)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			total += int64(read)
		}
		if readErr == io.EOF {
			return hex.EncodeToString(hash.Sum(nil)), total, nil
		}
		if readErr != nil {
			return "", total, readErr
		}
	}
}

func sourceManifestHash(files []FileEntry, directories []DirectoryEntry, excludedPaths []string) (string, error) {
	value := struct {
		Files         []FileEntry      `json:"files"`
		Directories   []DirectoryEntry `json:"directories"`
		ExcludedPaths []string         `json:"excluded_paths,omitempty"`
	}{Files: append([]FileEntry(nil), files...), Directories: append([]DirectoryEntry(nil), directories...), ExcludedPaths: append([]string(nil), excludedPaths...)}
	sort.Slice(value.Files, func(i, j int) bool { return value.Files[i].RelativePath < value.Files[j].RelativePath })
	sort.Slice(value.Directories, func(i, j int) bool { return value.Directories[i].RelativePath < value.Directories[j].RelativePath })
	sort.Strings(value.ExcludedPaths)
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("序列化源清单失败：%w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validatePlan(plan CopyPlan) error {
	if plan.Version != PlanVersion || strings.TrimSpace(plan.SourceRoot) == "" || strings.TrimSpace(plan.DestinationRoot) == "" || plan.SourceManifestSHA256 == "" {
		return errors.New("稳定副本计划不完整")
	}
	if !strings.EqualFold(filepath.Base(plan.SourceRoot), ".codex") {
		return errors.New("稳定副本计划源目录不是 .codex")
	}
	if pathWithin(plan.SourceRoot, plan.DestinationRoot) || pathWithin(plan.DestinationRoot, plan.SourceRoot) || hasPathComponent(plan.DestinationRoot, ".codex") {
		return errors.New("稳定副本计划源/目标路径不安全")
	}
	if plan.Marker.Kind == "" {
		return errors.New("稳定副本计划缺少 manifest 类型")
	}
	normalizedExcluded, err := normalizeExcludedPaths(plan.ExcludedPaths)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(normalizedExcluded, plan.ExcludedPaths) {
		return errors.New("稳定副本计划排除路径未规范化")
	}
	return nil
}

func planRevalidationActivityOptions(plan CopyPlan) *activity.Options {
	options := activity.DefaultOptions()
	if plan.Activity.IntervalMS > 0 {
		options.Interval = time.Duration(plan.Activity.IntervalMS) * time.Millisecond
	}
	if plan.AllowUnavailableProcessProbe {
		options.Conservative = false
	}
	return &options
}

// rejectSQLiteAuxiliaryFiles 保证严格副本不会在计划生成后悄悄遇到新的 WAL/SHM。
func rejectSQLiteAuxiliaryFiles(root string, files []FileEntry) error {
	for _, file := range files {
		if activityKind(file.RelativePath) != domain.AssetKindSQLite {
			continue
		}
		path, err := safeJoin(root, file.RelativePath)
		if err != nil {
			return err
		}
		auxiliary := sqliteguard.ExistingAuxiliaryFiles(path)
		if len(auxiliary) > 0 {
			return fmt.Errorf("源 SQLite 存在 WAL/SHM，严格稳定模式拒绝声称完整副本：%s", strings.Join(auxiliary, "、"))
		}
	}
	return nil
}

func normalizeExcludedPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	result := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		value := raw
		slash := filepath.ToSlash(value)
		clean := filepath.Clean(filepath.FromSlash(slash))
		normalized := filepath.ToSlash(clean)
		if strings.TrimSpace(value) == "" || filepath.IsAbs(filepath.FromSlash(slash)) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || normalized != slash {
			return nil, fmt.Errorf("稳定副本排除路径不安全：%s", raw)
		}
		if _, exists := seen[normalized]; exists {
			return nil, fmt.Errorf("稳定副本排除路径重复：%s", normalized)
		}
		for _, existing := range result {
			if normalized == existing || strings.HasPrefix(normalized, existing+"/") || strings.HasPrefix(existing, normalized+"/") {
				return nil, fmt.Errorf("稳定副本排除路径相互重叠：%s、%s", existing, normalized)
			}
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)
	return result, nil
}

func containsPath(paths []string, candidate string) bool {
	normalized := filepath.ToSlash(filepath.Clean(filepath.FromSlash(candidate)))
	for _, path := range paths {
		path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
		if normalized == path || strings.HasPrefix(normalized, path+"/") {
			return true
		}
	}
	return false
}

func sortPlan(plan *CopyPlan) {
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].RelativePath < plan.Files[j].RelativePath })
	sort.Slice(plan.Directories, func(i, j int) bool {
		left, right := plan.Directories[i].RelativePath, plan.Directories[j].RelativePath
		leftDepth, rightDepth := strings.Count(left, string(filepath.Separator)), strings.Count(right, string(filepath.Separator))
		if leftDepth == rightDepth {
			return left < right
		}
		return leftDepth < rightDepth
	})
	sort.Strings(plan.AuxiliarySQLiteFiles)
	sort.Strings(plan.ExcludedPaths)
}

// isIgnorableRuntimeEntry 只允许 Codex 自身运行时目录中的 symlink/IPC 特殊项被排除；未知 symlink 仍阻断副本。
func isIgnorableRuntimeEntry(relative string, mode fs.FileMode) bool {
	if mode&(os.ModeSymlink|os.ModeSocket|os.ModeNamedPipe) == 0 {
		return false
	}
	path := filepath.ToSlash(relative)
	for _, prefix := range []string{"app-server-control/", "app-server-daemon/", "ipc/", "packages/", "tmp/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func validateSQLiteAuxiliarySet(files []FileEntry, auxiliaries []string) error {
	known := make(map[string]struct{}, len(files))
	for _, file := range files {
		known[strings.ToLower(filepath.ToSlash(file.RelativePath))] = struct{}{}
	}
	for _, auxiliary := range auxiliaries {
		lower := strings.ToLower(filepath.ToSlash(auxiliary))
		base := strings.TrimSuffix(strings.TrimSuffix(lower, "-wal"), "-shm")
		if base == lower || !isSQLiteBasePath(base) {
			return fmt.Errorf("SQLite WAL/SHM 没有可验证的主库：%s", auxiliary)
		}
		if _, ok := known[base]; !ok {
			return fmt.Errorf("SQLite WAL/SHM 缺少对应主库：%s -> %s", auxiliary, base)
		}
	}
	return nil
}

func isSQLiteBasePath(path string) bool {
	return strings.HasSuffix(path, ".sqlite") || strings.HasSuffix(path, ".sqlite3") || strings.HasSuffix(path, ".db")
}

func createStaging(parent, base string) (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		name := fmt.Sprintf(".%s.codex-chronicle-staging-%d-%d", base, time.Now().UnixNano(), attempt)
		path := filepath.Join(parent, name)
		if err := os.Mkdir(path, 0o700); err == nil {
			return path, nil
		} else if !os.IsExist(err) {
			return "", fmt.Errorf("创建副本 staging 目录失败：%w", err)
		}
	}
	return "", errors.New("创建副本 staging 目录冲突过多")
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		// Windows 和部分网络文件系统不允许打开目录；文件级 Sync 仍已执行。
		return nil
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步目录失败 %s：%w", path, err)
	}
	return nil
}

func safeJoin(root, relative string) (string, error) {
	if filepath.IsAbs(filepath.FromSlash(relative)) {
		return "", fmt.Errorf("相对路径不能是绝对路径：%s", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("相对路径越出根目录：%s", relative)
	}
	path := filepath.Join(root, clean)
	if !pathWithin(root, path) {
		return "", fmt.Errorf("相对路径越出根目录：%s", relative)
	}
	return path, nil
}

func relativePath(path, root string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func hasPathComponent(path, wanted string) bool {
	clean := filepath.Clean(path)
	for _, part := range strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, wanted) {
			return true
		}
	}
	return false
}

func fileIdentity(info os.FileInfo) string {
	if info == nil || info.Sys() == nil {
		return ""
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return ""
	}
	dev, devOK := stableStatField(value, "Dev")
	ino, inoOK := stableStatField(value, "Ino")
	if devOK && inoOK {
		return fmt.Sprintf("%T:%s:%s", info.Sys(), dev, ino)
	}
	return ""
}

func stableStatField(value reflect.Value, name string) (string, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() || !field.CanInterface() {
		return "", false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprintf("%d", field.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return fmt.Sprintf("%d", field.Uint()), true
	default:
		return "", false
	}
}

func sameFileState(before, after os.FileInfo) bool {
	if before == nil || after == nil {
		return false
	}
	return os.SameFile(before, after) && before.Size() == after.Size() && before.Mode().Perm() == after.Mode().Perm() && before.ModTime().Equal(after.ModTime())
}

func samePlanMetadata(expected FileEntry, info os.FileInfo) bool {
	if info == nil || fileIdentity(info) != expected.Identity {
		return false
	}
	return info.Mode().IsRegular() && info.Size() == expected.SizeBytes && info.Mode().Perm() == os.FileMode(expected.Mode) && info.ModTime().Equal(expected.ModTime)
}

func normalizeChunkBytes(value int64) int64 {
	if value <= 0 {
		return DefaultChunkBytes
	}
	if value < 32<<10 {
		return 32 << 10
	}
	if value > 64<<20 {
		return 64 << 20
	}
	return value
}

func normalizeMarker(value MarkerSpec) MarkerSpec {
	if strings.TrimSpace(value.Kind) == "" {
		value.Kind = ManifestKind
	}
	return value
}

func progress(callback ProgressFunc, value Progress) {
	if callback != nil {
		callback(value)
	}
}

func activityKind(relative string) domain.AssetKind {
	lower := strings.ToLower(relative)
	switch {
	case strings.HasSuffix(lower, ".sqlite"), strings.HasSuffix(lower, ".sqlite3"), strings.HasSuffix(lower, ".db"):
		return domain.AssetKindSQLite
	case strings.HasSuffix(lower, ".jsonl"):
		return domain.AssetKindJSONL
	default:
		return domain.AssetKindUnknown
	}
}

func isSQLiteAuxiliary(relative string) bool {
	lower := strings.ToLower(relative)
	return strings.HasSuffix(lower, "-wal") || strings.HasSuffix(lower, "-shm") || strings.HasSuffix(lower, ".sqlite-wal") || strings.HasSuffix(lower, ".sqlite-shm")
}
