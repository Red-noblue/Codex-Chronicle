// 中文说明：为独立 archive.db 的 schema 迁移提供一致性备份和关闭后原子恢复能力。
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	moderncsqlite "modernc.org/sqlite"
)

// OpenOptions 控制 archive.db 的打开行为。
type OpenOptions struct {
	// MigrationBackupRoot 为空时使用 archive.db 旁的默认备份目录。
	MigrationBackupRoot string
	// DisableMigrationBackup 仅供明确的临时数据库场景使用。
	DisableMigrationBackup bool
}

// MigrationBackup 描述一次 schema 迁移前的独立 SQLite 快照。
type MigrationBackup struct {
	Path        string    `json:"path"`
	SourcePath  string    `json:"source_path"`
	FromVersion int       `json:"from_version"`
	ToVersion   int       `json:"to_version"`
	SizeBytes   int64     `json:"size_bytes"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"created_at"`
}

// DefaultMigrationBackupRoot 返回独立于 archive.db 文件名的迁移备份目录。
func DefaultMigrationBackupRoot(path string) string {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(abs), ".codex-chronicle-migration-backups")
}

// CreateMigrationBackup 使用 SQLite online backup 创建一致性快照，不复制未提交的文件字节。
func CreateMigrationBackup(ctx context.Context, database *sql.DB, sourcePath, backupRoot string, fromVersion, toVersion int) (MigrationBackup, error) {
	return createMigrationBackup(ctx, database, sourcePath, backupRoot, fromVersion, toVersion)
}

// OnlineBackup 将一个已打开的 SQLite 连接复制到外部目标文件。
// SQLite backup API 会读取一致的数据库快照，可在源连接存在 WAL writer 时使用；
// 调用方必须确保 destination 位于源目录之外，并自行负责临时文件和权限。
func OnlineBackup(ctx context.Context, database *sql.DB, destination string) error {
	if database == nil {
		return errors.New("SQLite 数据库连接为空")
	}
	if strings.TrimSpace(destination) == "" {
		return errors.New("SQLite online backup 目标不能为空")
	}
	return onlineBackup(ctx, database, destination)
}

