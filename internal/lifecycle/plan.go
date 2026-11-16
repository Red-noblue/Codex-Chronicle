// 中文说明：定义可审阅的归档/回收/删除写入计划；当前只生成和校验计划，不执行源目录写操作。
package lifecycle

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
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
)

// Action 是一次生命周期计划中要求执行的动作。
type Action string

const (
	ActionArchive          Action = "archive"
	ActionEncryptedArchive Action = "encrypted_archive"
	ActionTrash            Action = "trash"
	ActionPermanentDelete  Action = "permanent_delete"
	ActionRestore          Action = "restore"
	ActionRepair           Action = "repair"
)

// PlanStatus 是计划当前是否仍可执行的状态。
type PlanStatus string

const (
	PlanPrepared  PlanStatus = "prepared"
	PlanInvalid   PlanStatus = "invalid"
	PlanConfirmed PlanStatus = "confirmed"
)

// AssetFingerprint 是执行前后比较的最小文件指纹。
type AssetFingerprint struct {
	RelativePath     string           `json:"relative_path"`
	Kind             domain.AssetKind `json:"kind"`
	ControlPlaneOnly bool             `json:"control_plane_only,omitempty"`
	SizeBytes        int64            `json:"size_bytes"`
	ModTime          time.Time        `json:"mod_time"`
	Identity         string           `json:"identity,omitempty"`
	SHA256           string           `json:"sha256"`
}

// PlanOptions 明确用户选择的生命周期组合。
type PlanOptions struct {
	Archive          bool
	Encrypted        bool
	Trash            bool
	PermanentDelete  bool
	ConfirmationCode string
	AssetOwners      map[string]int
}

// OperationPlan 是可展示、可重新校验和可补偿的写入计划；它本身不执行操作。
type OperationPlan struct {
	OperationID        string             `json:"operation_id"`
	CreatedAt          time.Time          `json:"created_at"`
	Root               string             `json:"root"`
	ConversationID     string             `json:"conversation_id"`
	SourceID           string             `json:"source_id,omitempty"`
	AssetSetHash       string             `json:"asset_set_hash"`
	AdapterID          string             `json:"adapter_id"`
	Status             PlanStatus         `json:"status"`
	Actions            []Action           `json:"actions"`
	Assets             []AssetFingerprint `json:"assets"`
	SharedAssetPaths   []string           `json:"shared_asset_paths,omitempty"`
	Compensation       []string           `json:"compensation"`
	Verification       []string           `json:"verification"`
	RequiresConfirm    bool               `json:"requires_confirmation"`
	ConfirmationDigest string             `json:"confirmation_digest,omitempty"`
	ReadOnly           bool               `json:"read_only"`
}

// CaptureAssetFingerprints 在不要求资产处于稳定状态的前提下捕获完整源指纹。
// 暂存操作使用它保存活动期间的基线；它不会生成写入计划，也不会修改源目录。
func CaptureAssetFingerprints(ctx context.Context, root string, files []domain.FileInventory) ([]AssetFingerprint, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析源根失败: %w", err)
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("源根不能为空")
	}
	fingerprints := make([]AssetFingerprint, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		relative := canonicalAssetPath(file.RelativePath)
		if relative == "." || relative == "" {
			return nil, fmt.Errorf("资产路径不能为空")
		}
		if _, duplicate := seen[relative]; duplicate {
			continue
		}
		seen[relative] = struct{}{}
		path, resolveErr := application.ResolveReadOnlyPath(absoluteRoot, relative)
		if resolveErr != nil {
			return nil, fmt.Errorf("读取资产 %s 失败: %w", relative, resolveErr)
		}
		fingerprint, fingerprintErr := fingerprint(ctx, path, relative, file.Kind, isControlPlaneAsset(file.Kind))
		if fingerprintErr != nil {
			return nil, fmt.Errorf("读取资产 %s 指纹失败: %w", relative, fingerprintErr)
		}
		fingerprints = append(fingerprints, fingerprint)
	}
	sort.Slice(fingerprints, func(left, right int) bool { return fingerprints[left].RelativePath < fingerprints[right].RelativePath })
	return fingerprints, nil
}

