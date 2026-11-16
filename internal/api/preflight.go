// 中文说明：提供更新前的 archive.db 迁移预检和兼容 adapter 健康检查；所有检查均为只读。
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"codex-chronicle.local/internal/store"
	updatepkg "codex-chronicle.local/internal/update"
)

func (s *Server) preflightHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	migration, migrationAvailable, migrationErr := s.currentMigrationPreflight(request.Context())
	adapter := s.registry.Health()
	blocked := !adapter.Valid
	if migrationAvailable && (migration.Status != "ready" || migrationErr != nil) {
		blocked = true
	}
	status := "ready"
	if blocked {
		status = "blocked"
	}
	response := map[string]any{
		"status":              status,
		"read_only":           true,
		"source_read_only":    true,
		"blocked":             blocked,
		"migration_available": migrationAvailable,
		"migration":           migration,
		"adapter":             adapter,
	}
	if migrationErr != nil {
		response["migration_error"] = migrationErr.Error()
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) currentMigrationPreflight(ctx context.Context) (store.MigrationPreflight, bool, error) {
	archive := s.currentArchive()
	if archive == nil {
		return store.MigrationPreflight{}, false, nil
	}
	result, err := archive.OpenedMigrationPreflight(ctx)
	return result, true, err
}

func (s *Server) updateHealthCheck() updatepkg.HealthCheck {
	return func(ctx context.Context, versionRoot string) error {
		if err := s.updater.VerifyVersionRoot(ctx, versionRoot); err != nil {
			return err
		}
		adapter := s.registry.Health()
		if !adapter.Valid {
			return fmt.Errorf("兼容 adapter 健康检查失败：%s", strings.Join(adapter.Errors, "；"))
		}
		migration, available, err := s.currentMigrationPreflight(ctx)
		if err != nil {
			return fmt.Errorf("archive.db 迁移预检失败：%w", err)
		}
		if available && migration.Status != "ready" {
			return fmt.Errorf("archive.db 迁移预检未通过：%s（当前 %d，支持 %d）", migration.Status, migration.CurrentVersion, migration.SupportedVersion)
		}
		if s.updatePackageCheck != nil {
			return s.updatePackageCheck(ctx, versionRoot)
		}
		return nil
	}
}
