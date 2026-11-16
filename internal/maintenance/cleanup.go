// 中文说明：登记、检查和清理本项目生成的临时目录与可重建产物。
// 清理只接受带有明确 manifest 的项目产物；未知对象只报告，真实 .codex 和受保护工作区永远不会进入写路径。
package maintenance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/domain"
)

const (
	cleanupManifestDirectory = "manifests"
	cleanupJournalDirectory  = "journals"
	cleanupPlanDirectory     = "plans"
	cleanupQuarantineDir     = "quarantine"
	cleanupPolicyFile        = "retention.json"
	cleanupManifestVersion   = 1
)

// ArtifactKind 说明项目生成物的来源和默认保留策略。
type ArtifactKind string

const (
	ArtifactSourceCode         ArtifactKind = "source_code"
	ArtifactFormalDocument     ArtifactKind = "formal_document"
	ArtifactFixture            ArtifactKind = "sanitized_fixture"
	ArtifactVerificationReport ArtifactKind = "verification_report"
	ArtifactRebuildable        ArtifactKind = "rebuildable_dependency"
	ArtifactRustTarget         ArtifactKind = "rust_target"
	ArtifactFrontendDependency ArtifactKind = "frontend_dependency"
	ArtifactPythonCache        ArtifactKind = "python_cache"
	ArtifactTemporaryBinary    ArtifactKind = "temporary_binary"
	ArtifactStaging            ArtifactKind = "staging"
	ArtifactFailedArchive      ArtifactKind = "failed_archive"
	ArtifactJournal            ArtifactKind = "journal"
	ArtifactExpiredCopy        ArtifactKind = "expired_copy"
	ArtifactTestDatabase       ArtifactKind = "test_database"
	ArtifactRuntimeCache       ArtifactKind = "runtime_cache"
	ArtifactCrashResidue       ArtifactKind = "crash_residue"
)

// RetentionPolicy 使用秒数而不是 time.Duration，便于稳定写入 JSON 配置。
type RetentionPolicy struct {
	Version                int   `json:"version"`
	DefaultTTLSeconds      int64 `json:"default_ttl_seconds"`
	StagingTTLSeconds      int64 `json:"staging_ttl_seconds"`
	FailedTTLSeconds       int64 `json:"failed_ttl_seconds"`
	ExpiredCopyTTLSeconds  int64 `json:"expired_copy_ttl_seconds"`
	RuntimeCacheTTLSeconds int64 `json:"runtime_cache_ttl_seconds"`
	PurgeTTLSeconds        int64 `json:"purge_ttl_seconds"`
	UnknownReportOnly      bool  `json:"unknown_report_only"`
}

// DefaultRetentionPolicy 是保守默认值；它只决定何时进入候选，不会自动删除文件。
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		Version: 1, DefaultTTLSeconds: int64((7 * 24 * time.Hour).Seconds()),
		StagingTTLSeconds:      int64((24 * time.Hour).Seconds()),
		FailedTTLSeconds:       int64((7 * 24 * time.Hour).Seconds()),
		ExpiredCopyTTLSeconds:  int64((30 * 24 * time.Hour).Seconds()),
		RuntimeCacheTTLSeconds: int64((14 * 24 * time.Hour).Seconds()),
		PurgeTTLSeconds:        int64((7 * 24 * time.Hour).Seconds()),
		UnknownReportOnly:      true,
	}
}

func (policy RetentionPolicy) normalized() RetentionPolicy {
	defaults := DefaultRetentionPolicy()
	if policy.Version == 0 {
		policy.Version = defaults.Version
	}
	if policy.DefaultTTLSeconds <= 0 {
		policy.DefaultTTLSeconds = defaults.DefaultTTLSeconds
	}
	if policy.StagingTTLSeconds <= 0 {
		policy.StagingTTLSeconds = defaults.StagingTTLSeconds
	}
	if policy.FailedTTLSeconds <= 0 {
		policy.FailedTTLSeconds = defaults.FailedTTLSeconds
	}
	if policy.ExpiredCopyTTLSeconds <= 0 {
		policy.ExpiredCopyTTLSeconds = defaults.ExpiredCopyTTLSeconds
	}
	if policy.RuntimeCacheTTLSeconds <= 0 {
		policy.RuntimeCacheTTLSeconds = defaults.RuntimeCacheTTLSeconds
	}
	if policy.PurgeTTLSeconds <= 0 {
		policy.PurgeTTLSeconds = defaults.PurgeTTLSeconds
	}
	return policy
}

func (policy RetentionPolicy) ttl(kind ArtifactKind) time.Duration {
	policy = policy.normalized()
	seconds := policy.DefaultTTLSeconds
	switch kind {
	case ArtifactStaging, ArtifactTemporaryBinary, ArtifactCrashResidue:
		seconds = policy.StagingTTLSeconds
	case ArtifactFailedArchive:
		seconds = policy.FailedTTLSeconds
	case ArtifactExpiredCopy:
		seconds = policy.ExpiredCopyTTLSeconds
	case ArtifactRuntimeCache, ArtifactPythonCache:
		seconds = policy.RuntimeCacheTTLSeconds
	}
	return time.Duration(seconds) * time.Second
}

// ArtifactManifest 是每个项目生成目录的归属、租约和恢复证据。
type ArtifactManifest struct {
	Version               int          `json:"version"`
	ArtifactID            string       `json:"artifact_id"`
	Kind                  ArtifactKind `json:"kind"`
	TargetPath            string       `json:"target_path"`
	CreatedAt             time.Time    `json:"created_at"`
	Owner                 string       `json:"owner"`
	OperationID           string       `json:"operation_id,omitempty"`
	LeaseUntil            time.Time    `json:"lease_until,omitempty"`
	Source                string       `json:"source"`
	InUse                 bool         `json:"in_use"`
	Rebuildable           bool         `json:"rebuildable"`
	ExpiresAt             time.Time    `json:"expires_at"`
	Risk                  string       `json:"risk"`
	RetentionReason       string       `json:"retention_reason"`
	JournalPath           string       `json:"journal_path,omitempty"`
	RemoteArchiveID       string       `json:"remote_archive_id,omitempty"`
	RemoteArchiveVerified bool         `json:"remote_archive_verified"`
	WorkspaceRoot         string       `json:"workspace_root,omitempty"`
	State                 string       `json:"state"`
	LastOperationID       string       `json:"last_operation_id,omitempty"`
	QuarantinePath        string       `json:"quarantine_path,omitempty"`
	QuarantinedAt         time.Time    `json:"quarantined_at,omitempty"`
	PurgeAfter            time.Time    `json:"purge_after,omitempty"`
	PurgedAt              time.Time    `json:"purged_at,omitempty"`
}

// CleanupFile 是清理前后记录的单个文件清单项，不包含正文。
type CleanupFile struct {
	RelativePath string    `json:"relative_path"`
	SizeBytes    int64     `json:"size_bytes"`
	ModTime      time.Time `json:"mod_time"`
	Mode         string    `json:"mode"`
}

