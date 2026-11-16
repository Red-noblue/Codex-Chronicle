// 中文说明：提供维护候选、JSONL 末行修复和临时文件隔离的可恢复 fixture saga。
// 真实 .codex 只允许读取和生成计划；写入必须由调用方显式启用 fixture 模式。
package maintenance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/platform"
)

// Action 表示维护计划对候选资产执行的动作。
type Action string

const (
	ActionIsolate         Action = "isolate"
	ActionRepairJSONLTail Action = "repair_jsonl_tail"
)

// PlanStatus 表示维护计划是否可以进入执行阶段。
type PlanStatus string

const (
	PlanPrepared PlanStatus = "prepared"
	PlanInvalid  PlanStatus = "invalid"
	PlanReadOnly PlanStatus = "read_only"
)

// Candidate 是维护报告中可供人工审核的单个文件；Eligible 为 true 才允许生成写计划。
type Candidate struct {
	RelativePath string             `json:"relative_path"`
	Category     string             `json:"category"`
	Action       Action             `json:"action"`
	Reason       string             `json:"reason"`
	SizeBytes    int64              `json:"size_bytes"`
	ModTime      time.Time          `json:"mod_time"`
	Status       domain.AssetStatus `json:"status"`
	Risk         string             `json:"risk"`
	Recoverable  bool               `json:"recoverable"`
	Eligible     bool               `json:"eligible"`
	BlockReason  string             `json:"block_reason,omitempty"`
}

// Report 是维护和修复的只读候选清单，不代表任何文件已经被修改。
type Report struct {
	ReadOnly              bool               `json:"read_only"`
	Root                  string             `json:"root"`
	GeneratedAt           time.Time          `json:"generated_at"`
	SourceFileCount       int                `json:"source_file_count"`
	SourceBytes           int64              `json:"source_total_bytes"`
	CandidateCount        int                `json:"candidate_count"`
	CandidateBytes        int64              `json:"candidate_bytes"`
	EligibleCount         int                `json:"eligible_count"`
	BlockedCount          int                `json:"blocked_count"`
	FilteredCount         int                `json:"filtered_count"`
	FilteredBytes         int64              `json:"filtered_bytes"`
	Categories            []CategorySummary  `json:"categories"`
	Directories           []DirectorySummary `json:"directories"`
	UngroupedCount        int                `json:"ungrouped_count"`
	UngroupedBlockedCount int                `json:"ungrouped_blocked_count"`
	CandidateOffset       int                `json:"candidate_offset"`
	CandidateLimit        int                `json:"candidate_limit"`
	HasMore               bool               `json:"has_more"`
	Candidates            []Candidate        `json:"candidates"`
}

// CategorySummary 统计完整快照中的候选分类，不受筛选和分页影响，也不表示可释放空间。
type CategorySummary struct {
	Category  string `json:"category"`
	Count     int    `json:"count"`
	SizeBytes int64  `json:"size_bytes"`
}

// ReportOptions 在完整候选中筛选、排序后分页；Eligibility 只代表可进入预览。
type ReportOptions struct {
	Offset, Limit                      int
	Query, Category, Eligibility, Sort string
	Directory                          string
	Scope                              string
}

// PlanAsset 是计划绑定的文件指纹和维护动作。
type PlanAsset struct {
	RelativePath string             `json:"relative_path"`
	Kind         domain.AssetKind   `json:"kind"`
	Action       Action             `json:"action"`
	Category     string             `json:"category"`
	Reason       string             `json:"reason"`
	SizeBytes    int64              `json:"size_bytes"`
	ModTime      time.Time          `json:"mod_time"`
	SHA256       string             `json:"sha256"`
	Status       domain.AssetStatus `json:"status"`
}

// Plan 是可审阅、可重新校验并可补偿的维护 manifest。
type Plan struct {
	Version        int         `json:"version"`
	OperationID    string      `json:"operation_id"`
	CreatedAt      time.Time   `json:"created_at"`
	Root           string      `json:"root"`
	Status         PlanStatus  `json:"status"`
	ReadOnly       bool        `json:"read_only"`
	WriteEnabled   bool        `json:"write_enabled"`
	BlockReason    string      `json:"block_reason,omitempty"`
	Assets         []PlanAsset `json:"assets"`
	Compensation   []string    `json:"compensation"`
	Verification   []string    `json:"verification"`
	ManifestSHA256 string      `json:"manifest_sha256,omitempty"`
}

// PlanOptions 控制计划选择范围和是否允许 fixture 写入。
type PlanOptions struct {
	SelectedPaths []string
	FixtureOnly   bool
}

// ExecuteOptions 是执行维护 saga 所需的外部目录和回收站 provider。
type ExecuteOptions struct {
	JournalRoot string
	TrashRoot   string
	Trash       platform.TrashProvider
	// RegisterArtifact 登记维护操作目录；nil 时保持现有只读/无 registry 行为。
	RegisterArtifact func(context.Context, ArtifactManifest) (ArtifactManifest, error)
}

// RecoverOptions 控制中断维护操作的补偿 provider 和生成物登记。
type RecoverOptions struct {
	Trash            platform.TrashProvider
	RegisterArtifact func(context.Context, ArtifactManifest) (ArtifactManifest, error)
}