// RestoreMigrationBackup 在数据库连接已关闭且没有其他 writer 时恢复迁移前快照。
func RestoreMigrationBackup(ctx context.Context, targetPath, backupPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := regularPath(targetPath, "archive.db")
	if err != nil {
		return err
	}
	backup, err := regularPath(backupPath, "迁移备份")
	if err != nil {
		return err
	}
	if target == backup {
		return errors.New("迁移备份路径不能与 archive.db 相同")
	}
	if filepath.Base(target) == "." || filepath.Base(backup) == "." {
		return errors.New("迁移恢复路径无效")
	}
	if err := ensureNotCodexPath(target); err != nil {
		return err
	}
	if err := ensureNotCodexPath(backup); err != nil {
		return err
	}
	if err := verifyRegularAuxiliary(target + "-wal"); err != nil {
		return err
	}
	if err := verifyRegularAuxiliary(target + "-shm"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	parent := filepath.Dir(target)
	temporary, err := os.CreateTemp(parent, ".codex-chronicle-restore-*.tmp")
	if err != nil {
		return fmt.Errorf("创建迁移恢复临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := copyDatabaseFile(temporary, backup); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("复制迁移备份失败: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("同步迁移恢复临时文件失败: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("设置迁移恢复文件权限失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭迁移恢复临时文件失败: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		path := target + suffix
		if _, err := os.Lstat(path); err == nil {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("清理恢复后的 SQLite %s 文件失败: %w", suffix, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查恢复后的 SQLite %s 文件失败: %w", suffix, err)
		}
	}
	if err := replaceDatabaseFile(temporaryPath, target); err != nil {
		return fmt.Errorf("原子替换 archive.db 失败: %w", err)
	}
	cleanup = false
	return syncDirectory(parent)
}

func createMigrationBackup(ctx context.Context, database *sql.DB, sourcePath, backupRoot string, fromVersion, toVersion int) (MigrationBackup, error) {
	if database == nil {
		return MigrationBackup{}, errors.New("archive.db 数据库连接为空")
	}
	source, err := regularPath(sourcePath, "archive.db")
	if err != nil {
		return MigrationBackup{}, err
	}
	if err := ensureNotCodexPath(source); err != nil {
		return MigrationBackup{}, err
	}
	root, err := filepath.Abs(filepath.Clean(strings.TrimSpace(backupRoot)))
	if err != nil || strings.TrimSpace(backupRoot) == "" {
		return MigrationBackup{}, errors.New("迁移备份目录不能为空")
	}
	if err := ensureNotCodexPath(root); err != nil {
		return MigrationBackup{}, err
	}
	if info, statErr := os.Lstat(root); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return MigrationBackup{}, errors.New("迁移备份目录不能是符号链接")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return MigrationBackup{}, fmt.Errorf("创建迁移备份目录失败: %w", err)
	}
	temporary, err := os.CreateTemp(root, "archive.db-migration-*.db.tmp")
	if err != nil {
		return MigrationBackup{}, fmt.Errorf("创建迁移备份临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return MigrationBackup{}, fmt.Errorf("关闭迁移备份临时文件失败: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return MigrationBackup{}, fmt.Errorf("准备迁移备份目标失败: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := onlineBackup(ctx, database, temporaryPath); err != nil {
		return MigrationBackup{}, fmt.Errorf("创建 archive.db 迁移备份失败: %w", err)
	}
	file, err := os.OpenFile(temporaryPath, os.O_RDWR, 0o600)
	if err != nil {
		return MigrationBackup{}, fmt.Errorf("打开迁移备份失败: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return MigrationBackup{}, fmt.Errorf("设置迁移备份文件权限失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return MigrationBackup{}, fmt.Errorf("同步迁移备份失败: %w", err)
	}
	if err := file.Close(); err != nil {
		return MigrationBackup{}, fmt.Errorf("关闭迁移备份失败: %w", err)
	}
	finalPath := strings.TrimSuffix(temporaryPath, ".tmp")
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return MigrationBackup{}, fmt.Errorf("发布迁移备份失败: %w", err)
	}
	cleanup = false
	if err := syncDirectory(root); err != nil {
		return MigrationBackup{}, fmt.Errorf("同步迁移备份目录失败: %w", err)
	}
	info, err := os.Stat(finalPath)
	if err != nil {
		return MigrationBackup{}, fmt.Errorf("读取迁移备份信息失败: %w", err)
	}
	hash, err := fileSHA256(finalPath)
	if err != nil {
		return MigrationBackup{}, fmt.Errorf("计算迁移备份摘要失败: %w", err)
	}
	return MigrationBackup{Path: finalPath, SourcePath: source, FromVersion: fromVersion, ToVersion: toVersion, SizeBytes: info.Size(), SHA256: hash, CreatedAt: time.Now().UTC()}, nil
}

func onlineBackup(ctx context.Context, database *sql.DB, destination string) error {
	connection, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	return connection.Raw(func(raw any) error {
		backuper, ok := raw.(interface {
			NewBackup(string) (*moderncsqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite 驱动不支持 online backup")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = backup.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(256)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
		finishErr := backup.Finish()
		finished = true
		return finishErr
	})
}

func copyDatabaseFile(destination *os.File, sourcePath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	_, err = io.Copy(destination, source)
	return err
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func regularPath(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return "", fmt.Errorf("%s 路径不能为空", label)
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("解析%s路径失败: %w", label, err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("读取%s失败: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s必须是普通文件，不能是链接或特殊文件", label)
	}
	return abs, nil
}

func verifyRegularAuxiliary(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查 SQLite 辅助文件失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("SQLite 辅助文件不是普通文件，拒绝迁移恢复")
	}
	return nil
}

func ensureNotCodexPath(path string) error {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == ".codex" {
			return fmt.Errorf("拒绝在原始 .codex 路径执行 archive.db 迁移操作: %s", path)
		}
	}
	return nil
}