// CleanupSnapshot 是清理前后可解释的大小和文件清单。
type CleanupSnapshot struct {
	TargetPath       string        `json:"target_path"`
	Exists           bool          `json:"exists"`
	IsDirectory      bool          `json:"is_directory"`
	SizeBytes        int64         `json:"size_bytes"`
	FileCount        int           `json:"file_count"`
	FileList         []CleanupFile `json:"file_list,omitempty"`
	FileListComplete bool          `json:"file_list_complete"`
}

// CleanupCandidate 是清理 UI 和 API 展示的单个登记产物。
type CleanupCandidate struct {
	ArtifactID            string       `json:"artifact_id"`
	Kind                  ArtifactKind `json:"kind"`
	TargetPath            string       `json:"target_path"`
	ManifestPath          string       `json:"manifest_path"`
	CreatedAt             time.Time    `json:"created_at"`
	Owner                 string       `json:"owner"`
	OperationID           string       `json:"operation_id,omitempty"`
	LastOperationID       string       `json:"last_operation_id,omitempty"`
	LeaseUntil            time.Time    `json:"lease_until,omitempty"`
	Source                string       `json:"source"`
	InUse                 bool         `json:"in_use"`
	InUseEvidence         string       `json:"in_use_evidence,omitempty"`
	ProcessProbeAvailable bool         `json:"process_probe_available"`
	Rebuildable           bool         `json:"rebuildable"`
	ExpiresAt             time.Time    `json:"expires_at"`
	Risk                  string       `json:"risk"`
	RetentionReason       string       `json:"retention_reason"`
	SizeBytes             int64        `json:"size_bytes"`
	FileCount             int          `json:"file_count"`
	Status                string       `json:"status"`
	Eligible              bool         `json:"eligible"`
	BlockReason           string       `json:"block_reason,omitempty"`
	ActiveWorkspace       bool         `json:"active_workspace"`
	RecoveryJournal       bool         `json:"recovery_journal"`
	RemoteArchiveVerified bool         `json:"remote_archive_verified"`
	QuarantinePath        string       `json:"quarantine_path,omitempty"`
	PurgeAfter            time.Time    `json:"purge_after,omitempty"`
}

// UnknownArtifact 记录没有安全归属的对象；未知对象永远不进入执行计划。
type UnknownArtifact struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	Risk      string `json:"risk"`
}

// CleanupReport 是只读清理候选和 crash residue 报告。
type CleanupReport struct {
	ReadOnly       bool               `json:"read_only"`
	RegistryRoot   string             `json:"registry_root"`
	GeneratedAt    time.Time          `json:"generated_at"`
	Policy         RetentionPolicy    `json:"policy"`
	CandidateCount int                `json:"candidate_count"`
	CandidateBytes int64              `json:"candidate_bytes"`
	Candidates     []CleanupCandidate `json:"candidates"`
	Retained       []CleanupCandidate `json:"retained"`
	Quarantined    []CleanupCandidate `json:"quarantined"`
	Unknown        []UnknownArtifact  `json:"unknown"`
	CrashResidues  []UnknownArtifact  `json:"crash_residues"`
	Recoveries     []CleanupRecovery  `json:"recoveries"`
	Errors         []string           `json:"errors,omitempty"`
}

// CleanupRecovery 描述重启后仍未进入终态的清理 journal，供 UI 提供恢复入口。
type CleanupRecovery struct {
	OperationID string       `json:"operation_id"`
	JournalPath string       `json:"journal_path"`
	Phase       CleanupPhase `json:"phase"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	NeedsReview bool         `json:"needs_review"`
	Reason      string       `json:"reason,omitempty"`
}

// CleanupPlanItem 固定清理操作的 manifest 指纹和清理前快照。
type CleanupPlanItem struct {
	ArtifactID     string           `json:"artifact_id"`
	ManifestPath   string           `json:"manifest_path"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Manifest       ArtifactManifest `json:"manifest"`
	Before         CleanupSnapshot  `json:"before"`
}

// CleanupPlan 是需要用户确认的清理计划；真实源和非 fixture 只能生成只读计划。
type CleanupPlan struct {
	Version        int               `json:"version"`
	OperationID    string            `json:"operation_id"`
	CreatedAt      time.Time         `json:"created_at"`
	RegistryRoot   string            `json:"registry_root"`
	Status         string            `json:"status"`
	ReadOnly       bool              `json:"read_only"`
	WriteEnabled   bool              `json:"write_enabled"`
	BlockReason    string            `json:"block_reason,omitempty"`
	Items          []CleanupPlanItem `json:"items"`
	Policy         RetentionPolicy   `json:"policy"`
	Compensation   []string          `json:"compensation"`
	Verification   []string          `json:"verification"`
	ManifestSHA256 string            `json:"manifest_sha256,omitempty"`
}

// CleanupPhase 是清理操作的持久阶段。
type CleanupPhase string

const (
	CleanupPrepared    CleanupPhase = "prepared"
	CleanupApplied     CleanupPhase = "applied"
	CleanupCommitted   CleanupPhase = "committed"
	CleanupRolledBack  CleanupPhase = "rolled_back"
	CleanupNeedsReview CleanupPhase = "needs_review"
)

// CleanupReceipt 记录一个产物移入隔离区前后的状态。
type CleanupReceipt struct {
	ArtifactID     string          `json:"artifact_id"`
	ManifestPath   string          `json:"manifest_path"`
	OriginalPath   string          `json:"original_path"`
	QuarantinePath string          `json:"quarantine_path"`
	Before         CleanupSnapshot `json:"before"`
	After          CleanupSnapshot `json:"after"`
	MovedAt        time.Time       `json:"moved_at"`
}

