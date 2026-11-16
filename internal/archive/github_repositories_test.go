// 中文说明：验证 GitHub 仓库列表、分页、私有仓库创建和凭证错误边界。
package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGitHubRepositoryClientListsPagesAndCreatesPrivateRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer runtime-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Fatalf("missing GitHub API version header")
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/user/repos":
			page, _ := strconv.Atoi(request.URL.Query().Get("page"))
			count := 1
			if page == 1 {
				count = 100
			}
			repositories := make([]map[string]any, 0, count)
			for index := 0; index < count; index++ {
				number := index
				if page == 2 {
					number = 100
				}
				repositories = append(repositories, map[string]any{
					"name": fmt.Sprintf("repo-%03d", number), "full_name": fmt.Sprintf("owner/repo-%03d", number),
					"description": "归档仓库", "private": true, "default_branch": "main",
					"html_url": fmt.Sprintf("https://github.example/owner/repo-%03d", number),
					"owner":    map[string]string{"login": "owner"},
				})
			}
			_ = json.NewEncoder(writer).Encode(repositories)
		case request.Method == http.MethodPost && request.URL.Path == "/user/repos":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create repository request: %v", err)
			}
			if payload["name"] != "chronicle-archive" || payload["private"] != true || payload["auto_init"] != true {
				t.Fatalf("unexpected create repository payload: %+v", payload)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"name": "chronicle-archive", "full_name": "owner/chronicle-archive",
				"description": payload["description"], "private": true, "default_branch": "main",
				"html_url": "https://github.example/owner/chronicle-archive",
				"owner":    map[string]string{"login": "owner"},
			})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubRepositoryClient(GitHubRepositoryClientConfig{BaseURL: server.URL}, "runtime-token")
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := client.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 101 || repositories[0].Owner != "owner" || repositories[100].Name != "repo-100" {
		t.Fatalf("unexpected paged repository list: len=%d first=%+v last=%+v", len(repositories), repositories[0], repositories[len(repositories)-1])
	}
	created, err := client.CreatePrivateRepository(context.Background(), "chronicle-archive", "归档仓库")
	if err != nil {
		t.Fatal(err)
	}
	if created.FullName != "owner/chronicle-archive" || !created.Private || created.DefaultBranch != "main" {
		t.Fatalf("unexpected created repository: %+v", created)
	}
}

func TestGitHubRepositoryClientRejectsInvalidNameAndRedactsToken(t *testing.T) {
	client, err := NewGitHubRepositoryClient(GitHubRepositoryClientConfig{BaseURL: "https://api.github.example"}, "runtime-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "../outside", "中文仓库", strings.Repeat("a", 101)} {
		if _, err := client.CreatePrivateRepository(context.Background(), name, ""); err == nil {
			t.Fatalf("expected invalid repository name rejection: %q", name)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte("{\"message\":\"token runtime-token was rejected\"}"))
	}))
	defer server.Close()
	errorClient, err := NewGitHubRepositoryClient(GitHubRepositoryClientConfig{BaseURL: server.URL}, "runtime-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = errorClient.ListRepositories(context.Background())
	if err == nil || strings.Contains(err.Error(), "runtime-token") || !strings.Contains(err.Error(), "[已隐藏]") {
		t.Fatalf("unexpected or secret-bearing GitHub error: %v", err)
	}
}

func TestGitHubCredentialRepositoryClientDefersCredentialLookup(t *testing.T) {
	store := NewMemoryGitHubTokenStore()
	service, err := NewGitHubDeviceAuthService(GitHubDeviceAuthConfig{BaseURL: "https://github.example", ClientID: "client"}, store, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	client, err := NewGitHubCredentialRepositoryClient(service, GitHubRepositoryClientConfig{BaseURL: "https://api.github.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListRepositories(context.Background()); err == nil {
		t.Fatal("expected missing credential before device binding")
	}
	if err := store.Save(context.Background(), "default", "runtime-token"); err != nil {
		t.Fatal(err)
	}
	var _ GitHubRepositoryCatalog = client
}
