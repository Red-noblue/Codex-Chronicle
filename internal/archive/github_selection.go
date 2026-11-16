// 中文说明：保存用户选择的 GitHub 归档仓库；文件只包含仓库标识，不包含访问令牌。
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GitHubArchiveSelection 是本地归档目标的最小持久化配置。
type GitHubArchiveSelection struct {
	Owner      string    `json:"owner"`
	Repository string    `json:"repository"`
	UpdatedAt  time.Time `json:"updated_at"`
}

var ErrGitHubArchiveSelectionNotFound = errors.New("GitHub 归档仓库选择不存在")

// GitHubArchiveSelectionStore 是远端仓库选择的持久化边界。
type GitHubArchiveSelectionStore interface {
	Load(ctx context.Context) (GitHubArchiveSelection, error)
	Save(ctx context.Context, selection GitHubArchiveSelection) error
	Clear(ctx context.Context) error
}

// FileGitHubArchiveSelectionStore 使用本地配置文件保存仓库选择，不保存任何凭证。
type FileGitHubArchiveSelectionStore struct {
	Path string
	mu   sync.Mutex
}

// NewFileGitHubArchiveSelectionStore 创建文件选择存储；构造时不会创建目录或写入文件。
func NewFileGitHubArchiveSelectionStore(path string) (*FileGitHubArchiveSelectionStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("GitHub 归档选择路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析 GitHub 归档选择路径失败: %w", err)
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("GitHub 归档选择文件不能是符号链接")
	}
	return &FileGitHubArchiveSelectionStore{Path: abs}, nil
}

// Load 读取选择；文件不存在时返回明确的未选择状态。
func (s *FileGitHubArchiveSelectionStore) Load(ctx context.Context) (GitHubArchiveSelection, error) {
	if err := ctx.Err(); err != nil {
		return GitHubArchiveSelection{}, err
	}
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return GitHubArchiveSelection{}, ErrGitHubArchiveSelectionNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Lstat(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return GitHubArchiveSelection{}, ErrGitHubArchiveSelectionNotFound
		}
		return GitHubArchiveSelection{}, fmt.Errorf("读取 GitHub 归档选择状态失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return GitHubArchiveSelection{}, fmt.Errorf("GitHub 归档选择文件不是普通文件")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return GitHubArchiveSelection{}, fmt.Errorf("读取 GitHub 归档选择失败: %w", err)
	}
	var selection GitHubArchiveSelection
	if len(data) == 0 || len(data) > 64<<10 || json.Unmarshal(data, &selection) != nil {
		return GitHubArchiveSelection{}, fmt.Errorf("GitHub 归档选择文件无效")
	}
	if err := validateGitHubArchiveSelection(selection); err != nil {
		return GitHubArchiveSelection{}, err
	}
	return selection, nil
}

// Save 原子替换选择文件，父目录只在用户明确选择仓库时创建。
func (s *FileGitHubArchiveSelectionStore) Save(ctx context.Context, selection GitHubArchiveSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return fmt.Errorf("GitHub 归档选择存储未初始化")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateGitHubArchiveSelection(selection); err != nil {
		return err
	}
	parent := filepath.Dir(s.Path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("创建 GitHub 归档选择目录失败: %w", err)
	}
	if info, statErr := os.Lstat(s.Path); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("GitHub 归档选择目标不是普通文件")
	}
	data, err := json.MarshalIndent(selection, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 GitHub 归档选择失败: %w", err)
	}
	temporary, err := os.CreateTemp(parent, ".github-archive-selection-*.tmp")
	if err != nil {
		return fmt.Errorf("创建 GitHub 归档选择临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("设置 GitHub 归档选择权限失败: %w", err)
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入 GitHub 归档选择失败: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("同步 GitHub 归档选择失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭 GitHub 归档选择失败: %w", err)
	}
	if err := os.Rename(temporaryPath, s.Path); err != nil {
		return fmt.Errorf("发布 GitHub 归档选择失败: %w", err)
	}
	return nil
}

// Clear 删除用户明确取消的选择文件；不存在时视为已清除。
func (s *FileGitHubArchiveSelectionStore) Clear(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Lstat(s.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 GitHub 归档选择状态失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("GitHub 归档选择目标不是普通文件")
	}
	if err := os.Remove(s.Path); err != nil {
		return fmt.Errorf("清除 GitHub 归档选择失败: %w", err)
	}
	return nil
}

func validateGitHubArchiveSelection(selection GitHubArchiveSelection) error {
	if !validGitHubIdentifier(selection.Owner) || !validGitHubIdentifier(selection.Repository) {
		return fmt.Errorf("GitHub 归档仓库标识无效")
	}
	return nil
}
