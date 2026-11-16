// 中文说明：验证版本和 schema 精确匹配，以及未知输入自动降级为只读。
package compat

import (
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestRegistryRequiresExactVersionAndSchema(t *testing.T) {
	descriptor := Descriptor{
		ID: "codex-fixture-v2", SupportedVersions: []string{"0.9.0"}, SchemaFingerprints: []string{"schema-a"},
		Capabilities: map[Capability]bool{CapabilityRead: true, CapabilityDelete: true},
	}
	registry := NewRegistry(descriptor)
	matched := registry.Resolve(FormatIdentity{Version: "0.9.0", SchemaFingerprint: "schema-a"})
	if !matched.Matched || matched.Descriptor.ID != descriptor.ID || !matched.Descriptor.AllowsWrite(CapabilityDelete) {
		t.Fatalf("expected exact writable match: %+v", matched)
	}

	for _, identity := range []FormatIdentity{
		{Version: "0.9.1", SchemaFingerprint: "schema-a"},
		{Version: "0.9.0", SchemaFingerprint: "schema-b"},
		{Version: "0.9.0"},
		{},
	} {
		resolved := registry.Resolve(identity)
		if resolved.Matched || resolved.Descriptor.AllowsWrite(CapabilityDelete) || resolved.Descriptor.ReadOnlyReason == "" {
			t.Fatalf("unknown identity was not downgraded: identity=%+v result=%+v", identity, resolved)
		}
	}
}

func TestDefaultRegistryOnlyResolvesSyntheticFixture(t *testing.T) {
	registry := DefaultRegistry()
	fixture := registry.Resolve(FormatIdentity{Version: "fixture-v1"})
	if !fixture.Matched || fixture.Descriptor.ID != "synthetic-fixture-v1" {
		t.Fatalf("synthetic fixture was not resolved: %+v", fixture)
	}
	unknown := registry.Resolve(FormatIdentity{Version: "codex-unknown", SchemaFingerprint: "schema"})
	if unknown.Matched || unknown.Descriptor.ID != "generic-jsonl-readonly" || unknown.Descriptor.AllowsWrite(CapabilityArchiveExport) {
		t.Fatalf("unknown version escaped readonly fallback: %+v", unknown)
	}
	for _, version := range []string{"0.164.0", "0.165.0", "0.166.0"} {
		observed := registry.Resolve(FormatIdentity{Version: version, SchemaFingerprint: "unverified-schema"})
		if !observed.Matched || observed.Descriptor.ID != "codex-observed-readonly" || observed.Descriptor.AllowsWrite(CapabilityDelete) || observed.Descriptor.AllowsWrite(CapabilityRepair) || observed.Descriptor.AllowsWrite(CapabilityArchiveExport) {
			t.Fatalf("observed version did not remain explicitly readonly: version=%s result=%+v", version, observed)
		}
	}
	realInventory := domain.InventoryReport{Files: []domain.FileInventory{
		{Rollout: &domain.RolloutSummary{CLIVersion: "0.166.0"}},
		{SQLite: &domain.SQLiteSummary{SchemaFingerprint: "real-state-schema"}},
		{SQLite: &domain.SQLiteSummary{SchemaFingerprint: "real-history-schema"}},
	}}
	observedInventory := ResolveInventory(realInventory, registry)
	if !observedInventory.Matched || observedInventory.Descriptor.ID != "codex-observed-readonly" || observedInventory.Descriptor.AllowsWrite(CapabilityDelete) {
		t.Fatalf("observed version with multiple schemas did not resolve readonly adapter: %+v", observedInventory)
	}
}

func TestRegistryHealthRejectsContradictoryAdapterDeclarations(t *testing.T) {
	registry := NewRegistry(
		Descriptor{ID: "valid-read", SupportedVersions: []string{"1.0"}, Capabilities: map[Capability]bool{CapabilityRead: true}},
		Descriptor{ID: "unsafe", SupportedVersions: []string{"unknown"}, ReadOnlyReason: "只读", Capabilities: map[Capability]bool{CapabilityRead: true, CapabilityDelete: true}},
		Descriptor{ID: "unsafe", SupportedVersions: []string{"1.1"}, Capabilities: map[Capability]bool{CapabilityDelete: true}},
	)
	health := registry.Health()
	if health.Valid || health.ReadOnly == false || health.DescriptorCount != 3 || len(health.Errors) < 3 {
		t.Fatalf("invalid registry was accepted: %+v", health)
	}
	valid := DefaultRegistry().Health()
	if !valid.Valid || !valid.ReadOnly || len(valid.Errors) != 0 {
		t.Fatalf("default registry health failed: %+v", valid)
	}
}

func TestRegistryDeepCopiesDescriptors(t *testing.T) {
	descriptor := Descriptor{
		ID: "immutable", SupportedVersions: []string{"1.0"},
		Capabilities: map[Capability]bool{CapabilityRead: true},
	}
	registry := NewRegistry(descriptor)
	descriptor.SupportedVersions[0] = "changed"
	descriptor.Capabilities[CapabilityDelete] = true
	resolved := registry.Resolve(FormatIdentity{Version: "1.0", SchemaFingerprint: "unused"})
	if !resolved.Matched || resolved.Descriptor.SupportedVersions[0] != "1.0" || resolved.Descriptor.Capabilities[CapabilityDelete] {
		t.Fatalf("registry was changed through caller-owned descriptor: %+v", resolved)
	}
	resolved.Descriptor.Capabilities[CapabilityDelete] = true
	second := registry.Resolve(FormatIdentity{Version: "1.0", SchemaFingerprint: "unused"})
	if second.Descriptor.Capabilities[CapabilityDelete] {
		t.Fatal("resolved descriptor leaked mutable capabilities map")
	}
}

func TestResolveInventoryCollectsVersionAndSchemaIdentity(t *testing.T) {
	registry := NewRegistry(Descriptor{
		ID: "fixture-with-schema", SupportedVersions: []string{"fixture-v2"}, SchemaFingerprints: []string{"schema-a"},
		Capabilities: map[Capability]bool{CapabilityRead: true, CapabilityDelete: true},
	})
	report := domain.InventoryReport{Files: []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "rollout.jsonl"}, Rollout: &domain.RolloutSummary{CLIVersion: "fixture-v2"}},
		{Asset: domain.Asset{RelativePath: "state.sqlite"}, SQLite: &domain.SQLiteSummary{SchemaFingerprint: "schema-a"}},
	}}
	resolution := ResolveInventory(report, registry)
	if !resolution.Matched || resolution.Identity.Version != "fixture-v2" || resolution.Identity.SchemaFingerprint != "schema-a" || resolution.Descriptor.ID != "fixture-with-schema" {
		t.Fatalf("inventory identity was not resolved exactly: %+v", resolution)
	}
}

