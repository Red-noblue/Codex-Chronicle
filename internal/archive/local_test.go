// 中文说明：验证本地归档只复制稳定资产、写入 manifest，并能发现归档篡改。
package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

func writableDescriptorForArchive() compat.Descriptor {
	return compat.Descriptor{ID: "fixture-writable", SupportedVersions: []string{"fixture-v1"}, Capabilities: map[compat.Capability]bool{
		compat.CapabilityRead: true, compat.CapabilityArchiveExport: true,
		compat.CapabilityDelete: true, compat.CapabilityVerify: true,
	}}
}

func TestCreateAndVerifyLocalArchive(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "archives")
	result, err := CreateLocal(context.Background(), plan, destination)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || !result.SourceUnchanged || result.AssetCount != 1 {
		t.Fatalf("unexpected archive result: %+v", result)
	}
	manifest, err := VerifyLocal(context.Background(), result.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.OperationID != plan.OperationID || len(manifest.Assets) != 1 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if _, err := CreateLocal(context.Background(), plan, destination); err == nil {
		t.Fatal("expected existing archive collision")
	}
}

func TestLocalArchiveClassifiesTypedControlPlaneAssetEvenWhenFlagMissing(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	statePath := filepath.Join(root, "state.sqlite")
	if err := os.WriteFile(rolloutPath, []byte("archive fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("control plane placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rolloutInfo, err := os.Stat(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	stateInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	plan := lifecycle.OperationPlan{
		OperationID: "typed-control-plane-archive", Root: root, ConversationID: "archive-conversation", Status: lifecycle.PlanPrepared,
		Assets: []lifecycle.AssetFingerprint{
			{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: rolloutInfo.Size(), ModTime: rolloutInfo.ModTime().UTC(), SHA256: hashFileForArchiveTest(t, rolloutPath)},
			{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: stateInfo.Size(), ModTime: stateInfo.ModTime().UTC(), SHA256: hashFileForArchiveTest(t, statePath)},
		},
	}
	result, err := CreateLocal(context.Background(), plan, filepath.Join(t.TempDir(), "archives"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := VerifyLocal(context.Background(), result.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 1 || manifest.Assets[0].RelativePath != "rollout.jsonl" || len(manifest.ControlPlaneAssets) != 1 || manifest.ControlPlaneAssets[0].RelativePath != "state.sqlite" {
		t.Fatalf("typed control-plane asset was misclassified: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(result.Directory, "payload", "state.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("typed control-plane asset was copied as a file payload: %v", err)
	}
}

func hashFileForArchiveTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func TestVerifyLocalRejectsTamperedPayload(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := CreateLocal(context.Background(), plan, filepath.Join(t.TempDir(), "archives"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(result.Directory, "payload", "rollout.jsonl")
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLocal(context.Background(), result.Directory); err == nil {
		t.Fatal("expected tampered archive rejection")
	}
}

func TestCreateAndVerifyEncryptedLocalArchive(t *testing.T) {
	root, conversation, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true, Encrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := CreateLocalEncrypted(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), "正确的密码")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || !result.Encrypted {
		t.Fatalf("unexpected encrypted archive result: %+v", result)
	}
	payload, err := os.ReadFile(filepath.Join(result.Directory, "payload", "rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) == "archive fixture\n" {
		t.Fatal("encrypted payload unexpectedly contains plaintext")
	}
	if _, err := VerifyLocal(context.Background(), result.Directory); err == nil {
		t.Fatal("expected plaintext verifier to reject encrypted archive")
	}
	manifest, err := VerifyLocalEncrypted(context.Background(), result.Directory, "正确的密码")
	if err != nil || !manifest.Encrypted || manifest.Encryption == nil {
		t.Fatalf("encrypted verification failed: manifest=%+v err=%v", manifest, err)
	}
	if _, err := VerifyLocalEncrypted(context.Background(), result.Directory, "错误的密码"); err == nil {
		t.Fatal("expected wrong password rejection")
	}
}

func TestLocalArchiveIncludesAndRestoresControlPlaneSnapshot(t *testing.T) {
	root := t.TempDir()
	rolloutPath := filepath.Join(root, "rollout.jsonl")
	statePath := filepath.Join(root, "state.sqlite")
	if err := os.WriteFile(rolloutPath, []byte("control archive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("state bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rolloutInfo, err := os.Stat(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	stateInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	stateAsset := lifecycle.AssetFingerprint{
		RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, ControlPlaneOnly: true,
		SizeBytes: stateInfo.Size(), ModTime: stateInfo.ModTime().UTC(), SHA256: hashFileForArchiveTest(t, statePath),
	}
	plan := lifecycle.OperationPlan{
		OperationID: "control-plane-archive", Root: root, ConversationID: "control-conversation", AssetSetHash: "control-assets-v1", Status: lifecycle.PlanPrepared,
		Assets: []lifecycle.AssetFingerprint{
			{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: rolloutInfo.Size(), ModTime: rolloutInfo.ModTime().UTC(), SHA256: hashFileForArchiveTest(t, rolloutPath)},
			stateAsset,
		},
	}
	snapshotPath := filepath.Join(t.TempDir(), "mutation.json")
	snapshotPayload := []byte(`{"version":1,"adapter_id":"fixture-adapter-v1","conversation_id":"control-conversation"}` + "\n")
	if err := os.WriteFile(snapshotPath, snapshotPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotDigest := fmt.Sprintf("%x", sha256.Sum256(bytes.TrimSuffix(snapshotPayload, []byte("\n"))))
	snapshot, err := compat.BuildMutationSnapshot(context.Background(), "fixture-adapter-v1", compat.MutationRequest{
		Root: root, ConversationID: plan.ConversationID, AssetSetHash: plan.AssetSetHash, SnapshotRef: snapshotPath, SnapshotSHA256: snapshotDigest,
		Assets: []compat.MutationAsset{{RelativePath: stateAsset.RelativePath, Kind: stateAsset.Kind, SizeBytes: stateAsset.SizeBytes, SHA256: stateAsset.SHA256, Status: domain.StatusStable}},
	})
	if err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "archives")
	result, err := CreateLocalWithMutationSnapshot(context.Background(), plan, destination, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := VerifyLocal(context.Background(), result.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ControlPlaneSnapshot == nil || manifest.ControlPlaneSnapshot.AdapterID != snapshot.AdapterID || manifest.ControlPlaneSnapshot.SnapshotHash != snapshot.SnapshotHash {
		t.Fatalf("控制面快照未写入 manifest: %+v", manifest.ControlPlaneSnapshot)
	}
	archivedPayload := filepath.Join(result.Directory, "payload", filepath.FromSlash(manifest.ControlPlaneSnapshot.PayloadPath))
	data, err := os.ReadFile(archivedPayload)
	if err != nil || string(data) != string(snapshotPayload) {
		t.Fatalf("控制面快照 payload 不完整: %q err=%v", data, err)
	}
	if err := os.Remove(snapshotPath); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMutationSnapshot(context.Background(), result.Directory, snapshot); err != nil {
		t.Fatalf("无法从归档补回控制面快照: %v", err)
	}
	if restored, err := os.ReadFile(snapshotPath); err != nil || string(restored) != string(snapshotPayload) {
		t.Fatalf("补回的控制面快照不一致: %q err=%v", restored, err)
	}

	encrypted, err := CreateLocalEncryptedWithMutationSnapshot(context.Background(), plan, filepath.Join(t.TempDir(), "encrypted"), "archive-password", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLocalEncrypted(context.Background(), encrypted.Directory, "archive-password"); err != nil {
		t.Fatalf("加密控制面归档校验失败: %v", err)
	}
	encryptedPayload, err := os.ReadFile(filepath.Join(encrypted.Directory, "payload", filepath.FromSlash(manifest.ControlPlaneSnapshot.PayloadPath)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encryptedPayload, snapshotPayload) {
		t.Fatal("加密控制面快照仍包含明文")
	}
	if _, err := VerifyLocal(context.Background(), encrypted.Directory); err == nil {
		t.Fatal("加密控制面归档不应通过明文校验")
	}
	encryptedSnapshotPath := filepath.Join(t.TempDir(), "encrypted-mutation.json")
	if err := os.WriteFile(encryptedSnapshotPath, snapshotPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	encryptedSnapshot := snapshot
	encryptedSnapshot.SnapshotRef = encryptedSnapshotPath
	if err := os.Remove(encryptedSnapshotPath); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMutationSnapshotWithPassword(context.Background(), encrypted.Directory, "archive-password", encryptedSnapshot); err != nil {
		t.Fatalf("从加密归档补回控制面快照失败: %v", err)
	}
	if restored, err := os.ReadFile(encryptedSnapshotPath); err != nil || string(restored) != string(snapshotPayload) {
		t.Fatalf("从加密归档补回的快照不一致: %q err=%v", restored, err)
	}
}

func TestPreviewAndRestoreLocalArchiveFiles(t *testing.T) {
	root, _, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{ID: "restore-conversation", AssetSetHash: "restore-assets", Status: domain.StatusStable}, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	archiveRoot := filepath.Join(t.TempDir(), "archives")
	created, err := CreateLocal(context.Background(), plan, archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	preview, err := PreviewLocalRestore(context.Background(), created.Directory, target, "")
	if err != nil || !preview.Ready || len(preview.Files) != 1 || len(preview.Conflicts) != 0 {
		t.Fatalf("unexpected restore preview: %+v err=%v", preview, err)
	}
	result, err := RestoreLocalFiles(context.Background(), created.Directory, target, "")
	if err != nil || !result.Verified || len(result.RestoredFiles) != 1 {
		t.Fatalf("unexpected restore result: %+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(target, "rollout.jsonl"))
	if err != nil || string(data) != "archive fixture\n" {
		t.Fatalf("restored content mismatch: %q err=%v", data, err)
	}

	conflictTarget := filepath.Join(t.TempDir(), "conflict")
	if err := os.MkdirAll(conflictTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conflictTarget, "rollout.jsonl"), []byte("new content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflictPreview, err := PreviewLocalRestore(context.Background(), created.Directory, conflictTarget, "")
	if err != nil || conflictPreview.Ready || len(conflictPreview.Conflicts) != 1 {
		t.Fatalf("existing target was not reported as conflict: %+v err=%v", conflictPreview, err)
	}
	if _, err := RestoreLocalFiles(context.Background(), created.Directory, conflictTarget, ""); err == nil {
		t.Fatal("restore unexpectedly overwrote an existing target")
	}
	conflictData, err := os.ReadFile(filepath.Join(conflictTarget, "rollout.jsonl"))
	if err != nil || string(conflictData) != "new content\n" {
		t.Fatalf("conflicting target changed: %q err=%v", conflictData, err)
	}

	tamperedPayload := filepath.Join(created.Directory, "payload", "rollout.jsonl")
	if err := os.WriteFile(tamperedPayload, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewLocalRestore(context.Background(), created.Directory, filepath.Join(t.TempDir(), "tampered"), ""); err == nil {
		t.Fatal("tampered archive unexpectedly passed restore preview")
	}
}

func TestRestoreLocalEncryptedArchiveFiles(t *testing.T) {
	root, _, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{ID: "encrypted-restore", AssetSetHash: "encrypted-restore-assets", Status: domain.StatusStable}, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true, Encrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	created, err := CreateLocalEncrypted(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), "恢复密码")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewLocalRestore(context.Background(), created.Directory, filepath.Join(t.TempDir(), "wrong"), "错误密码"); err == nil {
		t.Fatal("wrong archive password unexpectedly passed preview")
	}
	target := filepath.Join(t.TempDir(), "restored")
	result, err := RestoreLocalFiles(context.Background(), created.Directory, target, "恢复密码")
	if err != nil || !result.Verified {
		t.Fatalf("encrypted restore failed: %+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(target, "rollout.jsonl"))
	if err != nil || string(data) != "archive fixture\n" {
		t.Fatalf("encrypted restored content mismatch: %q err=%v", data, err)
	}
}

func TestRestoreAllowsResolvableParentAlias(t *testing.T) {
	root, _, files := stableFixtureForArchive(t)
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{ID: "alias-restore", AssetSetHash: "alias-assets", Status: domain.StatusStable}, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	created, err := CreateLocal(context.Background(), plan, filepath.Join(t.TempDir(), "archives"))
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(t.TempDir(), "real-parent")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(t.TempDir(), "alias-parent")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	target := filepath.Join(aliasParent, "restored")
	preview, err := PreviewLocalRestore(context.Background(), created.Directory, target, "")
	if err != nil || !preview.Ready {
		t.Fatalf("resolvable parent alias was rejected: %+v err=%v", preview, err)
	}
	if _, err := RestoreLocalFiles(context.Background(), created.Directory, target, ""); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(realParent, "restored", "rollout.jsonl")); err != nil || string(data) != "archive fixture\n" {
		t.Fatalf("restore through parent alias failed: %q err=%v", data, err)
	}
}

func TestRestoreRejectsSymlinkInsideTargetRoot(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "nested")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, "rollout.jsonl")
	if err := os.WriteFile(sourcePath, []byte("nested archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	files := []domain.FileInventory{{Asset: domain.Asset{RelativePath: "nested/rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}
	plan, err := lifecycle.BuildPlan(context.Background(), root, domain.ConversationSummary{ID: "symlink-restore", AssetSetHash: "symlink-assets", Status: domain.StatusStable}, files, writableDescriptorForArchive(), lifecycle.PlanOptions{Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	created, err := CreateLocal(context.Background(), plan, filepath.Join(t.TempDir(), "archives"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "nested")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	preview, err := PreviewLocalRestore(context.Background(), created.Directory, target, "")
	if err != nil || preview.Ready || len(preview.Conflicts) != 1 {
		t.Fatalf("target-root symlink was not rejected: %+v err=%v", preview, err)
	}
	if _, err := RestoreLocalFiles(context.Background(), created.Directory, target, ""); err == nil {
		t.Fatal("restore unexpectedly followed target-root symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "rollout.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("restore wrote through target-root symlink: %v", err)
	}
}

func stableFixtureForArchive(t *testing.T) (string, domain.ConversationSummary, []domain.FileInventory) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("archive fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return root, domain.ConversationSummary{ID: "archive-conversation", AssetSetHash: "hash", Status: domain.StatusStable}, []domain.FileInventory{{Asset: domain.Asset{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}
}
