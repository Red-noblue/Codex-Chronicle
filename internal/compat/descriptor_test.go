// 中文说明：验证未知或通用版本的兼容层默认拒绝写能力。
package compat

import "testing"

func TestGenericJSONLDescriptorIsReadOnly(t *testing.T) {
	descriptor := GenericJSONLDescriptor()
	if !descriptor.Capabilities[CapabilityRead] || !descriptor.Capabilities[CapabilityInspect] {
		t.Fatalf("generic descriptor must support read and inspect: %+v", descriptor)
	}
	for _, capability := range []Capability{
		CapabilityRepair,
		CapabilityArchiveExport,
		CapabilityDelete,
		CapabilityRestore,
		CapabilityRebuildIndex,
	} {
		if descriptor.AllowsWrite(capability) {
			t.Fatalf("generic descriptor unexpectedly allows %q", capability)
		}
	}
}

func TestSyntheticFixtureDescriptorIsExplicitlyWritable(t *testing.T) {
	descriptor := SyntheticFixtureDescriptor()
	if descriptor.ID == "" || descriptor.ReadOnlyReason != "" || !descriptor.AllowsWrite(CapabilityArchiveExport) || !descriptor.AllowsWrite(CapabilityDelete) {
		t.Fatalf("unexpected fixture descriptor: %+v", descriptor)
	}
}

func TestDescriptorWithoutKnownVersionCannotWrite(t *testing.T) {
	for _, descriptor := range []Descriptor{
		{ID: "missing-version", Capabilities: map[Capability]bool{CapabilityDelete: true}},
		{ID: "unknown-version", SupportedVersions: []string{"unknown"}, Capabilities: map[Capability]bool{CapabilityDelete: true}},
	} {
		if descriptor.AllowsWrite(CapabilityDelete) {
			t.Fatalf("descriptor without a known version unexpectedly writable: %+v", descriptor)
		}
	}
}