func TestResolveInventoryDowngradesMixedIdentities(t *testing.T) {
	report := domain.InventoryReport{Files: []domain.FileInventory{
		{Rollout: &domain.RolloutSummary{CLIVersion: "fixture-v1"}},
		{Rollout: &domain.RolloutSummary{CLIVersion: "fixture-v2"}},
	}}
	resolution := ResolveInventory(report, DefaultRegistry())
	if resolution.Matched || resolution.Descriptor.AllowsWrite(CapabilityDelete) || resolution.Reason == "" {
		t.Fatalf("mixed versions escaped readonly resolution: %+v", resolution)
	}
}

func TestResolveInventoryMatchesMultipleSchemasForOneVersion(t *testing.T) {
	registry := NewRegistry(Descriptor{
		ID: "codex-real-readonly", SupportedVersions: []string{"0.166.0"},
		SchemaFingerprints: []string{"schema-state", "schema-history", "schema-queue"},
		Capabilities:       map[Capability]bool{CapabilityRead: true, CapabilityInspect: true},
	})
	report := domain.InventoryReport{Files: []domain.FileInventory{
		{Rollout: &domain.RolloutSummary{CLIVersion: "0.166.0"}},
		{Asset: domain.Asset{RelativePath: "state_5.sqlite"}, SQLite: &domain.SQLiteSummary{SchemaFingerprint: "schema-state"}},
		{Asset: domain.Asset{RelativePath: "thread_history_1.sqlite"}, SQLite: &domain.SQLiteSummary{SchemaFingerprint: "schema-history"}},
		{Asset: domain.Asset{RelativePath: "queue_1.sqlite"}, SQLite: &domain.SQLiteSummary{SchemaFingerprint: "schema-queue"}},
	}}
	resolution := ResolveInventory(report, registry)
	if !resolution.Matched || resolution.Descriptor.ID != "codex-real-readonly" || len(resolution.Identity.SchemaFingerprints) != 3 {
		t.Fatalf("same version with known schema set did not resolve: %+v", resolution)
	}
	if resolution.Identity.SchemaFingerprint != "" || resolution.Descriptor.AllowsWrite(CapabilityDelete) {
		t.Fatalf("multi-schema identity escaped readonly boundary: %+v", resolution)
	}
}

func TestResolveInventoryRejectsUnknownSchemaInKnownVersion(t *testing.T) {
	registry := NewRegistry(Descriptor{
		ID: "codex-real-readonly", SupportedVersions: []string{"0.166.0"},
		SchemaFingerprints: []string{"schema-state", "schema-history"},
		Capabilities:       map[Capability]bool{CapabilityRead: true, CapabilityDelete: true},
	})
	report := domain.InventoryReport{Files: []domain.FileInventory{
		{Rollout: &domain.RolloutSummary{CLIVersion: "0.166.0"}},
		{SQLite: &domain.SQLiteSummary{SchemaFingerprint: "schema-state"}},
		{SQLite: &domain.SQLiteSummary{SchemaFingerprint: "schema-new"}},
	}}
	resolution := ResolveInventory(report, registry)
	if resolution.Matched || resolution.Descriptor.AllowsWrite(CapabilityDelete) || resolution.Reason == "" {
		t.Fatalf("unknown schema escaped readonly fallback: %+v", resolution)
	}
}

func TestResolveInventoryRejectsWritableAdapterWithoutSchemaSet(t *testing.T) {
	registry := NewRegistry(Descriptor{
		ID: "unsafe-broad-writer", SupportedVersions: []string{"0.166.0"},
		Capabilities: map[Capability]bool{CapabilityRead: true, CapabilityDelete: true},
	})
	report := domain.InventoryReport{Files: []domain.FileInventory{
		{Rollout: &domain.RolloutSummary{CLIVersion: "0.166.0"}},
		{SQLite: &domain.SQLiteSummary{SchemaFingerprint: "schema-a"}},
		{SQLite: &domain.SQLiteSummary{SchemaFingerprint: "schema-b"}},
	}}
	resolution := ResolveInventory(report, registry)
	if resolution.Matched || resolution.Descriptor.AllowsWrite(CapabilityDelete) {
		t.Fatalf("broad writable adapter escaped multi-schema guard: %+v", resolution)
	}
}