// BackupRecord 记录修复前副本和修复后的源指纹，供恢复流程做冲突判断。
type BackupRecord struct {
	RelativePath      string `json:"relative_path"`
	BackupPath        string `json:"backup_path"`
	OriginalSHA256    string `json:"original_sha256"`
	RepairedSHA256    string `json:"repaired_sha256"`
	OriginalSizeBytes int64  `json:"original_size_bytes"`
}

// JournalPhase 表示维护 saga 的持久化阶段。
type JournalPhase string

const (
	PhasePrepared    JournalPhase = "prepared"
	PhaseApplied     JournalPhase = "applied"
	PhaseCommitted   JournalPhase = "committed"
	PhaseRolledBack  JournalPhase = "rolled_back"
	PhaseNeedsReview JournalPhase = "needs_review"
)

type journal struct {
	Version       int                     `json:"version"`
	OperationID   string                  `json:"operation_id"`
	ArtifactID    string                  `json:"artifact_id,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	Phase         JournalPhase            `json:"phase"`
	Plan          Plan                    `json:"plan"`
	Backups       []BackupRecord          `json:"backups,omitempty"`
	TrashReceipts []platform.TrashReceipt `json:"trash_receipts,omitempty"`
	ErrorCode     string                  `json:"error_code,omitempty"`
	ErrorMessage  string                  `json:"error_message,omitempty"`
	RecoveryNote  string                  `json:"recovery_note,omitempty"`
}

// Result 汇总一次维护执行或恢复的结果。
type Result struct {
	OperationID   string                  `json:"operation_id"`
	JournalPath   string                  `json:"journal_path"`
	ManifestPath  string                  `json:"manifest_path"`
	Phase         JournalPhase            `json:"phase"`
	Backups       []BackupRecord          `json:"backups,omitempty"`
	TrashReceipts []platform.TrashReceipt `json:"trash_receipts,omitempty"`
	NeedsReview   bool                    `json:"needs_review"`
}

// BuildReport 从只读 inventory 生成候选，不读取正文之外的额外数据，也不执行清理。
func BuildReport(report domain.InventoryReport) Report {
	return BuildReportWithOptions(report, ReportOptions{})
}

// BuildReportPage 从只读 inventory 生成维护候选的一页，并保留全量统计信息。
func BuildReportPage(report domain.InventoryReport, offset, limit int) Report {
	return BuildReportWithOptions(report, ReportOptions{Offset: offset, Limit: limit})
}

func buildCandidates(report domain.InventoryReport) []Candidate {
	candidates := make([]Candidate, 0)
	for _, file := range report.Files {
		candidate, ok := candidateForFile(file)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// BuildReportWithOptions 保留全量分类和来源统计，列表的筛选、排序发生在分页之前。
func BuildReportWithOptions(report domain.InventoryReport, options ReportOptions) Report {
	candidates := buildCandidates(report)
	offset, limit := max(0, options.Offset), max(0, options.Limit)
	result := Report{ReadOnly: true, Root: report.Root, GeneratedAt: report.GeneratedAt, SourceFileCount: len(report.Files), CandidateCount: len(candidates), CandidateOffset: offset, CandidateLimit: limit, Candidates: make([]Candidate, 0), Categories: make([]CategorySummary, 0), Directories: BuildDirectories(report)}
	for _, file := range report.Files {
		result.SourceBytes += file.SizeBytes
	}
	categories := make(map[string]CategorySummary)
	filtered := make([]Candidate, 0)
	query := strings.ToLower(strings.TrimSpace(options.Query))
	for _, candidate := range candidates {
		ungrouped := directoryForFile(candidate.RelativePath) == ""
		if ungrouped {
			result.UngroupedCount++
			if !candidate.Eligible {
				result.UngroupedBlockedCount++
			}
		}
		result.CandidateBytes += candidate.SizeBytes
		if candidate.Eligible {
			result.EligibleCount++
		}
		category := categories[candidate.Category]
		category.Category, category.Count, category.SizeBytes = candidate.Category, category.Count+1, category.SizeBytes+candidate.SizeBytes
		categories[candidate.Category] = category
		if options.Scope == "ungrouped" && !ungrouped {
			continue
		}
		if options.Directory != "" && !strings.HasPrefix(candidate.RelativePath, options.Directory+"/") {
			continue
		}
		if options.Category != "" && candidate.Category != options.Category {
			continue
		}
		if options.Eligibility == "eligible" && !candidate.Eligible || options.Eligibility == "blocked" && candidate.Eligible {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(candidate.RelativePath), query) {
			continue
		}
		filtered = append(filtered, candidate)
		result.FilteredBytes += candidate.SizeBytes
	}
	result.BlockedCount = result.CandidateCount - result.EligibleCount
	for _, category := range categories {
		result.Categories = append(result.Categories, category)
	}
	sort.Slice(result.Categories, func(i, j int) bool { return result.Categories[i].Category < result.Categories[j].Category })
	sort.Slice(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		switch options.Sort {
		case "path":
			return a.RelativePath < b.RelativePath
		case "newest":
			if !a.ModTime.Equal(b.ModTime) {
				return a.ModTime.After(b.ModTime)
			}
		case "oldest":
			if !a.ModTime.Equal(b.ModTime) {
				return a.ModTime.Before(b.ModTime)
			}
		default:
			if a.SizeBytes != b.SizeBytes {
				return a.SizeBytes > b.SizeBytes
			}
		}
		return a.RelativePath < b.RelativePath
	})
	result.FilteredCount = len(filtered)
	if offset >= len(filtered) {
		return result
	}
	end := offset + limit
	if limit <= 0 || end > len(filtered) {
		end = len(filtered)
	}
	result.Candidates = filtered[offset:end]
	result.HasMore = end < len(filtered)
	return result
}

func candidateForFile(file domain.FileInventory) (Candidate, bool) {
	path := filepath.ToSlash(file.RelativePath)
	parts := strings.Split(strings.ToLower(path), "/")
	base := parts[len(parts)-1]
	// 目录名只提供候选来源，不能让其下的数据库、凭据、锁或真实会话绕过保护。
	protected := protectedMaintenanceFile(file, base)
	category, reason, risk, ok := maintenancePathCategory(parts, base)
	if protected != "" && ok {
		return Candidate{RelativePath: path, Category: category, Action: ActionIsolate,
			SizeBytes: file.SizeBytes, ModTime: file.ModTime, Status: file.Status, Risk: "high", Recoverable: true,
			Reason: protected, BlockReason: protected}, true
	}
	if file.JSONL != nil && file.JSONL.PendingLines == 1 && file.JSONL.InvalidJSONLines == 1 && file.JSONL.InvalidUTF8Lines == 0 {
		candidate := Candidate{
			RelativePath: path, Category: "repair_jsonl_tail", Action: ActionRepairJSONLTail,
			Reason:    "文件末尾存在唯一未完成 JSONL 记录，可截断到最后一条完整记录",
			SizeBytes: file.SizeBytes, ModTime: file.ModTime, Status: file.Status,
			Risk: "high", Recoverable: true, Eligible: file.Status == domain.StatusStable,
		}
		if !candidate.Eligible {
			candidate.BlockReason = "文件状态显示正在变化或存在冲突，必须先完成活动探测"
		}
		return candidate, true
	}
	if !ok {
		if file.Rollout == nil || !file.Rollout.Orphan {
			return Candidate{}, false
		}
		return Candidate{
			RelativePath: path, Category: "orphan", Action: ActionIsolate,
			Reason:    "rollout 未能与可观测线程关系关联，不能自动判断是否可清理",
			SizeBytes: file.SizeBytes, ModTime: file.ModTime, Status: file.Status,
			Risk: "high", Recoverable: true, Eligible: false,
			BlockReason: "孤儿 rollout 可能是独立有效对话，当前只允许人工导出和诊断",
		}, true
	}
	candidate := Candidate{
		RelativePath: path, Category: category, Action: ActionIsolate,
		Reason: reason, SizeBytes: file.SizeBytes, ModTime: file.ModTime,
		Status: file.Status, Risk: risk, Recoverable: true,
		Eligible: file.Status == domain.StatusStable && category != "sqlite_auxiliary",
	}
	if !candidate.Eligible {
		if category == "sqlite_auxiliary" {
			candidate.BlockReason = "WAL/SHM 可能仍被 SQLite 使用，必须先关闭 writer 并由兼容 adapter 确认"
		} else {
			candidate.BlockReason = "文件状态不是 stable，禁止维护写入"
		}
	}
	return candidate, true
}

func maintenancePathCategory(parts []string, base string) (category, reason, risk string, ok bool) {
	if strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm") {
		return "sqlite_auxiliary", "SQLite WAL/SHM 辅助文件需要先确认是否仍在使用", "high", true
	}
	for _, part := range parts[:len(parts)-1] {
		switch part {
		case "tmp", ".tmp", "temp", ".temp":
			return "temporary", "路径包含临时目录名称", "high", true
		case "cache", "caches", ".cache":
			return "cache", "路径包含缓存目录名称", "medium", true
		case "logs", "log":
			return "log", "路径包含日志目录名称", "medium", true
		}
	}
	switch {
	case strings.HasSuffix(base, ".tmp") || strings.HasSuffix(base, ".temp"):
		return "temporary", "文件扩展名表明可能是临时文件", "high", true
	case strings.HasSuffix(base, ".log"):
		return "log", "文件扩展名表明可能是日志", "medium", true
	case strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm"):
		return "sqlite_auxiliary", "SQLite WAL/SHM 辅助文件需要先确认是否仍在使用", "high", true
	default:
		return "", "", "", false
	}
}

func protectedMaintenanceFile(file domain.FileInventory, base string) string {
	for _, component := range strings.Split(strings.ToLower(filepath.ToSlash(file.RelativePath)), "/") {
		if component == ".git" {
			return "Git 内部数据用于版本历史和恢复，保持原位，不作为运行日志清理"
		}
	}
	if file.Kind == domain.AssetKindSQLite || strings.HasSuffix(base, ".sqlite") || strings.HasSuffix(base, ".sqlite3") || strings.HasSuffix(base, ".db") || strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm") {
		return "数据库、WAL/SHM 文件保持原位，不通过目录清理处理"
	}
	if file.Rollout != nil || file.Kind == domain.AssetKindJSONL || base == "rollout.jsonl" || strings.HasPrefix(base, "rollout-") || base == "history.jsonl" || base == "session_index.jsonl" || base == "session-index.jsonl" {
		return "会话正文、输入历史和名称索引不属于目录清理范围"
	}
	if base == "auth.json" || base == "credentials.json" || base == "config.toml" || strings.HasSuffix(base, ".lock") || base == "lock" {
		return "凭据、Codex 配置和锁文件保持原位"
	}
	return ""
}

// BuildPlan 根据用户明确选择的候选生成带指纹维护计划；未开启 fixture 时只返回只读计划。
func BuildPlan(ctx context.Context, root string, report domain.InventoryReport, options PlanOptions) (Plan, error) {
	operationID, err := newOperationID()
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{
		Version: 1, OperationID: operationID, CreatedAt: time.Now().UTC(), Root: root,
		Status: PlanInvalid, ReadOnly: true, WriteEnabled: options.FixtureOnly,
		Assets: make([]PlanAsset, 0), Compensation: []string{
			"修复前保留独立副本；隔离文件保留回收站回执",
			"任一阶段失败都停止后续动作并尝试逆序恢复",
		}, Verification: []string{
			"重新扫描候选文件并校验 JSONL framing",
			"校验源文件指纹、回收站回执和 journal 阶段",
		},
	}
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return plan, fmt.Errorf("维护计划根目录必须是绝对路径")
	}
	if filepath.Clean(root) != filepath.Clean(report.Root) {
		return plan, fmt.Errorf("维护计划根目录与 inventory 不一致")
	}
	if len(options.SelectedPaths) == 0 {
		return plan, fmt.Errorf("必须明确选择至少一个维护候选")
	}
	candidates := BuildReport(report)
	byPath := make(map[string]Candidate, len(candidates.Candidates))
	for _, candidate := range candidates.Candidates {
		byPath[candidate.RelativePath] = candidate
	}
	seen := make(map[string]struct{}, len(options.SelectedPaths))
	for _, rawPath := range options.SelectedPaths {
		relative := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(rawPath))))
		if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") || strings.Contains(relative, "\x00") {
			return plan, fmt.Errorf("维护候选路径不安全：%q", rawPath)
		}
		if _, exists := seen[relative]; exists {
			return plan, fmt.Errorf("维护候选路径重复：%s", relative)
		}
		seen[relative] = struct{}{}
		candidate, ok := byPath[relative]
		if !ok {
			return plan, fmt.Errorf("路径不是当前维护候选：%s", relative)
		}
		if !candidate.Eligible {
			return plan, fmt.Errorf("候选 %s 被阻断：%s", relative, candidate.BlockReason)
		}
		path, resolveErr := safePath(root, relative)
		if resolveErr != nil {
			return plan, resolveErr
		}
		fingerprint, fpErr := fingerprint(ctx, path)
		if fpErr != nil {
			return plan, fmt.Errorf("读取候选 %s 指纹失败：%w", relative, fpErr)
		}
		if fingerprint.SizeBytes != candidate.SizeBytes || !fingerprint.ModTime.Equal(candidate.ModTime.UTC()) {
			return plan, fmt.Errorf("候选 %s 在生成计划前已变化", relative)
		}
		plan.Assets = append(plan.Assets, PlanAsset{
			RelativePath: relative, Kind: fileKind(report.Files, relative), Action: candidate.Action,
			Category: candidate.Category, Reason: candidate.Reason, SizeBytes: fingerprint.SizeBytes,
			ModTime: fingerprint.ModTime, SHA256: fingerprint.SHA256, Status: candidate.Status,
		})
	}
	if len(plan.Assets) == 0 {
		return plan, fmt.Errorf("维护计划没有资产")
	}
	sort.Slice(plan.Assets, func(i, j int) bool { return plan.Assets[i].RelativePath < plan.Assets[j].RelativePath })
	if !options.FixtureOnly {
		plan.Status = PlanReadOnly
		plan.BlockReason = "真实 .codex 和未显式标记的源目录只允许生成计划，禁止执行维护写入"
		return plan, nil
	}
	plan.Status = PlanPrepared
	return plan, nil
}

// RevalidatePlan 在执行或恢复前重新比较所有源文件指纹。
func RevalidatePlan(ctx context.Context, plan Plan) error {
	if plan.Status != PlanPrepared {
		return fmt.Errorf("维护计划状态 %s 不可执行", plan.Status)
	}
	for _, asset := range plan.Assets {
		path, err := safePath(plan.Root, asset.RelativePath)
		if err != nil {
			return err
		}
		actual, err := fingerprint(ctx, path)
		if err != nil {
			return fmt.Errorf("重新读取 %s 指纹失败：%w", asset.RelativePath, err)
		}
		if actual.SizeBytes != asset.SizeBytes || !actual.ModTime.Equal(asset.ModTime) || actual.SHA256 != asset.SHA256 {
			return fmt.Errorf("候选 %s 指纹已变化，计划失效", asset.RelativePath)
		}
	}
	return nil
}

// Execute 执行 fixture 维护 saga；源目录出现 .codex 路径时始终拒绝。
func Execute(ctx context.Context, plan Plan, options ExecuteOptions) (Result, error) {
	result := Result{OperationID: plan.OperationID, Phase: PhasePrepared, Backups: make([]BackupRecord, 0), TrashReceipts: make([]platform.TrashReceipt, 0)}
	if plan.Status != PlanPrepared || !plan.WriteEnabled {
		return result, fmt.Errorf("维护计划只读或状态无效：%s", plan.BlockReason)
	}
	if pathHasCodexComponent(plan.Root) {
		return result, fmt.Errorf("维护 fixture 写模式拒绝真实 .codex 根目录")
	}
	if err := validateDestination(plan.Root, options.JournalRoot); err != nil {
		return result, err
	}
	if err := RevalidatePlan(ctx, plan); err != nil {
		return result, err
	}
	trash := options.Trash
	if trash == nil {
		if strings.TrimSpace(options.TrashRoot) == "" {
			for _, asset := range plan.Assets {
				if asset.Action == ActionIsolate {
					return result, fmt.Errorf("隔离动作必须提供回收站 provider 或 TrashRoot")
				}
			}
		} else {
			var err error
			if err = validateDestination(plan.Root, options.TrashRoot); err != nil {
				return result, err
			}
			trash, err = platform.NewDirectoryTrash(options.TrashRoot)
			if err != nil {
				return result, err
			}
		}
	}
	operationRoot := filepath.Join(options.JournalRoot, plan.OperationID)
	if err := os.MkdirAll(filepath.Join(operationRoot, "backups"), 0o700); err != nil {
		return result, fmt.Errorf("创建维护操作目录失败：%w", err)
	}
	manifestPath := filepath.Join(options.JournalRoot, plan.OperationID+".manifest.json")
	journalPath := filepath.Join(options.JournalRoot, plan.OperationID+".journal.json")
	var operationManifest ArtifactManifest
	if options.RegisterArtifact != nil {
		registered, registerErr := options.RegisterArtifact(ctx, ArtifactManifest{
			Kind: ArtifactStaging, TargetPath: operationRoot, CreatedAt: time.Now().UTC(), Owner: "maintenance",
			OperationID: plan.OperationID, LeaseUntil: time.Now().UTC().Add(24 * time.Hour), Source: "维护操作 backup/staging",
			Rebuildable: true, Risk: "high", RetentionReason: "等待维护操作完成或异常恢复后再判断", JournalPath: journalPath, WorkspaceRoot: plan.Root,
		})
		if registerErr != nil {
			_ = os.RemoveAll(operationRoot)
			return result, fmt.Errorf("登记维护操作目录失败：%w", registerErr)
		}
		operationManifest = registered
	}
	plan.ManifestSHA256, _ = jsonHash(plan)
	entry := journal{Version: 1, OperationID: plan.OperationID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Phase: PhasePrepared, Plan: plan, Backups: make([]BackupRecord, 0), TrashReceipts: make([]platform.TrashReceipt, 0), ArtifactID: operationManifest.ArtifactID}
	if err := writeJSONAtomic(manifestPath, plan, false); err != nil {
		return result, err
	}
	if err := writeJSONAtomic(journalPath, entry, true); err != nil {
		return result, err
	}
	result.JournalPath, result.ManifestPath = journalPath, manifestPath
	fail := func(code string, cause error) (Result, error) {
		entry.ErrorCode, entry.ErrorMessage = code, cause.Error()
		entry.RecoveryNote = "已尝试按逆序恢复；若存在冲突，保留 backup 和回收站回执供人工复核"
		rollbackErr := rollback(ctx, &entry, journalPath, trash)
		if options.RegisterArtifact != nil && operationManifest.ArtifactID != "" {
			operationManifest.Kind = ArtifactFailedArchive
			operationManifest.State = "failed"
			operationManifest.LeaseUntil = time.Time{}
			operationManifest.ExpiresAt = time.Time{}
			operationManifest.Rebuildable = true
			operationManifest.Risk = "high"
			operationManifest.RetentionReason = "维护操作失败；保留 backup 供审计，等待 failed retention 到期"
			if _, registerErr := options.RegisterArtifact(context.Background(), operationManifest); registerErr != nil && rollbackErr == nil {
				rollbackErr = fmt.Errorf("登记失败维护操作目录失败：%w", registerErr)
			}
		}
		result.Backups, result.TrashReceipts = append([]BackupRecord(nil), entry.Backups...), append([]platform.TrashReceipt(nil), entry.TrashReceipts...)
		result.Phase = entry.Phase
		result.NeedsReview = rollbackErr != nil || entry.Phase == PhaseNeedsReview
		if rollbackErr != nil {
			return result, fmt.Errorf("%s；回滚失败：%w", cause, rollbackErr)
		}
		return result, fmt.Errorf("%s", cause)
	}
	for _, asset := range plan.Assets {
		if err := ctx.Err(); err != nil {
			return fail("MAINTENANCE_CANCELLED", err)
		}
		if currentErr := RevalidateAsset(ctx, plan, asset); currentErr != nil {
			return fail("ASSET_CHANGED", currentErr)
		}
		switch asset.Action {
		case ActionIsolate:
			if trash == nil {
				return fail("TRASH_PROVIDER_MISSING", fmt.Errorf("候选 %s 未配置回收站 provider", asset.RelativePath))
			}
			receipt, moveErr := trash.Move(ctx, plan.OperationID, plan.Root, asset.RelativePath)
			if moveErr != nil {
				return fail("ISOLATE_FAILED", moveErr)
			}
			entry.TrashReceipts = append(entry.TrashReceipts, receipt)
		case ActionRepairJSONLTail:
			backup, repairErr := repairTail(ctx, plan, asset, filepath.Join(operationRoot, "backups"))
			if repairErr != nil {
				return fail("REPAIR_FAILED", repairErr)
			}
			entry.Backups = append(entry.Backups, backup)
		default:
			return fail("UNSUPPORTED_ACTION", fmt.Errorf("不支持维护动作 %s", asset.Action))
		}
		entry.UpdatedAt = time.Now().UTC()
		if err := writeJSONAtomic(journalPath, entry, false); err != nil {
			return fail("JOURNAL_WRITE_FAILED", err)
		}
	}
	entry.Phase = PhaseApplied
	entry.UpdatedAt = time.Now().UTC()
	if err := writeJSONAtomic(journalPath, entry, false); err != nil {
		return fail("JOURNAL_APPLIED_FAILED", err)
	}
	if err := verifyApplied(ctx, plan, entry); err != nil {
		return fail("VERIFY_FAILED", err)
	}
	entry.Phase = PhaseCommitted
	entry.UpdatedAt = time.Now().UTC()
	if err := writeJSONAtomic(journalPath, entry, false); err != nil {
		return fail("JOURNAL_COMMIT_FAILED", err)
	}
	if options.RegisterArtifact != nil && operationManifest.ArtifactID != "" {
		operationManifest.State = "published"
		operationManifest.LeaseUntil = time.Time{}
		operationManifest.ExpiresAt = time.Time{}
		operationManifest.Rebuildable = true
		operationManifest.RetentionReason = "维护操作已完成；backup 可在 retention 到期后清理，journal 永久保留"
		if _, registerErr := options.RegisterArtifact(context.Background(), operationManifest); registerErr != nil {
			result.NeedsReview = true
			return result, fmt.Errorf("维护操作已提交但更新生成物登记失败：%w", registerErr)
		}
	}
	result.Phase, result.Backups, result.TrashReceipts = entry.Phase, entry.Backups, entry.TrashReceipts
	return result, nil
}

// Recover 从维护 journal 逆序恢复隔离和修复动作；已被用户再次修改的文件不会被覆盖。
func Recover(ctx context.Context, journalPath string, trash platform.TrashProvider) (Result, error) {
	return RecoverWithOptions(ctx, journalPath, RecoverOptions{Trash: trash})
}

// RecoverWithOptions 恢复维护 journal，并在状态改变后同步操作目录登记。
func RecoverWithOptions(ctx context.Context, journalPath string, options RecoverOptions) (Result, error) {
	entry, err := loadJournal(ctx, journalPath)
	if err != nil {
		return Result{}, err
	}
	result := Result{OperationID: entry.OperationID, JournalPath: journalPath, Phase: entry.Phase, Backups: entry.Backups, TrashReceipts: entry.TrashReceipts}
	if entry.Phase == PhaseCommitted || entry.Phase == PhaseRolledBack {
		if options.RegisterArtifact != nil && entry.ArtifactID != "" {
			kind := ArtifactStaging
			reason := "维护操作已完成；backup 可在 retention 到期后清理，journal 永久保留"
			rebuildable := true
			state := "published"
			if entry.Phase == PhaseRolledBack {
				kind = ArtifactFailedArchive
				reason = "维护操作已回滚；等待 failed retention 到期"
				state = "failed"
			}
			manifest := ArtifactManifest{Version: 1, ArtifactID: entry.ArtifactID, Kind: kind, TargetPath: filepath.Join(filepath.Dir(journalPath), entry.OperationID), CreatedAt: entry.CreatedAt, Owner: "maintenance", OperationID: entry.OperationID, Source: "维护操作 backup/staging", Rebuildable: rebuildable, Risk: "high", RetentionReason: reason, State: state, JournalPath: journalPath, WorkspaceRoot: entry.Plan.Root}
			if _, registerErr := options.RegisterArtifact(context.Background(), manifest); registerErr != nil {
				result.NeedsReview = true
				return result, fmt.Errorf("维护终态已落盘但更新生成物登记失败：%w", registerErr)
			}
		}
		return result, nil
	}
	if len(entry.TrashReceipts) > 0 && options.Trash == nil {
		result.NeedsReview = true
		return result, fmt.Errorf("恢复维护 journal 需要回收站 provider")
	}
	for index := len(entry.Backups) - 1; index >= 0; index-- {
		if err := restoreBackup(ctx, entry.Plan.Root, entry.Backups[index]); err != nil {
			entry.Phase, entry.ErrorCode, entry.ErrorMessage = PhaseNeedsReview, "RECOVERY_BACKUP_FAILED", err.Error()
			_ = writeJSONAtomic(journalPath, entry, false)
			result.Phase, result.NeedsReview = entry.Phase, true
			return result, err
		}
	}
	for index := len(entry.TrashReceipts) - 1; index >= 0; index-- {
		receipt := entry.TrashReceipts[index]
		if err := options.Trash.Restore(ctx, receipt, entry.Plan.Root); err != nil {
			entry.Phase, entry.ErrorCode, entry.ErrorMessage = PhaseNeedsReview, "RECOVERY_TRASH_FAILED", err.Error()
			_ = writeJSONAtomic(journalPath, entry, false)
			result.Phase, result.NeedsReview = entry.Phase, true
			return result, err
		}
	}
	entry.Phase, entry.UpdatedAt, entry.RecoveryNote = PhaseRolledBack, time.Now().UTC(), "维护操作已按 backup 和回收站回执恢复"
	if err := writeJSONAtomic(journalPath, entry, false); err != nil {
		return result, err
	}
	if options.RegisterArtifact != nil && entry.ArtifactID != "" {
		manifest := ArtifactManifest{Version: 1, ArtifactID: entry.ArtifactID, Kind: ArtifactFailedArchive, TargetPath: filepath.Join(filepath.Dir(journalPath), entry.OperationID), CreatedAt: entry.CreatedAt, Owner: "maintenance", OperationID: entry.OperationID, Source: "维护操作 backup/staging", Rebuildable: true, Risk: "high", RetentionReason: "维护操作已回滚；等待 failed retention 到期", JournalPath: journalPath, WorkspaceRoot: entry.Plan.Root}
		if _, registerErr := options.RegisterArtifact(context.Background(), manifest); registerErr != nil {
			result.NeedsReview = true
			return result, fmt.Errorf("维护已回滚但更新生成物登记失败：%w", registerErr)
		}
	}
	result.Phase = entry.Phase
	return result, nil
}

// RevalidateAsset 仅重新读取单个计划资产，供执行循环在每次写入前使用。
func RevalidateAsset(ctx context.Context, plan Plan, asset PlanAsset) error {
	path, err := safePath(plan.Root, asset.RelativePath)
	if err != nil {
		return err
	}
	actual, err := fingerprint(ctx, path)
	if err != nil {
		return err
	}
	if actual.SizeBytes != asset.SizeBytes || !actual.ModTime.Equal(asset.ModTime) || actual.SHA256 != asset.SHA256 {
		return fmt.Errorf("候选 %s 指纹已变化", asset.RelativePath)
	}
	return nil
}

func verifyApplied(ctx context.Context, plan Plan, entry journal) error {
	for _, asset := range plan.Assets {
		path, err := safePath(plan.Root, asset.RelativePath)
		if err != nil {
			return err
		}
		switch asset.Action {
		case ActionIsolate:
			if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
				if statErr == nil {
					return fmt.Errorf("隔离后源文件仍存在：%s", asset.RelativePath)
				}
				return statErr
			}
		case ActionRepairJSONLTail:
			input, openErr := os.Open(path)
			if openErr != nil {
				return openErr
			}
			summary, scanErr := catalog.ScanReader(input, nil)
			_ = input.Close()
			if scanErr != nil {
				return scanErr
			}
			if summary.PendingLines != 0 || summary.InvalidJSONLines != 0 || summary.InvalidUTF8Lines != 0 {
				return fmt.Errorf("修复后 JSONL 仍有 framing 错误：%s", asset.RelativePath)
			}
		}
	}
	return nil
}

func repairTail(ctx context.Context, plan Plan, asset PlanAsset, backupRoot string) (BackupRecord, error) {
	path, err := safePath(plan.Root, asset.RelativePath)
	if err != nil {
		return BackupRecord{}, err
	}
	input, err := os.Open(path)
	if err != nil {
		return BackupRecord{}, err
	}
	var pendingStart int64 = -1
	var pendingCount uint64
	var invalid uint64
	_, scanErr := catalog.ScanReaderContent(input, func(frame domain.JSONLFrame, _ []byte) error {
		if frame.Pending {
			pendingStart = frame.StartOffset
			pendingCount++
		}
		if !frame.ValidJSON {
			invalid++
		}
		return nil
	})
	if scanErr != nil {
		_ = input.Close()
		return BackupRecord{}, scanErr
	}
	if closeErr := input.Close(); closeErr != nil {
		return BackupRecord{}, closeErr
	}
	if pendingCount != 1 || invalid != 1 || pendingStart <= 0 {
		return BackupRecord{}, fmt.Errorf("仅允许修复唯一的末尾半截 JSONL 记录")
	}
	backupPath := filepath.Join(backupRoot, filepath.FromSlash(asset.RelativePath)+".bak")
	if err := ensureParent(backupRoot, backupPath); err != nil {
		return BackupRecord{}, err
	}
	if err := copyFile(path, backupPath); err != nil {
		return BackupRecord{}, fmt.Errorf("保存修复前副本失败：%w", err)
	}
	tempPath := path + ".codex-chronicle-repair.tmp"
	if err := copyPrefix(ctx, path, tempPath, pendingStart); err != nil {
		_ = os.Remove(tempPath)
		return BackupRecord{}, err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return BackupRecord{}, fmt.Errorf("发布修复后的 JSONL 失败：%w", err)
	}
	repaired, err := fingerprint(ctx, path)
	if err != nil {
		return BackupRecord{}, err
	}
	return BackupRecord{RelativePath: asset.RelativePath, BackupPath: backupPath, OriginalSHA256: asset.SHA256, RepairedSHA256: repaired.SHA256, OriginalSizeBytes: asset.SizeBytes}, nil
}

func rollback(ctx context.Context, entry *journal, journalPath string, trash platform.TrashProvider) error {
	var firstErr error
	for index := len(entry.Backups) - 1; index >= 0; index-- {
		if err := restoreBackup(ctx, entry.Plan.Root, entry.Backups[index]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for index := len(entry.TrashReceipts) - 1; index >= 0; index-- {
		if trash == nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("缺少回收站 provider")
			}
			break
		}
		if err := trash.Restore(ctx, entry.TrashReceipts[index], entry.Plan.Root); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		entry.Phase = PhaseRolledBack
	} else {
		entry.Phase = PhaseNeedsReview
	}
	entry.UpdatedAt = time.Now().UTC()
	_ = writeJSONAtomic(journalPath, *entry, false)
	return firstErr
}

func restoreBackup(ctx context.Context, root string, backup BackupRecord) error {
	path, err := safePath(root, backup.RelativePath)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(backup.BackupPath); err != nil {
		return err
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("恢复目标不是普通文件：%s", backup.RelativePath)
		}
		actual, hashErr := fingerprint(ctx, path)
		if hashErr != nil {
			return hashErr
		}
		if actual.SHA256 == backup.OriginalSHA256 {
			return nil
		}
		if actual.SHA256 != backup.RepairedSHA256 {
			return fmt.Errorf("恢复目标已被再次修改，拒绝覆盖：%s", backup.RelativePath)
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	temporary := path + ".codex-chronicle-restore.tmp"
	if err := copyFile(backup.BackupPath, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		// Windows 不允许 Rename 覆盖已有目标；目标已经通过指纹校验，移除后再发布副本。
		if removeErr := os.Remove(path); removeErr != nil {
			_ = os.Remove(temporary)
			return fmt.Errorf("准备恢复目标失败：%w", err)
		}
		if retryErr := os.Rename(temporary, path); retryErr != nil {
			_ = os.Remove(temporary)
			return retryErr
		}
	}
	return nil
}

func copyPrefix(ctx context.Context, source, destination string, length int64) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(writerContext{ctx, output}, input, length)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	return closeErr
}

// writerContext 让大文件复制在取消时尽快停止，不缓存整个 JSONL。
type writerContext struct {
	ctx context.Context
	w   io.Writer
}

func (w writerContext) Write(p []byte) (int, error) {
	select {
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	default:
		return w.w.Write(p)
	}
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := ensureParent(filepath.Dir(destination), destination); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	return closeErr
}

func fingerprint(ctx context.Context, path string) (struct {
	SizeBytes int64
	ModTime   time.Time
	SHA256    string
}, error) {
	var result struct {
		SizeBytes int64
		ModTime   time.Time
		SHA256    string
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return result, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return result, fmt.Errorf("维护目标必须是普通文件：%s", path)
	}
	input, err := os.Open(path)
	if err != nil {
		return result, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, input)
	closeErr := input.Close()
	if copyErr != nil {
		return result, copyErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.SizeBytes, result.ModTime, result.SHA256 = info.Size(), info.ModTime().UTC(), hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

func safePath(root, relative string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(relative, "\x00") {
		return "", fmt.Errorf("维护路径越出源根：%s", relative)
	}
	target := filepath.Join(absRoot, clean)
	if rel, err := filepath.Rel(absRoot, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("维护路径越出源根：%s", relative)
	}
	return target, nil
}

func validateDestination(root, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("维护 journal 目录不能为空")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absDestination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if pathHasCodexComponent(absDestination) {
		return fmt.Errorf("维护目标目录不能位于 .codex 路径")
	}
	rel, err := filepath.Rel(absRoot, absDestination)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return fmt.Errorf("维护目标目录不能位于源根内")
	}
	return nil
}

func pathHasCodexComponent(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return false
}

func ensureParent(root, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return nil
}

func fileKind(files []domain.FileInventory, path string) domain.AssetKind {
	for _, file := range files {
		if filepath.ToSlash(file.RelativePath) == path {
			return file.Kind
		}
	}
	return domain.AssetKindUnknown
}

func newOperationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "maintenance-" + hex.EncodeToString(raw[:]), nil
}

func jsonHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func writeJSONAtomic(path string, value any, exclusive bool) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if exclusive {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(tmp, flags, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if exclusive {
		if _, err := os.Stat(path); err == nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("目标已存在：%s", path)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func loadJournal(ctx context.Context, path string) (journal, error) {
	if err := ctx.Err(); err != nil {
		return journal{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return journal{}, err
	}
	var entry journal
	if err := json.Unmarshal(data, &entry); err != nil {
		return journal{}, err
	}
	if entry.Version != 1 || entry.OperationID == "" || entry.Plan.OperationID != entry.OperationID {
		return journal{}, fmt.Errorf("维护 journal 身份无效")
	}
	return entry, nil
}