// BuildPlan 生成只读 dry-run 计划。任何不安全状态、未声明能力或指纹读取失败都会拒绝计划。
func BuildPlan(ctx context.Context, root string, conversation domain.ConversationSummary, files []domain.FileInventory, descriptor compat.Descriptor, options PlanOptions) (OperationPlan, error) {
	plan := OperationPlan{
		CreatedAt: time.Now().UTC(), Root: root, ConversationID: conversation.ID, SourceID: conversation.SourceID,
		AssetSetHash: conversation.AssetSetHash, AdapterID: descriptor.ID,
		Status: PlanInvalid, Actions: make([]Action, 0), Assets: make([]AssetFingerprint, 0),
		Compensation: []string{"保留操作 journal 和原始快照", "任何阶段失败都停止后续动作并进入人工复核"},
		Verification: []string{"重新扫描受影响资产", "校验归档 manifest 哈希", "确认源、回收站和控制面状态一致"},
		ReadOnly:     true,
	}
	operationID, err := newOperationID()
	if err != nil {
		return plan, err
	}
	plan.OperationID = operationID
	if strings.TrimSpace(root) == "" || strings.TrimSpace(conversation.ID) == "" {
		return plan, fmt.Errorf("计划必须包含源根和对话 ID")
	}
	if conversation.Status != domain.StatusStable {
		return plan, fmt.Errorf("对话状态 %s 不允许生成写入计划", conversation.Status)
	}
	if len(files) == 0 {
		return plan, fmt.Errorf("对话没有可验证的物理资产")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return plan, fmt.Errorf("解析源根失败: %w", err)
	}
	var rolloutTargets []string
	for _, file := range files {
		if file.Kind == domain.AssetKindJSONL {
			rolloutTargets = append(rolloutTargets, file.RelativePath)
		}
	}
	if len(rolloutTargets) > 0 {
		if err := reader.CheckDependencies(ctx, absoluteRoot, rolloutTargets); err != nil {
			return plan, err
		}
	}
	for _, file := range files {
		if file.Status != domain.StatusStable || file.ChangedDuringScan {
			return plan, fmt.Errorf("资产 %s 状态 %s 不允许写入", file.RelativePath, file.Status)
		}
		if owners := options.AssetOwners[file.RelativePath]; owners > 1 && !descriptor.AllowsSharedAssetSplit(file.Kind) {
			return plan, fmt.Errorf("资产 %s 被 %d 个对话共享，当前 adapter 不支持按行安全拆分，拒绝写入", file.RelativePath, owners)
		}
		if options.AssetOwners[file.RelativePath] > 1 {
			plan.SharedAssetPaths = append(plan.SharedAssetPaths, file.RelativePath)
		}
		path, err := application.ResolveReadOnlyPath(absoluteRoot, file.RelativePath)
		if err != nil {
			return plan, fmt.Errorf("资产 %s 无法安全定位: %w", file.RelativePath, err)
		}
		fingerprint, err := fingerprint(ctx, path, file.RelativePath, file.Kind, isControlPlaneAsset(file.Kind))
		if err != nil {
			return plan, fmt.Errorf("读取资产 %s 指纹失败: %w", file.RelativePath, err)
		}
		plan.Assets = append(plan.Assets, fingerprint)
	}
	if len(plan.Assets) == 0 {
		return plan, fmt.Errorf("计划没有资产")
	}
	sort.Strings(plan.SharedAssetPaths)
	if err := validateActions(descriptor, options, &plan); err != nil {
		return plan, err
	}
	if options.PermanentDelete {
		plan.RequiresConfirm = true
		if !regexp.MustCompile(`^[0-9]{4,8}$`).MatchString(options.ConfirmationCode) {
			return plan, fmt.Errorf("永久删除计划必须提供 4-8 位随机数字确认代码")
		}
		plan.ConfirmationDigest = confirmationDigest(plan.OperationID, options.ConfirmationCode)
	}
	plan.Status = PlanPrepared
	return plan, nil
}

// RevalidatePlan 重新读取指纹，只有源资产没有变化时才允许进入执行阶段。
func RevalidatePlan(ctx context.Context, plan OperationPlan) error {
	var targets []string
	for _, asset := range plan.Assets {
		if asset.Kind == domain.AssetKindJSONL {
			targets = append(targets, asset.RelativePath)
		}
	}
	if len(targets) > 0 {
		if err := reader.CheckDependencies(ctx, plan.Root, targets); err != nil {
			return err
		}
	}
	if plan.Status != PlanPrepared && plan.Status != PlanConfirmed {
		return fmt.Errorf("计划状态 %s 不可重新校验", plan.Status)
	}
	for _, expected := range plan.Assets {
		path, err := application.ResolveReadOnlyPath(plan.Root, expected.RelativePath)
		if err != nil {
			return fmt.Errorf("资产 %s 已不可定位: %w", expected.RelativePath, err)
		}
		actual, err := fingerprint(ctx, path, expected.RelativePath, expected.Kind, expected.ControlPlaneOnly)
		if err != nil {
			return err
		}
		if actual.SizeBytes != expected.SizeBytes || !actual.ModTime.Equal(expected.ModTime) || (expected.Identity != "" && actual.Identity != expected.Identity) || actual.SHA256 != expected.SHA256 {
			return fmt.Errorf("资产 %s 指纹已变化，计划失效", expected.RelativePath)
		}
	}
	return nil
}

