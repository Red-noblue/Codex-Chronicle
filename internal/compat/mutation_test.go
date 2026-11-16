// 中文说明：验证 control-plane mutation 请求、快照排序和篡改检测，不连接真实 Codex 数据库。
package compat

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func mutationRequest(t *testing.T) MutationRequest {
	t.Helper()
	return MutationRequest{
		Root: filepath.Join(t.TempDir(), "fixture"), ConversationID: "conversation-1", AssetSetHash: "asset-set-1",
		Assets: []MutationAsset{
			{RelativePath: "z.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: 2, SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: domain.StatusStable},
			{RelativePath: "a.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: 1, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: domain.StatusStable},
		},
	}
}

func TestBuildMutationSnapshotSortsAssetsAndBindsIdentity(t *testing.T) {
	request := mutationRequest(t)
	snapshot, err := BuildMutationSnapshot(context.Background(), "fixture-adapter-v1", request)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 1 || snapshot.AdapterID != "fixture-adapter-v1" || len(snapshot.SnapshotHash) != 64 {
		t.Fatalf("unexpected snapshot identity: %+v", snapshot)
	}
	if len(snapshot.Assets) != 2 || snapshot.Assets[0].RelativePath != "a.jsonl" || snapshot.Assets[1].RelativePath != "z.jsonl" {
		t.Fatalf("assets were not canonically sorted: %+v", snapshot.Assets)
	}
	if err := ValidateMutationSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("fresh snapshot rejected: %v", err)
	}
	snapshot.Assets[0].SizeBytes++
	if err := ValidateMutationSnapshot(context.Background(), snapshot); err == nil {
		t.Fatal("tampered snapshot was accepted")
	}
}

func TestValidateMutationRequestFailsClosed(t *testing.T) {
	base := mutationRequest(t)
	cases := []struct {
		name   string
		mutate func(*MutationRequest)
		code   string
	}{
		{name: "relative root", mutate: func(request *MutationRequest) { request.Root = "relative" }, code: "MUTATION_ROOT_NOT_ABSOLUTE"},
		{name: "missing conversation", mutate: func(request *MutationRequest) { request.ConversationID = "" }, code: "MUTATION_CONVERSATION_MISSING"},
		{name: "duplicate path", mutate: func(request *MutationRequest) { request.Assets[1].RelativePath = request.Assets[0].RelativePath }, code: "MUTATION_ASSET_DUPLICATE"},
		{name: "traversal", mutate: func(request *MutationRequest) { request.Assets[0].RelativePath = "../outside" }, code: "MUTATION_ASSET_PATH_INVALID"},
		{name: "invalid hash", mutate: func(request *MutationRequest) { request.Assets[0].SHA256 = "bad" }, code: "MUTATION_ASSET_HASH_INVALID"},
		{name: "unstable", mutate: func(request *MutationRequest) { request.Assets[0].Status = domain.StatusUnknown }, code: "MUTATION_ASSET_NOT_STABLE"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := base
			request.Assets = append([]MutationAsset(nil), base.Assets...)
			testCase.mutate(&request)
			err := ValidateMutationRequest(context.Background(), request)
			var mutationErr *MutationError
			if !errors.As(err, &mutationErr) || mutationErr.Code != testCase.code || !mutationErr.ReadOnly {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestValidateMutationRequestHonorsCancellation(t *testing.T) {
	request := mutationRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateMutationRequest(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
