// 中文说明：只对明确标记、位于 .codex 之外的真实结构副本执行可验证的控制面变更。
package compat

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

	"codex-chronicle.local/internal/domain"
)

// RealShapeCopyMutation 是真实结构副本专用 adapter；它不接受真实 .codex 根目录。
type RealShapeCopyMutation struct {
	descriptor        Descriptor
	snapshotDirectory string
}

// RealShapeCopyDescriptor 声明副本 adapter 的能力，不代表任何真实 Codex 版本。
func RealShapeCopyDescriptor() Descriptor {
	return Descriptor{
		ID:                 RealShapeCopyKind,
		SupportedVersions:  []string{RealShapeCopyKind, "0.166.0"},
		SchemaFingerprints: []string{RealShapeFixtureSchemaSHA256, RealCodex0166SchemaSHA256},
		FixtureVersions:    []string{RealShapeCopyKind},
		KnownLimits: []string{
			"只接受 codex-real-copy-v1 标记、已登记的 fixture 或 Codex 0.166.0 六库 schema 和 .codex 之外的副本根",
			"Codex 0.166.0 能力只代表当前隔离副本证据，不自动授予真实 .codex 写权限",
			"副本必须在写入前由上游活动探测确认稳定；WAL、活动 writer 或 schema 漂移不得绕过该门禁",
		},
		Capabilities: map[Capability]bool{
			CapabilityRead: true, CapabilityInspect: true, CapabilitySearch: true,
			CapabilityArchiveExport: true, CapabilityDelete: true, CapabilityRestore: true,
			CapabilityVerify: true, CapabilityActivityProbe: true, CapabilitySnapshot: true,
		},
		SharedAssetKinds: []domain.AssetKind{domain.AssetKindSQLite, domain.AssetKindSessionIndex},
	}
}

// NewRealShapeCopyMutation 创建副本 mutation adapter 的独立快照目录。
func NewRealShapeCopyMutation(snapshotDirectory string) (*RealShapeCopyMutation, error) {
	if strings.TrimSpace(snapshotDirectory) == "" {
		return nil, &MutationError{Code: "MUTATION_SNAPSHOT_DIRECTORY_MISSING", Message: "真实副本 mutation 必须配置独立快照目录", ReadOnly: true}
	}
	abs, err := filepath.Abs(snapshotDirectory)
	if err != nil {
		return nil, fmt.Errorf("解析真实副本快照目录失败: %w", err)
	}
	if pathHasCodexComponent(abs) {
		return nil, &MutationError{Code: "MUTATION_SNAPSHOT_DIRECTORY_CODEX", Message: "真实副本快照目录不能位于 .codex 路径", ReadOnly: true}
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, &MutationError{Code: "MUTATION_SNAPSHOT_DIRECTORY_SYMLINK", Message: "真实副本快照目录不能是符号链接", ReadOnly: true}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("检查真实副本快照目录失败: %w", statErr)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("创建真实副本快照目录失败: %w", err)
	}
	return &RealShapeCopyMutation{descriptor: RealShapeCopyDescriptor(), snapshotDirectory: filepath.Clean(abs)}, nil
}

// Descriptor 返回仅对明确真实结构副本授权的能力描述。
func (m *RealShapeCopyMutation) Descriptor() Descriptor {
	if m == nil {
		return GenericJSONLDescriptor()
	}
	return m.descriptor
}

