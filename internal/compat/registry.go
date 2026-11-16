// 中文说明：按观测到的 Codex 版本和 SQLite schema 指纹解析兼容 adapter，未知输入自动降级为只读。
package compat

import (
	"sort"
	"strings"

	"codex-chronicle.local/internal/domain"
)

// FormatIdentity 是一次只读探针得到的格式身份；任一关键字段缺失都不能推导写能力。
type FormatIdentity struct {
	Version           string `json:"version,omitempty"`
	SchemaFingerprint string `json:"schema_fingerprint,omitempty"`
	// SchemaFingerprints 用于一个 Codex 版本同时携带多个 SQLite 数据库时的可解释身份。
	// SchemaFingerprint 保留单数据库场景的向后兼容字段。
	SchemaFingerprints []string `json:"schema_fingerprints,omitempty"`
}

// AdapterResolution 是 registry 对一次格式匹配的可解释结果。
type AdapterResolution struct {
	Identity   FormatIdentity `json:"identity"`
	Descriptor Descriptor     `json:"descriptor"`
	Matched    bool           `json:"matched"`
	Reason     string         `json:"reason"`
}

// RegistryHealth 是兼容 registry 自身的静态契约检查结果，不会探测或修改源目录。
type RegistryHealth struct {
	Valid           bool     `json:"valid"`
	ReadOnly        bool     `json:"read_only"`
	DescriptorCount int      `json:"descriptor_count"`
	Errors          []string `json:"errors,omitempty"`
}

// Registry 保存显式注册的版本化 descriptor；它不自动把未知版本映射到最近版本。
type Registry struct {
	descriptors []Descriptor
}

// NewRegistry 创建不可变视图；调用方后续修改传入切片不会影响 registry。
func NewRegistry(descriptors ...Descriptor) Registry {
	copyOf := make([]Descriptor, len(descriptors))
	for index, descriptor := range descriptors {
		copyOf[index] = cloneDescriptor(descriptor)
	}
	return Registry{descriptors: copyOf}
}

// DefaultRegistry 注册合成 fixture 和已有脱敏版本样本；真实写能力仍全部关闭。
func DefaultRegistry() Registry {
	return NewRegistry(SyntheticFixtureDescriptor(), ObservedReadonlyDescriptor())
}

// Health 检查 adapter 声明是否自洽；不自洽的 registry 必须阻断更新激活和写操作。
func (r Registry) Health() RegistryHealth {
	result := RegistryHealth{Valid: true, ReadOnly: true, DescriptorCount: len(r.descriptors), Errors: make([]string, 0)}
	seenIDs := make(map[string]struct{}, len(r.descriptors))
	for _, descriptor := range r.descriptors {
		id := strings.TrimSpace(descriptor.ID)
		if id == "" {
			result.Errors = append(result.Errors, "adapter ID 不能为空")
			continue
		}
		if _, exists := seenIDs[id]; exists {
			result.Errors = append(result.Errors, "adapter ID 重复："+id)
		}
		seenIDs[id] = struct{}{}
		if len(nonEmptyValues(descriptor.SupportedVersions)) == 0 {
			result.Errors = append(result.Errors, "adapter "+id+" 未声明支持版本")
		}
		if !descriptor.Capabilities[CapabilityRead] {
			result.Errors = append(result.Errors, "adapter "+id+" 未声明 read 能力")
		}
		if descriptor.ReadOnlyReason != "" && descriptorDeclaresWriteCapability(descriptor) {
			result.Errors = append(result.Errors, "adapter "+id+" 同时声明只读原因和写能力")
		}
		if contains(descriptor.SupportedVersions, "unknown") && descriptorDeclaresWriteCapability(descriptor) {
			result.Errors = append(result.Errors, "adapter "+id+" 不能为 unknown 版本声明写能力")
		}
	}
	if len(result.Errors) != 0 {
		result.Valid = false
	}
	return result
}