// CleanupJournal 是清理恢复和审计使用的持久记录。
type CleanupJournal struct {
	Version      int              `json:"version"`
	OperationID  string           `json:"operation_id"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	Phase        CleanupPhase     `json:"phase"`
	Plan         CleanupPlan      `json:"plan"`
	Receipts     []CleanupReceipt `json:"receipts,omitempty"`
	ErrorCode    string           `json:"error_code,omitempty"`
	ErrorMessage string           `json:"error_message,omitempty"`
	RecoveryNote string           `json:"recovery_note,omitempty"`
	Permanent    bool             `json:"permanent"`
}

// CleanupResult 汇总一次清理或恢复操作。
type CleanupResult struct {
	OperationID string           `json:"operation_id"`
	JournalPath string           `json:"journal_path"`
	PlanPath    string           `json:"plan_path,omitempty"`
	Phase       CleanupPhase     `json:"phase"`
	Receipts    []CleanupReceipt `json:"receipts,omitempty"`
	NeedsReview bool             `json:"needs_review"`
}

// CleanupOptions 控制扫描时的活动工作区和保护根目录。
type CleanupOptions struct {
	ProtectedRoots       []string
	ActiveWorkspaceRoots []string
	Now                  time.Time
	ProbeOpenFiles       bool
	// OpenFileProbe 供平台适配器和测试注入打开文件探测；返回 inUse、available 和证据。
	OpenFileProbe func(context.Context, string) (bool, bool, string)
}

// CleanupPlanOptions 控制预览是否处于明确 fixture 写模式。
type CleanupPlanOptions struct {
	SelectedIDs          []string
	FixtureOnly          bool
	ProtectedRoots       []string
	ActiveWorkspaceRoots []string
}

// Registry 管理独立的 manifest、journal、计划和隔离目录。
type Registry struct {
	Root   string
	Policy RetentionPolicy
}

// OpenRegistry 打开清理 registry；不会创建目录或修改任何文件。
func OpenRegistry(root string) (*Registry, error) {
	abs, err := safeCleanupRoot(root)
	if err != nil {
		return nil, err
	}
	policy := DefaultRetentionPolicy()
	data, readErr := os.ReadFile(filepath.Join(abs, cleanupPolicyFile))
	if readErr == nil {
		var stored RetentionPolicy
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil, fmt.Errorf("读取清理 retention 配置失败：%w", err)
		}
		policy = stored.normalized()
	} else if !os.IsNotExist(readErr) {
		return nil, fmt.Errorf("读取清理 retention 配置失败：%w", readErr)
	}
	return &Registry{Root: abs, Policy: policy.normalized()}, nil
}

// SavePolicy 原子保存独立登记区的保留配置；不会更新已有产物或触发清理，启用门禁由调用方校验。
func (registry *Registry) SavePolicy(ctx context.Context, policy RetentionPolicy) error {
	if registry == nil {
		return fmt.Errorf("清理 registry 为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCleanupWriteRoot(registry.Root); err != nil {
		return err
	}
	registry.Policy = policy.normalized()
	return writeJSONAtomic(filepath.Join(registry.Root, cleanupPolicyFile), registry.Policy, false)
}

// RegisterArtifact 登记一个项目生成目录；登记不会修改目标目录。
func (registry *Registry) RegisterArtifact(ctx context.Context, manifest ArtifactManifest) (ArtifactManifest, string, error) {
	if registry == nil {
		return ArtifactManifest{}, "", fmt.Errorf("清理 registry 为空")
	}
	if err := ctx.Err(); err != nil {
		return ArtifactManifest{}, "", err
	}
	if err := validateCleanupWriteRoot(registry.Root); err != nil {
		return ArtifactManifest{}, "", err
	}
	if manifest.Version == 0 {
		manifest.Version = cleanupManifestVersion
	}
	if manifest.Version != cleanupManifestVersion {
		return ArtifactManifest{}, "", fmt.Errorf("不支持的清理 manifest 版本：%d", manifest.Version)
	}
	generatedID := false
	if manifest.ArtifactID == "" {
		id, err := newCleanupID()
		if err != nil {
			return ArtifactManifest{}, "", err
		}
		manifest.ArtifactID = id
		generatedID = true
	}
	if !validCleanupID(manifest.ArtifactID) {
		return ArtifactManifest{}, "", fmt.Errorf("artifact_id 无效：%s", manifest.ArtifactID)
	}
	if manifest.Kind == "" || strings.TrimSpace(manifest.Owner) == "" || strings.TrimSpace(manifest.Source) == "" {
		return ArtifactManifest{}, "", fmt.Errorf("清理 manifest 必须包含 kind、owner 和 source")
	}
	target, err := filepath.Abs(filepath.Clean(manifest.TargetPath))
	if err != nil || strings.TrimSpace(manifest.TargetPath) == "" {
		return ArtifactManifest{}, "", fmt.Errorf("清理目标路径无效")
	}
	if err := validateCleanupTarget(registry.Root, target); err != nil {
		return ArtifactManifest{}, "", err
	}
	manifest.TargetPath = target
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now().UTC()
	}
	if manifest.ExpiresAt.IsZero() {
		manifest.ExpiresAt = manifest.CreatedAt.Add(registry.Policy.ttl(manifest.Kind))
	}
	if manifest.Risk == "" {
		manifest.Risk = defaultArtifactRisk(manifest.Kind)
	}
	if manifest.RetentionReason == "" {
		manifest.RetentionReason = "等待 retention 到期后再进入清理候选"
	}
	if manifest.State == "" {
		manifest.State = "registered"
	}
	manifestPath := filepath.Join(registry.Root, cleanupManifestDirectory, manifest.ArtifactID+".json")
	if err := writeJSONAtomic(manifestPath, manifest, generatedID); err != nil {
		return ArtifactManifest{}, "", err
	}
	return manifest, manifestPath, nil
}

// Discover 扫描 manifest、未知 registry 对象和 crash residue；只读，不清理。
func (registry *Registry) Discover(ctx context.Context, options CleanupOptions) (CleanupReport, error) {
	if registry == nil {
		return CleanupReport{}, fmt.Errorf("清理 registry 为空")
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := CleanupReport{ReadOnly: true, RegistryRoot: registry.Root, GeneratedAt: now, Policy: registry.Policy.normalized(), Candidates: make([]CleanupCandidate, 0), Retained: make([]CleanupCandidate, 0), Quarantined: make([]CleanupCandidate, 0), Unknown: make([]UnknownArtifact, 0), CrashResidues: make([]UnknownArtifact, 0), Recoveries: make([]CleanupRecovery, 0)}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	rootInfo, err := os.Lstat(registry.Root)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return result, fmt.Errorf("清理 registry 根必须是普通目录")
	}
	manifestRoot := filepath.Join(registry.Root, cleanupManifestDirectory)
	entries, readErr := os.ReadDir(manifestRoot)
	if readErr != nil && !os.IsNotExist(readErr) {
		result.Errors = append(result.Errors, fmt.Sprintf("读取 manifest 目录失败：%v", readErr))
	}
	seen := make(map[string]struct{})
	for _, item := range entries {
		if item.IsDir() || filepath.Ext(item.Name()) != ".json" {
			continue
		}
		manifestPath := filepath.Join(manifestRoot, item.Name())
		info, statErr := os.Lstat(manifestPath)
		if statErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("读取 manifest %s 失败：%v", item.Name(), statErr))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			result.Unknown = append(result.Unknown, UnknownArtifact{Path: manifestPath, Kind: "manifest", Reason: "manifest 不是普通文件", Risk: "high"})
			continue
		}
		data, loadErr := os.ReadFile(manifestPath)
		if loadErr != nil {
			result.Unknown = append(result.Unknown, UnknownArtifact{Path: manifestPath, Kind: "manifest", Reason: loadErr.Error(), Risk: "high"})
			continue
		}
		var manifest ArtifactManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			result.Unknown = append(result.Unknown, UnknownArtifact{Path: manifestPath, Kind: "manifest", Reason: "JSON 无法解析：" + err.Error(), Risk: "high"})
			continue
		}
		seen[manifestPath] = struct{}{}
		candidate := registry.inspectManifest(ctx, manifestPath, manifest, options, now)
		if candidate.Status == "candidate" {
			result.Candidates = append(result.Candidates, candidate)
			result.CandidateCount++
			result.CandidateBytes += candidate.SizeBytes
		} else if candidate.Status == "quarantined" {
			result.Quarantined = append(result.Quarantined, candidate)
		} else if candidate.Status != "purged" {
			result.Retained = append(result.Retained, candidate)
		}
	}
	registry.scanUnknownRoot(&result, seen)
	registry.scanCrashResidues(&result)
	registry.scanRecoveries(ctx, &result)
	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].SizeBytes == result.Candidates[j].SizeBytes {
			return result.Candidates[i].ArtifactID < result.Candidates[j].ArtifactID
		}
		return result.Candidates[i].SizeBytes > result.Candidates[j].SizeBytes
	})
	sort.Slice(result.Retained, func(i, j int) bool { return result.Retained[i].TargetPath < result.Retained[j].TargetPath })
	sort.Slice(result.Quarantined, func(i, j int) bool { return result.Quarantined[i].TargetPath < result.Quarantined[j].TargetPath })
	sort.Slice(result.Recoveries, func(i, j int) bool { return result.Recoveries[i].UpdatedAt.After(result.Recoveries[j].UpdatedAt) })
	return result, nil
}

// BuildPlan 只接受 Discover 报告中的 eligible candidate，避免按目录名直接删除。
func (registry *Registry) BuildPlan(ctx context.Context, report CleanupReport, options CleanupPlanOptions) (CleanupPlan, error) {
	operationID, err := newCleanupID()
	if err != nil {
		return CleanupPlan{}, err
	}
	if registry == nil {
		return CleanupPlan{Version: 1, OperationID: operationID, CreatedAt: time.Now().UTC(), Status: "invalid", ReadOnly: true}, fmt.Errorf("清理 registry 为空")
	}
	plan := CleanupPlan{Version: 1, OperationID: operationID, CreatedAt: time.Now().UTC(), RegistryRoot: registry.Root, Status: "invalid", ReadOnly: true, WriteEnabled: options.FixtureOnly, Items: make([]CleanupPlanItem, 0), Policy: registry.Policy.normalized(), Compensation: []string{"隔离目录保留在 registry quarantine 中，恢复前不得清理 journal", "任一移动失败都按逆序恢复，目标冲突转人工复核"}, Verification: []string{"重新读取 manifest 和目标文件清单", "校验目标消失、隔离目录存在和 journal 阶段"}}
	if filepath.Clean(report.RegistryRoot) != filepath.Clean(registry.Root) {
		return plan, fmt.Errorf("清理报告与 registry 根不一致")
	}
	if len(options.SelectedIDs) == 0 {
		return plan, fmt.Errorf("必须明确选择至少一个清理候选")
	}
	byID := make(map[string]CleanupCandidate, len(report.Candidates))
	for _, candidate := range report.Candidates {
		byID[candidate.ArtifactID] = candidate
	}
	seen := make(map[string]struct{}, len(options.SelectedIDs))
	for _, id := range options.SelectedIDs {
		if _, ok := seen[id]; ok {
			return plan, fmt.Errorf("清理候选重复：%s", id)
		}
		seen[id] = struct{}{}
		candidate, ok := byID[id]
		if !ok || !candidate.Eligible || candidate.Status != "candidate" {
			return plan, fmt.Errorf("清理候选不可执行：%s", id)
		}
		manifest, err := loadCleanupManifest(candidate.ManifestPath)
		if err != nil {
			return plan, err
		}
		before, err := snapshotCleanupTarget(ctx, manifest.TargetPath)
		if err != nil {
			return plan, fmt.Errorf("读取清理前快照失败：%w", err)
		}
		manifestHash, err := hashJSON(manifest)
		if err != nil {
			return plan, err
		}
		plan.Items = append(plan.Items, CleanupPlanItem{ArtifactID: id, ManifestPath: candidate.ManifestPath, ManifestSHA256: manifestHash, Manifest: manifest, Before: before})
	}
	sort.Slice(plan.Items, func(i, j int) bool { return plan.Items[i].ArtifactID < plan.Items[j].ArtifactID })
	if !options.FixtureOnly {
		plan.Status = "read_only"
		plan.BlockReason = "清理写入只允许显式 fixture registry；真实 .codex 和受保护工作区只生成预览"
		return plan, nil
	}
	plan.Status = "prepared"
	plan.ManifestSHA256, _ = hashJSON(plan)
	return plan, nil
}

// ExecutePlan 将已确认生成物移入可恢复 quarantine；不会直接删除目标内容。
func (registry *Registry) ExecutePlan(ctx context.Context, plan CleanupPlan) (CleanupResult, error) {
	result := CleanupResult{OperationID: plan.OperationID, Phase: CleanupPrepared, Receipts: make([]CleanupReceipt, 0)}
	if registry == nil {
		return result, fmt.Errorf("清理 registry 为空")
	}
	if plan.Status != "prepared" || !plan.WriteEnabled {
		return result, fmt.Errorf("清理计划只读或状态无效：%s", plan.BlockReason)
	}
	if filepath.Clean(plan.RegistryRoot) != filepath.Clean(registry.Root) {
		return result, fmt.Errorf("清理计划 registry 根不一致")
	}
	if err := validateCleanupWriteRoot(registry.Root); err != nil {
		return result, err
	}
	if err := registry.revalidatePlan(ctx, plan); err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Join(registry.Root, cleanupJournalDirectory), 0o700); err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Join(registry.Root, cleanupPlanDirectory), 0o700); err != nil {
		return result, err
	}
	journalPath := filepath.Join(registry.Root, cleanupJournalDirectory, plan.OperationID+".json")
	planPath := filepath.Join(registry.Root, cleanupPlanDirectory, plan.OperationID+".json")
	if err := writeJSONAtomic(planPath, plan, true); err != nil {
		return result, err
	}
	entry := CleanupJournal{Version: 1, OperationID: plan.OperationID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Phase: CleanupPrepared, Plan: plan, Receipts: make([]CleanupReceipt, 0)}
	if err := writeJSONAtomic(journalPath, entry, true); err != nil {
		return result, err
	}
	result.JournalPath, result.PlanPath = journalPath, planPath
	fail := func(code string, cause error) (CleanupResult, error) {
		entry.ErrorCode, entry.ErrorMessage = code, cause.Error()
		entry.RecoveryNote = "已尝试逆序恢复；目标冲突时保留 quarantine 和 journal 供人工复核"
		rollbackErr := registry.rollbackCleanup(ctx, &entry, journalPath)
		result.Phase, result.Receipts = entry.Phase, append([]CleanupReceipt(nil), entry.Receipts...)
		result.NeedsReview = rollbackErr != nil || entry.Phase == CleanupNeedsReview
		if rollbackErr != nil {
			return result, fmt.Errorf("%s；回滚失败：%w", cause, rollbackErr)
		}
		return result, cause
	}
	for index := range entry.Plan.Items {
		item := &entry.Plan.Items[index]
		if err := ctx.Err(); err != nil {
			return fail("CLEANUP_CANCELLED", err)
		}
		current, err := snapshotCleanupTarget(ctx, item.Manifest.TargetPath)
		if err != nil || !sameCleanupSnapshot(current, item.Before) {
			if err == nil {
				err = fmt.Errorf("清理目标在执行前发生变化：%s", item.Manifest.TargetPath)
			}
			return fail("ARTIFACT_CHANGED", err)
		}
		quarantine := filepath.Join(registry.Root, cleanupQuarantineDir, plan.OperationID, item.ArtifactID)
		if err := ensureNoSymlinkParent(filepath.Dir(quarantine)); err != nil {
			return fail("QUARANTINE_UNSAFE", err)
		}
		if err := os.MkdirAll(filepath.Dir(quarantine), 0o700); err != nil {
			return fail("QUARANTINE_CREATE_FAILED", err)
		}
		if _, err := os.Lstat(quarantine); err == nil {
			return fail("QUARANTINE_EXISTS", fmt.Errorf("隔离目标已存在：%s", quarantine))
		} else if !os.IsNotExist(err) {
			return fail("QUARANTINE_STAT_FAILED", err)
		}
		if err := os.Rename(item.Manifest.TargetPath, quarantine); err != nil {
			return fail("QUARANTINE_MOVE_FAILED", fmt.Errorf("移动生成物到隔离目录失败：%w", err))
		}
		after, err := snapshotCleanupTarget(ctx, quarantine)
		if err != nil {
			return fail("QUARANTINE_VERIFY_FAILED", err)
		}
		receipt := CleanupReceipt{ArtifactID: item.ArtifactID, ManifestPath: item.ManifestPath, OriginalPath: item.Manifest.TargetPath, QuarantinePath: quarantine, Before: item.Before, After: after, MovedAt: time.Now().UTC()}
		entry.Receipts = append(entry.Receipts, receipt)
		item.Manifest.State = "quarantined"
		item.Manifest.LastOperationID = plan.OperationID
		item.Manifest.QuarantinePath = quarantine
		item.Manifest.QuarantinedAt = receipt.MovedAt
		item.Manifest.PurgeAfter = receipt.MovedAt.Add(time.Duration(registry.Policy.PurgeTTLSeconds) * time.Second)
		if err := writeJSONAtomic(item.ManifestPath, item.Manifest, false); err != nil {
			return fail("MANIFEST_UPDATE_FAILED", err)
		}
		entry.UpdatedAt = time.Now().UTC()
		if err := writeJSONAtomic(journalPath, entry, false); err != nil {
			return fail("CLEANUP_JOURNAL_FAILED", err)
		}
	}
	entry.Phase, entry.UpdatedAt = CleanupApplied, time.Now().UTC()
	if err := writeJSONAtomic(journalPath, entry, false); err != nil {
		return fail("CLEANUP_APPLIED_FAILED", err)
	}
	for _, receipt := range entry.Receipts {
		if _, err := os.Lstat(receipt.OriginalPath); !os.IsNotExist(err) {
			return fail("CLEANUP_VERIFY_FAILED", fmt.Errorf("清理后目标仍存在：%s", receipt.OriginalPath))
		}
		if _, err := os.Lstat(receipt.QuarantinePath); err != nil {
			return fail("CLEANUP_VERIFY_FAILED", err)
		}
	}
	entry.Phase, entry.UpdatedAt = CleanupCommitted, time.Now().UTC()
	if err := writeJSONAtomic(journalPath, entry, false); err != nil {
		return fail("CLEANUP_COMMIT_FAILED", err)
	}
	result.Phase, result.Receipts = entry.Phase, entry.Receipts
	return result, nil
}

// RecoverPlan 恢复尚未进入终态的清理 journal；冲突时只返回 needs_review。
func (registry *Registry) RecoverPlan(ctx context.Context, journalPath string) (CleanupResult, error) {
	entry, err := loadCleanupJournal(ctx, journalPath)
	if err != nil {
		return CleanupResult{}, err
	}
	result := CleanupResult{OperationID: entry.OperationID, JournalPath: journalPath, Phase: entry.Phase, Receipts: entry.Receipts}
	if entry.Phase == CleanupCommitted || entry.Phase == CleanupRolledBack {
		return result, nil
	}
	for index := len(entry.Receipts) - 1; index >= 0; index-- {
		if err := restoreCleanupReceipt(ctx, entry.Receipts[index]); err != nil {
			entry.Phase, entry.ErrorCode, entry.ErrorMessage = CleanupNeedsReview, "CLEANUP_RECOVERY_FAILED", err.Error()
			entry.RecoveryNote = "恢复目标或隔离目录存在冲突，禁止覆盖"
			_ = writeJSONAtomic(journalPath, entry, false)
			result.Phase, result.NeedsReview = entry.Phase, true
			return result, err
		}
		manifest, manifestErr := loadCleanupManifest(entry.Receipts[index].ManifestPath)
		if manifestErr != nil {
			entry.Phase, entry.ErrorCode, entry.ErrorMessage = CleanupNeedsReview, "MANIFEST_RECOVERY_FAILED", manifestErr.Error()
			_ = writeJSONAtomic(journalPath, entry, false)
			result.Phase, result.NeedsReview = entry.Phase, true
			return result, manifestErr
		}
		manifest.State, manifest.QuarantinePath, manifest.LastOperationID = "registered", "", entry.OperationID
		manifest.QuarantinedAt, manifest.PurgeAfter = time.Time{}, time.Time{}
		if err := writeJSONAtomic(entry.Receipts[index].ManifestPath, manifest, false); err != nil {
			entry.Phase, entry.ErrorCode, entry.ErrorMessage = CleanupNeedsReview, "MANIFEST_RECOVERY_WRITE_FAILED", err.Error()
			_ = writeJSONAtomic(journalPath, entry, false)
			result.Phase, result.NeedsReview = entry.Phase, true
			return result, err
		}
	}
	entry.Phase, entry.UpdatedAt, entry.RecoveryNote = CleanupRolledBack, time.Now().UTC(), "清理隔离物已恢复，manifest 回到 registered"
	if err := writeJSONAtomic(journalPath, entry, false); err != nil {
		return result, err
	}
	result.Phase = entry.Phase
	return result, nil
}

func (registry *Registry) inspectManifest(ctx context.Context, manifestPath string, manifest ArtifactManifest, options CleanupOptions, now time.Time) CleanupCandidate {
	candidate := CleanupCandidate{ArtifactID: manifest.ArtifactID, Kind: manifest.Kind, TargetPath: manifest.TargetPath, ManifestPath: manifestPath, CreatedAt: manifest.CreatedAt, Owner: manifest.Owner, OperationID: manifest.OperationID, LeaseUntil: manifest.LeaseUntil, Source: manifest.Source, Rebuildable: manifest.Rebuildable, ExpiresAt: manifest.ExpiresAt, Risk: manifest.Risk, RetentionReason: manifest.RetentionReason, RemoteArchiveVerified: manifest.RemoteArchiveVerified, QuarantinePath: manifest.QuarantinePath, PurgeAfter: manifest.PurgeAfter, Status: "retained"}
	if err := validateManifest(manifest); err != nil {
		candidate.Status, candidate.BlockReason = "unknown", err.Error()
		return candidate
	}
	if manifest.State == "quarantined" || manifest.State == "purged" {
		candidate.Status = manifest.State
		candidate.BlockReason = "产物已经进入隔离或终态，不重复加入清理候选"
		candidate.LastOperationID = manifest.LastOperationID
		if manifest.State == "quarantined" {
			expected := filepath.Join(registry.Root, cleanupQuarantineDir, manifest.LastOperationID, manifest.ArtifactID)
			if !validCleanupID(manifest.LastOperationID) || !validCleanupID(manifest.ArtifactID) || filepath.Clean(manifest.QuarantinePath) != expected {
				candidate.Status, candidate.BlockReason = "unknown", "隔离路径与登记记录不一致"
				return candidate
			}
			if err := ensureNoSymlinkParent(filepath.Dir(expected)); err != nil {
				candidate.Status, candidate.BlockReason = "unknown", err.Error()
				return candidate
			}
			snapshot, err := snapshotCleanupTarget(ctx, expected)
			if err != nil || !snapshot.Exists {
				candidate.Status, candidate.BlockReason = "unknown", "隔离文件无法读取或已经不存在"
				return candidate
			}
			candidate.SizeBytes, candidate.FileCount = snapshot.SizeBytes, snapshot.FileCount
			candidate.BlockReason = "文件保留在隔离区，仍占用磁盘；本页只支持恢复中断操作，不自动撤销已完成的隔离"
		}
		return candidate
	}
	if err := validateCleanupTarget(registryRootForManifest(manifestPath), manifest.TargetPath); err != nil {
		candidate.Status, candidate.BlockReason = "unknown", err.Error()
		return candidate
	}
	info, err := os.Lstat(manifest.TargetPath)
	if os.IsNotExist(err) {
		candidate.Status, candidate.BlockReason = "missing", "manifest 目标已经不存在，保留 manifest 供审计"
		return candidate
	}
	if err != nil {
		candidate.Status, candidate.BlockReason = "unknown", "读取目标失败："+err.Error()
		return candidate
	}
	if info.Mode()&os.ModeSymlink != 0 {
		candidate.Status, candidate.BlockReason = "unknown", "清理目标不能是符号链接"
		return candidate
	}
	snapshot, snapshotErr := snapshotCleanupTarget(ctx, manifest.TargetPath)
	if snapshotErr != nil {
		candidate.Status, candidate.BlockReason = "unknown", snapshotErr.Error()
		return candidate
	}
	candidate.SizeBytes, candidate.FileCount = snapshot.SizeBytes, snapshot.FileCount
	// 只按生成物自身路径判断工作区保护；WorkspaceRoot 是归属说明，不应让工作区仍在使用时永久阻断其外置 staging 清理。
	if containsPath(options.ProtectedRoots, manifest.TargetPath) || containsPath(options.ActiveWorkspaceRoots, manifest.TargetPath) {
		candidate.ActiveWorkspace, candidate.BlockReason = true, "目标属于当前或受保护工作区"
		return candidate
	}
	if isProtectedArtifactKind(manifest.Kind) {
		candidate.BlockReason = "源代码、正式文档、脱敏 fixture 和正式验证报告永远保留"
		return candidate
	}
	if manifest.InUse {
		candidate.InUse, candidate.InUseEvidence, candidate.BlockReason = true, "manifest 声明仍在使用", "生成者仍声明该产物在使用"
		return candidate
	}
	if !manifest.LeaseUntil.IsZero() && manifest.LeaseUntil.After(now) {
		candidate.InUse, candidate.InUseEvidence, candidate.BlockReason = true, "lease 未到期", "生成物租约尚未过期"
		return candidate
	}
	if lockPath := findArtifactLock(manifest.TargetPath); lockPath != "" {
		candidate.InUse, candidate.InUseEvidence, candidate.BlockReason = true, lockPath, "目标目录存在活动锁"
		return candidate
	}
	if options.ProbeOpenFiles {
		inUse, available, evidence := false, false, ""
		if options.OpenFileProbe != nil {
			inUse, available, evidence = options.OpenFileProbe(ctx, manifest.TargetPath)
		} else {
			inUse, available, evidence = probeCleanupOpenFile(ctx, manifest.TargetPath)
		}
		candidate.ProcessProbeAvailable, candidate.InUse, candidate.InUseEvidence = available, inUse, evidence
		if inUse {
			candidate.BlockReason = "目标文件仍被进程打开"
			return candidate
		}
		if !available {
			candidate.Status, candidate.BlockReason = "unknown", "当前平台无法确认目标是否被进程打开"
			return candidate
		}
	} else {
		candidate.ProcessProbeAvailable = false
		candidate.Status, candidate.BlockReason = "unknown", "未执行打开文件探测"
		return candidate
	}
	if journalPath := manifestJournalPath(registryRootForManifest(manifestPath), manifest); journalPath != "" {
		active, reason := cleanupJournalActive(journalPath)
		candidate.RecoveryJournal = active
		if active {
			candidate.BlockReason = reason
			return candidate
		}
	}
	if strings.TrimSpace(manifest.RemoteArchiveID) != "" && !manifest.RemoteArchiveVerified {
		candidate.BlockReason = "远端归档存在但尚未验证"
		return candidate
	}
	if !manifest.Rebuildable {
		candidate.BlockReason = "manifest 未声明可重建，默认只保留不清理"
		return candidate
	}
	if manifest.ExpiresAt.IsZero() || manifest.ExpiresAt.After(now) {
		candidate.BlockReason = "retention 尚未到期"
		return candidate
	}
	candidate.Status, candidate.Eligible, candidate.BlockReason = "candidate", true, "已过期、可重建且没有活动或恢复证据"
	return candidate
}

func (registry *Registry) revalidatePlan(ctx context.Context, plan CleanupPlan) error {
	for _, item := range plan.Items {
		manifest, err := loadCleanupManifest(item.ManifestPath)
		if err != nil {
			return err
		}
		digest, err := hashJSON(manifest)
		if err != nil || digest != item.ManifestSHA256 {
			return fmt.Errorf("manifest 已变化：%s", item.ArtifactID)
		}
		current, err := snapshotCleanupTarget(ctx, manifest.TargetPath)
		if err != nil || !sameCleanupSnapshot(current, item.Before) {
			if err == nil {
				err = fmt.Errorf("目标快照已变化：%s", item.ArtifactID)
			}
			return err
		}
	}
	return nil
}

func (registry *Registry) rollbackCleanup(ctx context.Context, entry *CleanupJournal, journalPath string) error {
	var firstErr error
	for index := len(entry.Receipts) - 1; index >= 0; index-- {
		if err := restoreCleanupReceipt(ctx, entry.Receipts[index]); err != nil && firstErr == nil {
			firstErr = err
		}
		if firstErr != nil {
			break
		}
		manifest, err := loadCleanupManifest(entry.Receipts[index].ManifestPath)
		if err != nil && firstErr == nil {
			firstErr = err
			break
		}
		manifest.State, manifest.QuarantinePath, manifest.LastOperationID = "registered", "", entry.OperationID
		manifest.QuarantinedAt, manifest.PurgeAfter = time.Time{}, time.Time{}
		if err := writeJSONAtomic(entry.Receipts[index].ManifestPath, manifest, false); err != nil && firstErr == nil {
			firstErr = err
			break
		}
	}
	if firstErr == nil {
		entry.Phase = CleanupRolledBack
	} else {
		entry.Phase = CleanupNeedsReview
	}
	entry.UpdatedAt = time.Now().UTC()
	_ = writeJSONAtomic(journalPath, *entry, false)
	return firstErr
}

func restoreCleanupReceipt(ctx context.Context, receipt CleanupReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	targetExists := pathExists(receipt.OriginalPath)
	quarantineExists := pathExists(receipt.QuarantinePath)
	switch {
	case targetExists && !quarantineExists:
		current, err := snapshotCleanupTarget(ctx, receipt.OriginalPath)
		if err != nil || !sameCleanupSnapshot(current, receipt.Before) {
			if err == nil {
				err = fmt.Errorf("目标已存在但内容与清理前快照不一致")
			}
			return err
		}
		return nil
	case targetExists && quarantineExists:
		return fmt.Errorf("目标和隔离目录同时存在：%s", receipt.OriginalPath)
	case !targetExists && !quarantineExists:
		return fmt.Errorf("目标和隔离目录都不存在：%s", receipt.OriginalPath)
	}
	if err := ensureNoSymlinkParent(filepath.Dir(receipt.OriginalPath)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(receipt.OriginalPath), 0o700); err != nil {
		return err
	}
	return os.Rename(receipt.QuarantinePath, receipt.OriginalPath)
}

func (registry *Registry) scanUnknownRoot(report *CleanupReport, seen map[string]struct{}) {
	entries, err := os.ReadDir(registry.Root)
	if err != nil {
		if !os.IsNotExist(err) {
			report.Errors = append(report.Errors, fmt.Sprintf("读取 registry 根失败：%v", err))
		}
		return
	}
	known := map[string]bool{cleanupManifestDirectory: true, cleanupJournalDirectory: true, cleanupPlanDirectory: true, cleanupQuarantineDir: true, cleanupPolicyFile: true, cleanupManagementFile: true}
	for _, entry := range entries {
		if known[entry.Name()] {
			continue
		}
		path := filepath.Join(registry.Root, entry.Name())
		if _, ok := seen[path]; ok {
			continue
		}
		size := int64(0)
		if snapshot, err := snapshotCleanupTarget(context.Background(), path); err == nil {
			size = snapshot.SizeBytes
		}
		report.Unknown = append(report.Unknown, UnknownArtifact{Path: path, SizeBytes: size, Kind: "unregistered", Reason: "registry 中没有对应 artifact manifest", Risk: "high"})
	}
}

func (registry *Registry) scanCrashResidues(report *CleanupReport) {
	for _, directory := range []string{cleanupManifestDirectory, cleanupJournalDirectory, cleanupPlanDirectory, cleanupQuarantineDir} {
		root := filepath.Join(registry.Root, directory)
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry == nil {
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			name := strings.ToLower(entry.Name())
			if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".partial") || strings.HasSuffix(name, ".staging") || strings.Contains(name, "crash") {
				info, _ := entry.Info()
				size := int64(0)
				if info != nil {
					size = info.Size()
				}
				report.CrashResidues = append(report.CrashResidues, UnknownArtifact{Path: path, SizeBytes: size, Kind: string(ArtifactCrashResidue), Reason: "临时操作产物可能来自中断或崩溃，只报告不自动删除", Risk: "high"})
			}
			return nil
		})
	}
}

func (registry *Registry) scanRecoveries(ctx context.Context, report *CleanupReport) {
	root := filepath.Join(registry.Root, cleanupJournalDirectory)
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			report.Errors = append(report.Errors, fmt.Sprintf("读取清理 journal 目录失败：%v", err))
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(root, entry.Name())
		journal, loadErr := loadCleanupJournal(ctx, path)
		if loadErr != nil {
			report.CrashResidues = append(report.CrashResidues, UnknownArtifact{Path: path, Kind: string(ArtifactCrashResidue), Reason: "清理 journal 无法解析，只报告并等待人工复核：" + loadErr.Error(), Risk: "high"})
			continue
		}
		if journal.Phase == CleanupCommitted || journal.Phase == CleanupRolledBack {
			continue
		}
		reason := journal.RecoveryNote
		if reason == "" {
			reason = journal.ErrorMessage
		}
		report.Recoveries = append(report.Recoveries, CleanupRecovery{
			OperationID: journal.OperationID, JournalPath: path, Phase: journal.Phase,
			CreatedAt: journal.CreatedAt, UpdatedAt: journal.UpdatedAt,
			NeedsReview: journal.Phase == CleanupNeedsReview, Reason: reason,
		})
	}
}

func snapshotCleanupTarget(ctx context.Context, target string) (CleanupSnapshot, error) {
	snapshot := CleanupSnapshot{TargetPath: target, FileList: make([]CleanupFile, 0), FileListComplete: true}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return snapshot, fmt.Errorf("清理目标不能是符号链接：%s", target)
	}
	snapshot.Exists, snapshot.IsDirectory = true, info.IsDir()
	if !info.IsDir() {
		snapshot.SizeBytes, snapshot.FileCount = info.Size(), 1
		snapshot.FileList = append(snapshot.FileList, CleanupFile{RelativePath: filepath.Base(target), SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Mode: info.Mode().String()})
		return snapshot, nil
	}
	const maxSnapshotFiles = 100000
	err = filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(target, path)
		if err != nil {
			return err
		}
		snapshot.SizeBytes += info.Size()
		snapshot.FileCount++
		if len(snapshot.FileList) < maxSnapshotFiles {
			snapshot.FileList = append(snapshot.FileList, CleanupFile{RelativePath: filepath.ToSlash(relative), SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Mode: info.Mode().String()})
		} else {
			snapshot.FileListComplete = false
		}
		return nil
	})
	if err != nil {
		return snapshot, err
	}
	sort.Slice(snapshot.FileList, func(i, j int) bool { return snapshot.FileList[i].RelativePath < snapshot.FileList[j].RelativePath })
	return snapshot, nil
}

func sameCleanupSnapshot(left, right CleanupSnapshot) bool {
	return left.Exists == right.Exists && left.IsDirectory == right.IsDirectory && left.SizeBytes == right.SizeBytes && left.FileCount == right.FileCount && left.FileListComplete == right.FileListComplete
}

func probeCleanupOpenFile(ctx context.Context, target string) (bool, bool, string) {
	probeTarget := target
	info, err := os.Lstat(target)
	if err != nil {
		return false, false, err.Error()
	}
	if info.IsDir() {
		probeTarget = ""
		_ = filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
			if probeTarget != "" || walkErr != nil || entry == nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			probeTarget = path
			return filepath.SkipDir
		})
		if probeTarget == "" {
			return false, false, "目标目录没有可供进程探测的普通文件"
		}
	}
	parent, err := filepath.Abs(filepath.Dir(probeTarget))
	if err != nil {
		return false, false, err.Error()
	}
	base := filepath.Base(probeTarget)
	report, err := activity.Probe(ctx, parent, []activity.Target{{RelativePath: base, Kind: domain.AssetKindUnknown}}, activity.Options{Interval: 10 * time.Millisecond, HashBytes: 0, Conservative: true})
	if err != nil {
		return false, false, err.Error()
	}
	if len(report.Results) == 0 {
		return false, report.ProcessProbeAvailable, report.ProcessProbeReason
	}
	result := report.Results[0]
	if len(result.Evidence.OpenByProcess) > 0 {
		return true, report.ProcessProbeAvailable, "打开进程数量：" + fmt.Sprint(len(result.Evidence.OpenByProcess))
	}
	return false, report.ProcessProbeAvailable, report.ProcessProbeReason
}

func findArtifactLock(target string) string {
	info, err := os.Lstat(target)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		for _, suffix := range []string{".lock", ".lease"} {
			if pathExists(target + suffix) {
				return target + suffix
			}
		}
		return ""
	}
	var found string
	_ = filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
		if found != "" || walkErr != nil || entry == nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".lease") {
			found = path
		}
		return nil
	})
	return found
}

func manifestJournalPath(registryRoot string, manifest ArtifactManifest) string {
	if strings.TrimSpace(manifest.JournalPath) != "" {
		return manifest.JournalPath
	}
	if manifest.OperationID == "" {
		return ""
	}
	return filepath.Join(registryRoot, cleanupJournalDirectory, manifest.OperationID+".json")
}

func registryRootForManifest(manifestPath string) string {
	return filepath.Dir(filepath.Dir(manifestPath))
}

func cleanupJournalActive(path string) (bool, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, ""
		}
		return true, "恢复 journal 无法读取"
	}
	var state struct {
		Phase        string `json:"phase"`
		CommitStatus string `json:"commit_status"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return true, "恢复 journal 无法解析"
	}
	if state.Phase == string(CleanupCommitted) || state.Phase == string(CleanupRolledBack) || state.CommitStatus == "committed" || state.CommitStatus == "rolled_back" {
		return false, ""
	}
	return true, "存在未完成恢复 journal"
}