// Prepare 在副本上重新读取所有控制面关系、session index 和资产指纹。
func (m *RealShapeCopyMutation) Prepare(ctx context.Context, request MutationRequest) (MutationSnapshot, error) {
	if err := m.validateAdapter(ctx, request); err != nil {
		return MutationSnapshot{}, err
	}
	if request.SourceID == "" {
		return MutationSnapshot{}, &MutationError{Code: "MUTATION_SOURCE_ID_MISSING", Message: "真实副本 mutation 必须绑定 SQLite thread ID", ReadOnly: true}
	}
	shape, err := InspectRealShapeCopy(ctx, request.Root, request.SourceID)
	if err != nil {
		return MutationSnapshot{}, fmt.Errorf("读取真实副本关系快照失败: %w", err)
	}
	if !shape.Complete {
		return MutationSnapshot{}, &MutationError{Code: "MUTATION_REALCOPY_INCOMPLETE", Message: "真实副本关系快照不完整，拒绝授权写入", ReadOnly: true}
	}
	targets := mutationTargets(request)
	rows := make([]sqliteRowSnapshot, 0)
	for _, databaseName := range realShapeDatabases {
		if err := ctx.Err(); err != nil {
			return MutationSnapshot{}, err
		}
		path, pathErr := realShapeReadOnlyPath(request.Root, databaseName)
		if pathErr != nil {
			return MutationSnapshot{}, fmt.Errorf("数据库 %s 不可安全定位: %w", databaseName, pathErr)
		}
		database, openErr := openFixtureSQLite(ctx, path, true)
		if openErr != nil {
			return MutationSnapshot{}, openErr
		}
		fileRows, readErr := collectSQLiteRows(ctx, database, databaseName, targets)
		closeErr := database.Close()
		if readErr != nil {
			return MutationSnapshot{}, fmt.Errorf("读取真实副本数据库 %s 关系行失败: %w", databaseName, readErr)
		}
		if closeErr != nil {
			return MutationSnapshot{}, fmt.Errorf("关闭真实副本数据库 %s 失败: %w", databaseName, closeErr)
		}
		for _, row := range fileRows {
			if isRetainedRealShapeCopyRow(row) {
				continue
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return MutationSnapshot{}, &MutationError{Code: "MUTATION_RELATION_ROWS_MISSING", Message: "真实副本没有找到与目标 thread 关联的 SQLite 行，拒绝猜测删除范围", ReadOnly: true}
	}
	var indexes []sessionIndexSnapshot
	if len(shape.SessionIndexLines) > 0 {
		indexPath, indexErr := realShapeReadOnlyPath(request.Root, realShapeSessionIndex)
		if indexErr != nil {
			return MutationSnapshot{}, fmt.Errorf("真实副本 session index 不可安全定位: %w", indexErr)
		}
		index, indexErr := collectSessionIndexSnapshot(ctx, indexPath, realShapeSessionIndex, targets)
		if indexErr != nil {
			return MutationSnapshot{}, indexErr
		}
		indexes = []sessionIndexSnapshot{index}
	}
	assets, err := collectRealShapeCopyAssets(ctx, request.Root, shape, request.Assets)
	if err != nil {
		return MutationSnapshot{}, err
	}
	sortSQLiteRows(rows)
	payload := sqliteMutationPayload{
		Version: 1, AdapterID: m.descriptor.ID, Root: filepath.Clean(request.Root), ConversationID: request.ConversationID,
		SourceID: request.SourceID, AssetSetHash: request.AssetSetHash, Assets: assets, Rows: rows, Indexes: indexes,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return MutationSnapshot{}, fmt.Errorf("序列化真实副本行快照失败: %w", err)
	}
	path, err := m.writeSnapshot(encoded)
	if err != nil {
		return MutationSnapshot{}, err
	}
	digest := sha256.Sum256(encoded)
	request.Assets = assets
	request.SnapshotRef = path
	request.SnapshotSHA256 = hex.EncodeToString(digest[:])
	return BuildMutationSnapshot(ctx, m.descriptor.ID, request)
}

// Apply 在控制面数据库内执行事务删除；rollout 文件由生命周期 saga 处理。
func (m *RealShapeCopyMutation) Apply(ctx context.Context, snapshot MutationSnapshot, action MutationAction) (MutationOutcome, error) {
	if action != MutationDelete {
		return MutationOutcome{}, realCopyUnsupportedAction(action)
	}
	payload, err := m.loadSnapshot(ctx, snapshot)
	if err != nil {
		return MutationOutcome{}, err
	}
	if err := preflightRealShapeCopy(ctx, snapshot.Root, payload, false); err != nil {
		return MutationOutcome{}, err
	}
	outcome := MutationOutcome{Action: action, AdapterID: snapshot.AdapterID, SnapshotHash: snapshot.SnapshotHash}
	for _, index := range payload.Indexes {
		changed, applyErr := applySessionIndex(ctx, snapshot.Root, index, false)
		if applyErr != nil {
			outcome.NeedsReview = outcome.ChangedRows > 0 || outcome.ChangedFiles > 0
			return outcome, applyErr
		}
		outcome.ChangedFiles += changed
	}
	rowsByDatabase := groupRealShapeCopyRows(payload.Rows, false)
	databaseNames := sortedSQLiteDatabaseNames(rowsByDatabase)
	for _, databaseName := range databaseNames {
		changed, applyErr := (&SyntheticSQLiteMutation{}).applyDatabase(ctx, snapshot.Root, databaseName, rowsByDatabase[databaseName], false)
		if applyErr != nil {
			outcome.NeedsReview = outcome.ChangedRows > 0 || outcome.ChangedFiles > 0
			return outcome, applyErr
		}
		outcome.ChangedRows += changed
	}
	if err := m.Verify(ctx, snapshot, outcome); err != nil {
		outcome.NeedsReview = true
		return outcome, err
	}
	outcome.Verified = true
	outcome.Verification = []string{"真实副本 SQLite 目标行和 session index 目标记录已重新读取并确认不存在"}
	return outcome, nil
}

// Verify 只读确认副本快照中的全部控制面目标已经消失。
func (m *RealShapeCopyMutation) Verify(ctx context.Context, snapshot MutationSnapshot, outcome MutationOutcome) error {
	payload, err := m.loadSnapshot(ctx, snapshot)
	if err != nil {
		return err
	}
	rowsByDatabase := groupSQLiteRows(payload.Rows)
	for _, index := range payload.Indexes {
		if err := verifySessionIndex(ctx, snapshot.Root, index, false); err != nil {
			return err
		}
	}
	for _, databaseName := range sortedSQLiteDatabaseNames(rowsByDatabase) {
		path, pathErr := fixturePath(snapshot.Root, databaseName)
		if pathErr != nil {
			return pathErr
		}
		if err := ensureRegularFixturePath(path); err != nil {
			return err
		}
		database, openErr := openFixtureSQLite(ctx, path, true)
		if openErr != nil {
			return openErr
		}
		for _, row := range rowsByDatabase[databaseName] {
			exists, checkErr := sqliteRowMatches(ctx, database, row)
			if checkErr != nil {
				_ = database.Close()
				return checkErr
			}
			if exists {
				_ = database.Close()
				return fmt.Errorf("真实副本 SQLite 行仍存在：%s.%s", row.Database, row.Table)
			}
		}
		if closeErr := database.Close(); closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// Restore 按照副本快照恢复缺失的控制面行，并拒绝覆盖冲突内容。
func (m *RealShapeCopyMutation) Restore(ctx context.Context, snapshot MutationSnapshot) (MutationOutcome, error) {
	payload, err := m.loadSnapshot(ctx, snapshot)
	if err != nil {
		return MutationOutcome{}, err
	}
	if err := preflightRealShapeCopy(ctx, snapshot.Root, payload, true); err != nil {
		return MutationOutcome{}, err
	}
	outcome := MutationOutcome{Action: MutationRestore, AdapterID: snapshot.AdapterID, SnapshotHash: snapshot.SnapshotHash}
	for _, index := range payload.Indexes {
		changed, restoreErr := applySessionIndex(ctx, snapshot.Root, index, true)
		if restoreErr != nil {
			outcome.NeedsReview = outcome.ChangedRows > 0 || outcome.ChangedFiles > 0
			return outcome, restoreErr
		}
		outcome.ChangedFiles += changed
		if verifyErr := verifySessionIndex(ctx, snapshot.Root, index, true); verifyErr != nil {
			outcome.NeedsReview = true
			return outcome, verifyErr
		}
	}
	rowsByDatabase := groupRealShapeCopyRows(payload.Rows, true)
	for _, databaseName := range sortedSQLiteDatabaseNames(rowsByDatabase) {
		changed, restoreErr := (&SyntheticSQLiteMutation{}).applyDatabase(ctx, snapshot.Root, databaseName, rowsByDatabase[databaseName], true)
		if restoreErr != nil {
			outcome.NeedsReview = outcome.ChangedRows > 0 || outcome.ChangedFiles > 0
			return outcome, restoreErr
		}
		outcome.ChangedRows += changed
	}
	outcome.Verified = true
	outcome.Verification = []string{"真实副本 SQLite 行和 session index 已按快照恢复，并拒绝覆盖冲突对象"}
	return outcome, nil
}

func (m *RealShapeCopyMutation) validateAdapter(ctx context.Context, request MutationRequest) error {
	if m == nil {
		return &MutationError{Code: "MUTATION_ADAPTER_MISSING", Message: "真实副本 mutation adapter 为空", ReadOnly: true}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.descriptor.ID != RealShapeCopyKind || !m.descriptor.AllowsWrite(CapabilityDelete) {
		return &MutationError{Code: "MUTATION_ADAPTER_NOT_AUTHORIZED", Message: "当前 adapter 未授权真实结构副本写入", ReadOnly: true}
	}
	if pathHasCodexComponent(request.Root) {
		return &MutationError{Code: "MUTATION_REALCOPY_CODEX_ROOT", Message: "真实副本 mutation 拒绝 .codex 根目录", ReadOnly: true}
	}
	if err := ValidateMutationRequest(ctx, request); err != nil {
		return err
	}
	if strings.TrimSpace(request.SourceID) == "" {
		return &MutationError{Code: "MUTATION_SOURCE_ID_MISSING", Message: "真实副本 mutation 必须绑定 SQLite thread ID", ReadOnly: true}
	}
	if pathsOverlap(request.Root, m.snapshotDirectory) {
		return &MutationError{Code: "MUTATION_SNAPSHOT_DIRECTORY_OVERLAP", Message: "真实副本快照目录不能与副本根重叠", ReadOnly: true}
	}
	absRoot, err := realShapeRoot(request.Root)
	if err != nil {
		return &MutationError{Code: "MUTATION_REALCOPY_ROOT_INVALID", Message: "真实副本根不是安全普通目录", ReadOnly: true}
	}
	if _, err := readRealShapeCopyMarker(ctx, absRoot); err != nil {
		return err
	}
	for _, asset := range request.Assets {
		path, pathErr := fixturePath(absRoot, asset.RelativePath)
		if pathErr != nil {
			return pathErr
		}
		if err := verifyFixtureAsset(path, asset); err != nil {
			return err
		}
	}
	return nil
}

func (m *RealShapeCopyMutation) writeSnapshot(encoded []byte) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("生成真实副本快照 ID 失败: %w", err)
		}
		path := filepath.Join(m.snapshotDirectory, "real-copy-mutation-"+hex.EncodeToString(random[:])+".json")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", fmt.Errorf("创建真实副本行快照失败: %w", err)
		}
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("写入真实副本行快照失败: %w", err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("同步真实副本行快照失败: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("关闭真实副本行快照失败: %w", err)
		}
		return path, nil
	}
	return "", fmt.Errorf("生成真实副本行快照 ID 冲突")
}

func (m *RealShapeCopyMutation) loadSnapshot(ctx context.Context, snapshot MutationSnapshot) (sqliteMutationPayload, error) {
	if err := ValidateMutationSnapshot(ctx, snapshot); err != nil {
		return sqliteMutationPayload{}, err
	}
	if m == nil || snapshot.AdapterID != m.descriptor.ID || strings.TrimSpace(snapshot.SnapshotRef) == "" || !pathWithin(m.snapshotDirectory, snapshot.SnapshotRef) {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_REFERENCE_INVALID", Message: "真实副本行快照引用越出快照目录", ReadOnly: true}
	}
	info, err := os.Lstat(snapshot.SnapshotRef)
	if err != nil {
		return sqliteMutationPayload{}, fmt.Errorf("读取真实副本行快照失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_NOT_REGULAR", Message: "真实副本行快照不是普通文件", ReadOnly: true}
	}
	data, err := os.ReadFile(snapshot.SnapshotRef)
	if err != nil {
		return sqliteMutationPayload{}, fmt.Errorf("读取真实副本行快照失败: %w", err)
	}
	digest := sha256.Sum256(dataWithoutTrailingNewline(data))
	if hex.EncodeToString(digest[:]) != snapshot.SnapshotSHA256 {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_TAMPERED", Message: "真实副本行快照摘要不匹配", ReadOnly: true}
	}
	var payload sqliteMutationPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return sqliteMutationPayload{}, fmt.Errorf("解析真实副本行快照失败: %w", err)
	}
	if payload.Version != 1 || payload.AdapterID != snapshot.AdapterID || filepath.Clean(payload.Root) != filepath.Clean(snapshot.Root) || payload.ConversationID != snapshot.ConversationID || payload.SourceID != snapshot.SourceID || (len(payload.Rows) == 0 && len(payload.Indexes) == 0) {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_IDENTITY_INVALID", Message: "真实副本行快照身份不匹配", ReadOnly: true}
	}
	if !mutationAssetsEqual(payload.Assets, snapshot.Assets) {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_ASSETS_INVALID", Message: "真实副本行快照资产集合不匹配", ReadOnly: true}
	}
	if _, err := readRealShapeCopyMarker(ctx, snapshot.Root); err != nil {
		return sqliteMutationPayload{}, err
	}
	return payload, nil
}

func readRealShapeCopyMarker(ctx context.Context, root string) (RealShapeFixtureManifest, error) {
	if pathHasCodexComponent(root) {
		return RealShapeFixtureManifest{}, &MutationError{Code: "MUTATION_REALCOPY_CODEX_ROOT", Message: "真实副本根目录不能是 .codex 路径", ReadOnly: true}
	}
	absRoot, err := realShapeRoot(root)
	if err != nil {
		return RealShapeFixtureManifest{}, &MutationError{Code: "MUTATION_REALCOPY_ROOT_INVALID", Message: "真实副本根不是安全普通目录", ReadOnly: true}
	}
	markerPath, err := realShapeReadOnlyPath(absRoot, realShapeCopyMarker)
	if err != nil {
		return RealShapeFixtureManifest{}, &MutationError{Code: "MUTATION_REALCOPY_MARKER_MISSING", Message: "真实副本缺少安全的 codex-real-copy-v1 标记", ReadOnly: true}
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return RealShapeFixtureManifest{}, fmt.Errorf("读取真实副本标记失败: %w", err)
	}
	var marker RealShapeFixtureManifest
	if err := json.Unmarshal(data, &marker); err != nil {
		return RealShapeFixtureManifest{}, &MutationError{Code: "MUTATION_REALCOPY_MARKER_INVALID", Message: "真实副本标记不是有效 JSON", ReadOnly: true}
	}
	if marker.Kind != RealShapeCopyKind || !trustedRealShapeSchema(marker) {
		return RealShapeFixtureManifest{}, &MutationError{Code: "MUTATION_REALCOPY_MARKER_UNTRUSTED", Message: "真实副本标记或 schema 摘要不受信任", ReadOnly: true}
	}
	if marker.CompletenessState == "incomplete" || (marker.ControlPlane != nil && (!marker.ControlPlane.Complete || marker.ControlPlane.CompletenessState == "incomplete")) {
		return RealShapeFixtureManifest{}, &MutationError{Code: "MUTATION_REALCOPY_SNAPSHOT_INCOMPLETE", Message: "真实副本控制面快照不完整，拒绝 mutation", ReadOnly: true}
	}
	semanticHash, err := realShapeSQLiteSemantics(ctx, absRoot)
	if err != nil {
		return RealShapeFixtureManifest{}, fmt.Errorf("读取真实副本 SQLite 语义摘要失败: %w", err)
	}
	if semanticHash != marker.SQLiteSemanticsSHA256 {
		return RealShapeFixtureManifest{}, &MutationError{Code: "MUTATION_REALCOPY_SCHEMA_CHANGED", Message: "真实副本 SQLite 语义摘要已变化，拒绝写入", ReadOnly: true}
	}
	return marker, nil
}

func collectRealShapeCopyAssets(ctx context.Context, root string, shape RealShapeFixtureSnapshot, requested []MutationAsset) ([]MutationAsset, error) {
	assetsByPath := make(map[string]MutationAsset, len(requested)+len(realShapeDatabases)+3)
	for _, asset := range requested {
		path := filepath.ToSlash(filepath.Clean(filepath.FromSlash(asset.RelativePath)))
		asset.RelativePath = path
		assetsByPath[path] = asset
	}
	addRequired := func(relative string, kind domain.AssetKind) error {
		relative = filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
		if existing, ok := assetsByPath[relative]; ok {
			if existing.Kind != kind && !(relative == realShapeCopyMarker && existing.Kind == domain.AssetKindUnknown) {
				return fmt.Errorf("真实副本资产类型不匹配: %s", relative)
			}
			return nil
		}
		path, err := realShapeReadOnlyPath(root, relative)
		if err != nil {
			return fmt.Errorf("读取真实副本资产 %s 失败: %w", relative, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		digest, err := realShapeHashFile(ctx, path)
		if err != nil {
			return err
		}
		assetsByPath[relative] = MutationAsset{RelativePath: relative, Kind: kind, SizeBytes: info.Size(), SHA256: digest, Status: domain.StatusStable}
		return nil
	}
	for _, databaseName := range realShapeDatabases {
		if err := addRequired(databaseName, domain.AssetKindSQLite); err != nil {
			return nil, err
		}
	}
	if err := addRequired(shape.RolloutPath, domain.AssetKindJSONL); err != nil {
		return nil, err
	}
	if len(shape.SessionIndexLines) > 0 {
		if err := addRequired(realShapeSessionIndex, domain.AssetKindSessionIndex); err != nil {
			return nil, err
		}
	}
	if err := addRequired(realShapeCopyMarker, domain.AssetKindUnknown); err != nil {
		return nil, err
	}
	assets := make([]MutationAsset, 0, len(assetsByPath))
	for _, asset := range assetsByPath {
		assets = append(assets, asset)
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].RelativePath < assets[j].RelativePath })
	return assets, nil
}

func mutationAssetsEqual(left, right []MutationAsset) bool {
	canonical := func(values []MutationAsset) []MutationAsset {
		copyOf := append([]MutationAsset(nil), values...)
		sort.Slice(copyOf, func(i, j int) bool { return copyOf[i].RelativePath < copyOf[j].RelativePath })
		return copyOf
	}
	leftBytes, leftErr := json.Marshal(canonical(left))
	rightBytes, rightErr := json.Marshal(canonical(right))
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}

func sortedSQLiteDatabaseNames(groups map[string][]sqliteRowSnapshot) []string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// isRetainedRealShapeCopyRow 保留由 queued_items 删除触发器维护的 revision 元数据。
func isRetainedRealShapeCopyRow(row sqliteRowSnapshot) bool {
	return row.Database == "queue_1.sqlite" && row.Table == "queued_thread_revisions"
}

func groupRealShapeCopyRows(rows []sqliteRowSnapshot, restore bool) map[string][]sqliteRowSnapshot {
	groups := make(map[string][]sqliteRowSnapshot)
	for _, row := range rows {
		groups[row.Database] = append(groups[row.Database], row)
	}
	for database := range groups {
		sort.Slice(groups[database], func(i, j int) bool {
			left, _ := sqliteRowKey(groups[database][i])
			right, _ := sqliteRowKey(groups[database][j])
			if restore {
				return left < right
			}
			return left > right
		})
	}
	return groups
}

// preflightRealShapeCopy 在任何索引或数据库写入前检查整组快照，减少跨库局部失败。
func preflightRealShapeCopy(ctx context.Context, root string, payload sqliteMutationPayload, restore bool) error {
	for _, index := range payload.Indexes {
		path, err := fixturePath(root, index.RelativePath)
		if err != nil {
			return err
		}
		if err := ensureRegularFixturePath(path); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("读取真实副本 session index 失败: %w", err)
		}
		current := hashBytes(data)
		if restore {
			if current != index.AfterSHA256 && current != index.OriginalSHA256 {
				return fmt.Errorf("恢复 session index 冲突：%s", index.RelativePath)
			}
		} else if current != index.OriginalSHA256 && current != index.AfterSHA256 {
			return fmt.Errorf("删除前 session index 已变化：%s", index.RelativePath)
		}
	}
	rowsByDatabase := groupRealShapeCopyRows(payload.Rows, restore)
	for _, databaseName := range sortedSQLiteDatabaseNames(rowsByDatabase) {
		path, err := fixturePath(root, databaseName)
		if err != nil {
			return err
		}
		database, err := openFixtureSQLite(ctx, path, true)
		if err != nil {
			return err
		}
		for _, row := range rowsByDatabase[databaseName] {
			values, exists, readErr := readSQLiteRow(ctx, database, row)
			if readErr != nil {
				_ = database.Close()
				return readErr
			}
			if restore {
				if exists && !sqlValuesEqual(values, row.Values) {
					_ = database.Close()
					return fmt.Errorf("恢复冲突：%s.%s 的现有行已变化", row.Database, row.Table)
				}
				continue
			}
			if !exists {
				_ = database.Close()
				return fmt.Errorf("删除前 SQLite 行已不存在：%s.%s", row.Database, row.Table)
			}
			if !sqlValuesEqual(values, row.Values) {
				_ = database.Close()
				return fmt.Errorf("删除前 SQLite 行已变化：%s.%s", row.Database, row.Table)
			}
		}
		if err := database.Close(); err != nil {
			return err
		}
	}
	return nil
}

func realCopyUnsupportedAction(action MutationAction) error {
	return &MutationError{Code: "MUTATION_ACTION_UNSUPPORTED", Capability: CapabilityDelete, Message: fmt.Sprintf("真实副本 adapter 不支持动作：%s", action), ReadOnly: true}
}