// RebaseSharedAssetFingerprints 仅在 completed 已完整提交后调用；它只刷新双方明确声明的共享资产。
func RebaseSharedAssetFingerprints(ctx context.Context, descriptor compat.Descriptor, completed OperationPlan, pending []*OperationPlan) error {
	if len(completed.SharedAssetPaths) == 0 || len(pending) == 0 {
		return nil
	}
	if descriptor.ID == "" || descriptor.ID != completed.AdapterID || !descriptor.AllowsWrite(compat.CapabilityDelete) {
		return fmt.Errorf("adapter 未授权共享资产指纹更新")
	}
	root, err := filepath.Abs(completed.Root)
	if err != nil {
		return fmt.Errorf("解析已完成计划源根失败: %w", err)
	}
	completedAssets := make(map[string]AssetFingerprint, len(completed.SharedAssetPaths))
	completedPaths := make(map[string]struct{}, len(completed.SharedAssetPaths))
	for _, relative := range completed.SharedAssetPaths {
		canonical := canonicalAssetPath(relative)
		if _, duplicate := completedPaths[canonical]; duplicate {
			return fmt.Errorf("已完成计划包含重复共享资产路径: %s", canonical)
		}
		completedPaths[canonical] = struct{}{}
		asset, found := findAsset(completed.Assets, canonical)
		if !found || !descriptor.AllowsSharedAssetSplit(asset.Kind) {
			return fmt.Errorf("已完成计划共享资产 %s 缺少资产指纹", canonical)
		}
		completedAssets[canonical] = asset
	}

	needed := make(map[string]struct{})
	for _, plan := range pending {
		if plan == nil || (plan.Status != PlanPrepared && plan.Status != PlanConfirmed) {
			return fmt.Errorf("待执行计划状态无效，拒绝更新共享资产指纹")
		}
		if plan.AdapterID != descriptor.ID {
			return fmt.Errorf("待执行计划 adapter 与已完成计划不一致")
		}
		pendingRoot, rootErr := filepath.Abs(plan.Root)
		if rootErr != nil || filepath.Clean(pendingRoot) != filepath.Clean(root) {
			return fmt.Errorf("待执行计划源根与已完成计划不一致")
		}
		for _, relative := range plan.SharedAssetPaths {
			canonical := canonicalAssetPath(relative)
			if _, shared := completedPaths[canonical]; !shared {
				continue
			}
			asset, found := findAsset(plan.Assets, canonical)
			if !found || asset.Kind != completedAssets[canonical].Kind || asset.ControlPlaneOnly != completedAssets[canonical].ControlPlaneOnly || !descriptor.AllowsSharedAssetSplit(asset.Kind) {
				return fmt.Errorf("待执行计划共享资产 %s 缺少匹配授权", canonical)
			}
			needed[canonical] = struct{}{}
		}
	}
	if len(needed) == 0 {
		return nil
	}

	current := make(map[string]AssetFingerprint, len(needed))
	for relative := range needed {
		expected := completedAssets[relative]
		path, resolveErr := application.ResolveReadOnlyPath(root, relative)
		if resolveErr != nil {
			return fmt.Errorf("共享资产 %s 无法安全定位: %w", relative, resolveErr)
		}
		actual, fingerprintErr := fingerprint(ctx, path, relative, expected.Kind, expected.ControlPlaneOnly)
		if fingerprintErr != nil {
			return fmt.Errorf("捕获共享资产 %s 新指纹失败: %w", relative, fingerprintErr)
		}
		current[relative] = actual
	}

	updates := make([]struct {
		plan  *OperationPlan
		index int
		asset AssetFingerprint
	}, 0)
	for _, plan := range pending {
		for _, relative := range plan.SharedAssetPaths {
			canonical := canonicalAssetPath(relative)
			actual, shared := current[canonical]
			if !shared {
				continue
			}
			index := findAssetIndex(plan.Assets, canonical)
			if index < 0 {
				return fmt.Errorf("待执行计划共享资产 %s 缺少资产指纹", canonical)
			}
			updates = append(updates, struct {
				plan  *OperationPlan
				index int
				asset AssetFingerprint
			}{plan: plan, index: index, asset: actual})
		}
	}
	for _, update := range updates {
		update.plan.Assets[update.index] = update.asset
	}
	return nil
}

