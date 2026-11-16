// 中文说明：验证 GitHub 归档仓库选择的持久化、运行时切换和凭证隔离。
package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileGitHubArchiveSelectionStoreRoundTripAndMissingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "github-selection.json")
	store, err := NewFileGitHubArchiveSelectionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrGitHubArchiveSelectionNotFound) {
		t.Fatalf("expected missing selection, got %v", err)
	}
	selection := GitHubArchiveSelection{Owner: "owner", Repository: "chronicle-archive"}
	if err := store.Save(context.Background(), selection); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Owner != selection.Owner || loaded.Repository != selection.Repository {
		t.Fatalf("unexpected selection: %+v", loaded)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token") || !strings.Contains(string(data), "chronicle-archive") {
		t.Fatalf("selection file contains unexpected data: %s", data)
	}
	if err := store.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("selection file was not cleared: %v", err)
	}
}

func TestGitHubCredentialArchiveProviderSelectionOverridesDefaultAndPersists(t *testing.T) {
	selectionPath := filepath.Join(t.TempDir(), "github-selection.json")
	selectionStore, err := NewFileGitHubArchiveSelectionStore(selectionPath)
	if err != nil {
		t.Fatal(err)
	}
	tokenStore := NewMemoryGitHubTokenStore()
	if err := tokenStore.Save(context.Background(), "default", "runtime-token"); err != nil {
		t.Fatal(err)
	}
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: "https://github.example", ClientID: "client"}, tokenStore, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	provider, err := NewGitHubCredentialArchiveProviderWithSelectionStore(service, GitHubConfig{
		BaseURL: "https://api.github.example", Owner: "default-owner", Repository: "default-repository",
	}, selectionStore)
	if err != nil {
		t.Fatal(err)
	}
	selected, configured, err := provider.SelectedArchiveRepository(context.Background())
	if err != nil || !configured || selected.FullName != "default-owner/default-repository" {
		t.Fatalf("unexpected default target: %+v configured=%v err=%v", selected, configured, err)
	}
	if _, err := provider.SelectArchiveRepository(context.Background(), "archive-owner", "archive-repository"); err != nil {
		t.Fatal(err)
	}
	selected, configured, err = provider.SelectedArchiveRepository(context.Background())
	if err != nil || !configured || selected.FullName != "archive-owner/archive-repository" {
		t.Fatalf("unexpected selected target: %+v configured=%v err=%v", selected, configured, err)
	}
	restarted, err := NewGitHubCredentialArchiveProviderWithSelectionStore(service, GitHubConfig{BaseURL: "https://api.github.example"}, selectionStore)
	if err != nil {
		t.Fatal(err)
	}
	selected, configured, err = restarted.SelectedArchiveRepository(context.Background())
	if err != nil || !configured || selected.FullName != "archive-owner/archive-repository" {
		t.Fatalf("selection did not survive provider restart: %+v configured=%v err=%v", selected, configured, err)
	}
	concrete, err := restarted.provider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if concrete.config.Owner != "archive-owner" || concrete.config.Repository != "archive-repository" || concrete.config.Token != "runtime-token" {
		t.Fatalf("provider did not use selected target or runtime token: %+v", concrete.config)
	}
	if err := restarted.ClearArchiveRepository(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, configured, err := restarted.SelectedArchiveRepository(context.Background()); err != nil || configured {
		t.Fatalf("selection was not cleared: configured=%v err=%v", configured, err)
	}
}

func TestGitHubCredentialArchiveProviderRejectsUnsafeSelection(t *testing.T) {
	tokenStore := NewMemoryGitHubTokenStore()
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: "https://github.example", ClientID: "client"}, tokenStore, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	provider, err := NewGitHubCredentialArchiveProvider(service, GitHubConfig{BaseURL: "https://api.github.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.SelectArchiveRepository(context.Background(), "../outside", "repo"); err == nil {
		t.Fatal("unsafe owner was accepted")
	}
	if _, err := provider.SelectArchiveRepository(context.Background(), "owner", "中文"); err == nil {
		t.Fatal("unsafe repository was accepted")
	}
}
