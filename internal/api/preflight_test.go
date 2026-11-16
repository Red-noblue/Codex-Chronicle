// 中文说明：验证安装版已打开的项目数据库可以正常健康检查，不将自身 WAL 当作外部未知数据库风险。
package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/store"
)

func TestPreflightForOpenedProjectDatabaseUsesCommittedSchema(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	server := NewServerWithStore("", database)
	defer server.Close()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/health/preflight", nil))
	var result struct {
		Status    string                   `json:"status"`
		Blocked   bool                     `json:"blocked"`
		Migration store.MigrationPreflight `json:"migration"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ready" || result.Blocked || result.Migration.CurrentVersion != result.Migration.SupportedVersion {
		t.Fatalf("自身打开的数据库被误判：%s", response.Body.String())
	}
}