func canonicalAssetPath(relative string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
}

func findAsset(assets []AssetFingerprint, relative string) (AssetFingerprint, bool) {
	for _, asset := range assets {
		if canonicalAssetPath(asset.RelativePath) == relative {
			return asset, true
		}
	}
	return AssetFingerprint{}, false
}

func findAssetIndex(assets []AssetFingerprint, relative string) int {
	for index, asset := range assets {
		if canonicalAssetPath(asset.RelativePath) == relative {
			return index
		}
	}
	return -1
}

// MarshalManifest 生成可写入归档包的稳定 JSON manifest；不包含正文。
func MarshalManifest(plan OperationPlan) ([]byte, error) {
	if plan.Status == PlanInvalid {
		return nil, fmt.Errorf("无效计划不能生成 manifest")
	}
	return json.MarshalIndent(plan, "", "  ")
}

// VerifyConfirmation 校验执行阶段提供的确认数字；计划只保存摘要，不保存用户输入的明文数字。
func VerifyConfirmation(plan OperationPlan, code string) bool {
	if !plan.RequiresConfirm || plan.ConfirmationDigest == "" {
		return false
	}
	if !regexp.MustCompile(`^[0-9]{4,8}$`).MatchString(code) {
		return false
	}
	return confirmationDigest(plan.OperationID, code) == plan.ConfirmationDigest
}

func validateActions(descriptor compat.Descriptor, options PlanOptions, plan *OperationPlan) error {
	if options.Encrypted && !options.Archive {
		return fmt.Errorf("加密归档必须同时选择归档")
	}
	if !options.Archive && !options.Trash && !options.PermanentDelete {
		return fmt.Errorf("至少选择归档、回收站或永久删除之一")
	}
	if options.Trash && options.PermanentDelete {
		return fmt.Errorf("回收站和永久删除不能同时选择")
	}
	if options.Archive {
		capability := compat.CapabilityArchiveExport
		if options.Encrypted {
			capability = compat.CapabilityArchiveExport
		}
		if !descriptor.AllowsWrite(capability) {
			return fmt.Errorf("adapter 未授权 %s", capability)
		}
		if options.Encrypted {
			plan.Actions = append(plan.Actions, ActionEncryptedArchive)
		} else {
			plan.Actions = append(plan.Actions, ActionArchive)
		}
	}
	if options.Trash {
		if !descriptor.AllowsWrite(compat.CapabilityDelete) {
			return fmt.Errorf("adapter 未授权回收站删除")
		}
		plan.Actions = append(plan.Actions, ActionTrash)
	}
	if options.PermanentDelete {
		if !descriptor.AllowsWrite(compat.CapabilityDelete) {
			return fmt.Errorf("adapter 未授权永久删除")
		}
		plan.Actions = append(plan.Actions, ActionPermanentDelete)
	}
	return nil
}

func fingerprint(ctx context.Context, path, relative string, kind domain.AssetKind, controlPlaneOnly bool) (AssetFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return AssetFingerprint{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return AssetFingerprint{}, err
	}
	input, err := os.Open(path)
	if err != nil {
		return AssetFingerprint{}, err
	}
	defer input.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, input); err != nil {
		return AssetFingerprint{}, err
	}
	if err := ctx.Err(); err != nil {
		return AssetFingerprint{}, err
	}
	return AssetFingerprint{RelativePath: relative, Kind: kind, ControlPlaneOnly: controlPlaneOnly, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Identity: fileIdentity(info), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
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

// FileAssets 返回需要归档、移动或删除的普通文件；SQLite 和 session index 由兼容 adapter 按行处理。
func (p OperationPlan) FileAssets() []AssetFingerprint {
	assets := make([]AssetFingerprint, 0, len(p.Assets))
	for _, asset := range p.Assets {
		if !asset.ControlPlaneOnly && !isControlPlaneAsset(asset.Kind) {
			assets = append(assets, asset)
		}
	}
	return assets
}

func isControlPlaneAsset(kind domain.AssetKind) bool {
	return kind == domain.AssetKindSQLite || kind == domain.AssetKindSessionIndex
}

func newOperationID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("生成操作 ID 失败: %w", err)
	}
	return "operation-" + hex.EncodeToString(bytes[:]), nil
}

func confirmationDigest(operationID, code string) string {
	digest := sha256.Sum256([]byte(operationID + "\x00" + code))
	return hex.EncodeToString(digest[:])
}