func validateManifest(manifest ArtifactManifest) error {
	if manifest.Version != cleanupManifestVersion {
		return fmt.Errorf("manifest 版本不受支持")
	}
	if !validCleanupID(manifest.ArtifactID) {
		return fmt.Errorf("manifest artifact_id 无效")
	}
	if manifest.Kind == "" || strings.TrimSpace(manifest.Owner) == "" || strings.TrimSpace(manifest.Source) == "" {
		return fmt.Errorf("manifest 缺少 kind、owner 或 source")
	}
	if manifest.CreatedAt.IsZero() || strings.TrimSpace(manifest.TargetPath) == "" {
		return fmt.Errorf("manifest 缺少 created_at 或 target_path")
	}
	return nil
}

func isProtectedArtifactKind(kind ArtifactKind) bool {
	switch kind {
	case ArtifactSourceCode, ArtifactFormalDocument, ArtifactFixture, ArtifactVerificationReport, ArtifactJournal:
		return true
	default:
		return false
	}
}

func defaultArtifactRisk(kind ArtifactKind) string {
	if isProtectedArtifactKind(kind) {
		return "block"
	}
	switch kind {
	case ArtifactFailedArchive, ArtifactExpiredCopy, ArtifactCrashResidue:
		return "high"
	case ArtifactStaging, ArtifactTemporaryBinary, ArtifactRuntimeCache:
		return "medium"
	default:
		return "low"
	}
}

