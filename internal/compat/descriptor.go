// 中文说明：定义兼容层的能力声明，默认拒绝所有破坏性 Codex 写操作。
package compat

import (
	"strings"

	"codex-chronicle.local/internal/domain"
)

// Capability 表示 adapter 对资产可以提供的能力。
type Capability string

const (
	CapabilityRead          Capability = "read"
	CapabilityInspect       Capability = "inspect"
	CapabilitySearch        Capability = "search_projection"
	CapabilityRepair        Capability = "repair"
	CapabilityArchiveExport Capability = "archive_export"
	CapabilityDelete        Capability = "delete"
	CapabilityRestore       Capability = "restore"
	CapabilityRebuildIndex  Capability = "rebuild_index"
	CapabilityActivityProbe Capability = "activity_probe"
	CapabilitySnapshot      Capability = "snapshot"
	CapabilityVerify        Capability = "verify"
)

// Descriptor 是 adapter 的静态能力描述；未知版本不得伪造写能力。
type Descriptor struct {
	ID                 string              `json:"id"`
	SupportedVersions  []string            `json:"supported_versions"`
	SchemaFingerprints []string            `json:"schema_fingerprints"`
	FixtureVersions    []string            `json:"fixture_versions,omitempty"`
	Capabilities       map[Capability]bool `json:"capabilities"`
	// SharedAssetKinds 声明 adapter 已实现安全的局部拆分；未声明的共享资产仍阻断写入。
	SharedAssetKinds []domain.AssetKind `json:"shared_asset_kinds,omitempty"`
	KnownLimits      []string           `json:"known_limits,omitempty"`
	ReadOnlyReason   string             `json:"read_only_reason,omitempty"`
}

// AllowsSharedAssetSplit 判断共享文件是否可以按 adapter 已验证的边界局部修改。
func (d Descriptor) AllowsSharedAssetSplit(kind domain.AssetKind) bool {
	for _, allowed := range d.SharedAssetKinds {
		if allowed == kind {
			return true
		}
	}
	return false
}

// GenericJSONLDescriptor 返回仅能读取和诊断通用 JSONL 的保守 descriptor。
func GenericJSONLDescriptor() Descriptor {
	return Descriptor{
		ID:                "generic-jsonl-readonly",
		SupportedVersions: []string{"unknown"},
		KnownLimits:       []string{"仅解析通用 JSONL；不读取 Codex SQLite 关系，不提供 repair/delete/restore"},
		Capabilities: map[Capability]bool{
			CapabilityRead:          true,
			CapabilityInspect:       true,
			CapabilitySearch:        true,
			CapabilityActivityProbe: true,
			CapabilitySnapshot:      true,
			CapabilityVerify:        true,
			CapabilityRepair:        false,
			CapabilityArchiveExport: false,
			CapabilityDelete:        false,
			CapabilityRestore:       false,
			CapabilityRebuildIndex:  false,
		},
		ReadOnlyReason: "通用 JSONL adapter 未验证 Codex 版本和 SQLite 关系，只允许读取与诊断",
	}
}

// SyntheticFixtureDescriptor 仅用于自行创建的副本/fixture 回归，不代表任何真实 Codex 版本。
func SyntheticFixtureDescriptor() Descriptor {
	return Descriptor{
		ID:                "synthetic-fixture-v1",
		SupportedVersions: []string{"fixture-v1"},
		FixtureVersions:   []string{"fixture-v1"},
		KnownLimits:       []string{"只允许由调用方创建的合成 fixture；不代表任何真实 Codex 版本"},
		Capabilities: map[Capability]bool{
			CapabilityRead: true, CapabilityInspect: true, CapabilitySearch: true,
			CapabilityArchiveExport: true, CapabilityDelete: true, CapabilityRestore: true,
			CapabilityVerify: true, CapabilityActivityProbe: true, CapabilitySnapshot: true,
		},
		SharedAssetKinds: []domain.AssetKind{domain.AssetKindSQLite, domain.AssetKindSessionIndex},
	}
}

// ObservedReadonlyDescriptor 列出本机脱敏结构样本中出现过的 CLI 版本。
// 这些样本只证明读取投影和诊断形状，不证明任何真实写入语义。
func ObservedReadonlyDescriptor() Descriptor {
	return Descriptor{
		ID: "codex-observed-readonly",
		SupportedVersions: []string{
			"0.41.0", "0.106.0", "0.114.0", "0.115.0", "0.116.0", "0.118.0", "0.120.0", "0.121.0",
			"0.128.0", "0.139.0", "0.142.4", "0.142.5", "0.145.0", "0.146.0", "0.147.0", "0.148.0",
			"0.149.1", "0.150.1", "0.151.0", "0.152.1", "0.153.0", "0.154.0", "0.162.1", "0.163.0", "0.164.0", "0.165.0", "0.166.0",
		},
		KnownLimits: []string{
			"版本来自脱敏结构样本，只授权读取、诊断和搜索投影",
			"未验证真实数据库迁移、活动 writer、WAL 和生命周期写入语义",
		},
		Capabilities: map[Capability]bool{
			CapabilityRead: true, CapabilityInspect: true, CapabilitySearch: true,
			CapabilityActivityProbe: true, CapabilitySnapshot: true, CapabilityVerify: true,
			CapabilityRepair: false, CapabilityArchiveExport: false, CapabilityDelete: false,
			CapabilityRestore: false, CapabilityRebuildIndex: false,
		},
		ReadOnlyReason: "当前版本只有脱敏结构样本证据，尚未授权 Codex 写操作",
	}
}

// AllowsWrite 只有明确声明、可识别且不是 unknown 的版本才可能返回 true。
func (d Descriptor) AllowsWrite(capability Capability) bool {
	if d.ReadOnlyReason != "" || strings.TrimSpace(d.ID) == "" || !d.hasKnownVersion() {
		return false
	}
	return d.Capabilities[capability]
}

func (d Descriptor) hasKnownVersion() bool {
	for _, version := range d.SupportedVersions {
		version = strings.TrimSpace(version)
		if version != "" && version != "unknown" {
			return true
		}
	}
	return false
}