func nonEmptyValues(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// Resolve 精确匹配版本和 schema；未命中时返回带原因的只读 descriptor。
func (r Registry) Resolve(identity FormatIdentity) AdapterResolution {
	version := strings.TrimSpace(identity.Version)
	schemas := identitySchemas(identity)
	if version == "" {
		resolution := readonlyResolution("未提供 Codex 版本，无法授权版本化 adapter")
		resolution.Identity = identity
		return resolution
	}
	for _, descriptor := range r.descriptors {
		if !contains(descriptor.SupportedVersions, version) {
			continue
		}
		if !descriptorMatchesSchemas(descriptor, schemas) {
			continue
		}
		return AdapterResolution{Identity: identity, Descriptor: cloneDescriptor(descriptor), Matched: true, Reason: "版本和 schema 指纹均精确匹配"}
	}
	if len(schemas) == 0 {
		resolution := readonlyResolution("未提供 SQLite schema 指纹，无法授权版本化 adapter")
		resolution.Identity = identity
		return resolution
	}
	resolution := readonlyResolution("未找到同时匹配版本和 schema 指纹的 adapter")
	resolution.Identity = identity
	return resolution
}

// ResolveInventory 根据一次只读 inventory 中观测到的版本和 schema 指纹解析 adapter。
// 工作区包含多个不一致身份时直接降级，避免把一个版本的写能力套到另一版本。
func ResolveInventory(inventory domain.InventoryReport, registry Registry) AdapterResolution {
	versions := make(map[string]struct{})
	schemas := make(map[string]struct{})
	for _, file := range inventory.Files {
		if file.Rollout != nil && strings.TrimSpace(file.Rollout.CLIVersion) != "" {
			versions[strings.TrimSpace(file.Rollout.CLIVersion)] = struct{}{}
		}
		if file.SQLite != nil && strings.TrimSpace(file.SQLite.SchemaFingerprint) != "" {
			schemas[strings.TrimSpace(file.SQLite.SchemaFingerprint)] = struct{}{}
		}
		if file.SQLite != nil {
			for _, thread := range file.SQLite.Threads {
				if strings.TrimSpace(thread.CLIVersion) != "" {
					versions[strings.TrimSpace(thread.CLIVersion)] = struct{}{}
				}
			}
		}
	}
	identity := FormatIdentity{}
	if len(versions) == 1 {
		for version := range versions {
			identity.Version = version
		}
	}
	if len(schemas) == 1 {
		for schema := range schemas {
			identity.SchemaFingerprint = schema
		}
	} else if len(schemas) > 1 {
		identity.SchemaFingerprints = sortedSet(schemas)
	}
	if len(versions) > 1 {
		resolution := readonlyResolution("一次 inventory 观测到多个 Codex 版本，不能选择单一 adapter")
		resolution.Identity = identity
		return resolution
	}
	if len(identity.SchemaFingerprints) == 0 {
		return registry.Resolve(identity)
	}
	for _, descriptor := range registry.descriptors {
		if !contains(descriptor.SupportedVersions, identity.Version) || !descriptorMatchesSchemas(descriptor, identity.SchemaFingerprints) {
			continue
		}
		return AdapterResolution{Identity: identity, Descriptor: cloneDescriptor(descriptor), Matched: true, Reason: "版本和 schema 指纹集合均精确匹配"}
	}
	resolution := readonlyResolution("未找到同时匹配版本和 schema 指纹集合的 adapter")
	resolution.Identity = identity
	return resolution
}

func readonlyResolution(reason string) AdapterResolution {
	descriptor := GenericJSONLDescriptor()
	descriptor.ReadOnlyReason = reason
	return AdapterResolution{Descriptor: descriptor, Matched: false, Reason: reason}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == wanted {
			return true
		}
	}
	return false
}

func identitySchemas(identity FormatIdentity) []string {
	values := append([]string(nil), identity.SchemaFingerprints...)
	if strings.TrimSpace(identity.SchemaFingerprint) != "" {
		values = append(values, identity.SchemaFingerprint)
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return sortedSet(set)
}

func schemaSetContained(allowed, observed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	if len(observed) == 0 {
		return false
	}
	for _, schema := range observed {
		if !contains(allowed, schema) {
			return false
		}
	}
	return true
}

func descriptorMatchesSchemas(descriptor Descriptor, observed []string) bool {
	if len(descriptor.SchemaFingerprints) > 0 {
		return schemaSetContained(descriptor.SchemaFingerprints, observed)
	}
	// 多库工作区没有 schema 白名单时，只允许明确只读 descriptor 命中。
	// 这样兼容层不会把一个宽泛的写 adapter 误套到未知数据库集合。
	if len(observed) > 1 && descriptorHasWriteCapability(descriptor) {
		return false
	}
	return true
}

func descriptorHasWriteCapability(descriptor Descriptor) bool {
	return descriptorDeclaresWriteCapability(descriptor) && descriptor.ReadOnlyReason == "" && descriptor.hasKnownVersion()
}

func descriptorDeclaresWriteCapability(descriptor Descriptor) bool {
	for _, capability := range []Capability{CapabilityRepair, CapabilityArchiveExport, CapabilityDelete, CapabilityRestore, CapabilityRebuildIndex} {
		if descriptor.Capabilities[capability] {
			return true
		}
	}
	return false
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	clone := descriptor
	clone.SupportedVersions = append([]string(nil), descriptor.SupportedVersions...)
	clone.SchemaFingerprints = append([]string(nil), descriptor.SchemaFingerprints...)
	clone.FixtureVersions = append([]string(nil), descriptor.FixtureVersions...)
	clone.SharedAssetKinds = append([]domain.AssetKind(nil), descriptor.SharedAssetKinds...)
	clone.KnownLimits = append([]string(nil), descriptor.KnownLimits...)
	clone.Capabilities = make(map[Capability]bool, len(descriptor.Capabilities))
	for capability, enabled := range descriptor.Capabilities {
		clone.Capabilities[capability] = enabled
	}
	return clone
}