func containsPath(roots []string, candidate string) bool {
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		if sameOrWithin(root, candidate) {
			return true
		}
	}
	return false
}

func sameOrWithin(root, candidate string) bool {
	rootAbs, rootErr := filepath.Abs(filepath.Clean(root))
	candidateAbs, candidateErr := filepath.Abs(filepath.Clean(candidate))
	if rootErr != nil || candidateErr != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func safeCleanupRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("cleanup registry 根不能为空")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if pathHasCodexComponent(abs) {
		return "", fmt.Errorf("cleanup registry 不能位于 .codex 路径")
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("cleanup registry 不能是符号链接")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	return abs, nil
}

func validateCleanupWriteRoot(root string) error {
	abs, err := safeCleanupRoot(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	for _, name := range []string{cleanupManifestDirectory, cleanupJournalDirectory, cleanupPlanDirectory, cleanupQuarantineDir} {
		if err := os.MkdirAll(filepath.Join(abs, name), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func validateCleanupTarget(registryRoot, target string) error {
	if pathHasCodexComponent(target) {
		return fmt.Errorf("清理目标不能位于 .codex 路径")
	}
	if sameOrWithin(registryRoot, target) || sameOrWithin(target, registryRoot) {
		return fmt.Errorf("清理目标不能与 registry 根重叠")
	}
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("清理目标不能是符号链接")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return ensureNoSymlinkParent(filepath.Dir(target))
}

func ensureNoSymlinkParent(path string) error {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	current := abs
	missing := make([]string, 0)
	for {
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			missing = append(missing, current)
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
			continue
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("清理路径父目录不安全：%s", path)
		}
		break
	}
	return nil
}

func loadCleanupManifest(path string) (ArtifactManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ArtifactManifest{}, err
	}
	var manifest ArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ArtifactManifest{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return ArtifactManifest{}, err
	}
	return manifest, nil
}

func loadCleanupJournal(ctx context.Context, path string) (CleanupJournal, error) {
	if err := ctx.Err(); err != nil {
		return CleanupJournal{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CleanupJournal{}, err
	}
	var entry CleanupJournal
	if err := json.Unmarshal(data, &entry); err != nil {
		return CleanupJournal{}, err
	}
	if entry.Version != 1 || !validCleanupID(entry.OperationID) || entry.Plan.OperationID != entry.OperationID {
		return CleanupJournal{}, fmt.Errorf("清理 journal 身份无效")
	}
	return entry, nil
}

func hashJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func newCleanupID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "cleanup-" + hex.EncodeToString(raw[:]), nil
}

func validCleanupID(value string) bool {
	return len(value) == len("cleanup-")+32 && strings.HasPrefix(value, "cleanup-") && !strings.ContainsAny(value, `/\\.`)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
