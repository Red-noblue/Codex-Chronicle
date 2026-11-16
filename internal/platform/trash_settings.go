// 中文说明：保存用户级回收站路径覆盖，并提供不写入目录的候选路径探测。
package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const trashSettingsVersion = 1

// TrashSettings 保存回收站 provider 的用户级路径覆盖；空路径表示使用系统默认路径。
type TrashSettings struct {
	Version             int                             `json:"version"`
	CustomPath          string                          `json:"custom_path,omitempty"`
	Probe               *TrashProbeRecord               `json:"probe,omitempty"`
	Verification        *TrashVerification              `json:"verification,omitempty"`
	ArchiveConfirmation *ArchiveDestinationConfirmation `json:"archive_confirmation,omitempty"`
}

// TrashSettingsStore 在 .codex 外持久化回收站设置。
type TrashSettingsStore struct {
	path string
	mu   sync.Mutex
}

// DefaultTrashSettingsStore 使用当前用户配置目录创建未落盘的设置存储。
func DefaultTrashSettingsStore() (*TrashSettingsStore, error) {
	directory, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(directory) == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			if err != nil {
				return nil, fmt.Errorf("无法读取用户配置目录: %w", err)
			}
			return nil, fmt.Errorf("无法读取用户主目录: %w", homeErr)
		}
		if strings.TrimSpace(home) == "" {
			return nil, fmt.Errorf("用户主目录为空")
		}
		return NewTrashSettingsStore(filepath.Join(home, ".codex-chronicle-trash-settings.json"))
	}
	return NewTrashSettingsStore(filepath.Join(directory, "Codex Chronicle", "trash-settings.json"))
}

// NewTrashSettingsStore 创建设置存储，不会创建文件或目录。
func NewTrashSettingsStore(path string) (*TrashSettingsStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("回收站设置路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析回收站设置路径失败: %w", err)
	}
	abs = filepath.Clean(abs)
	if pathHasCodexComponent(abs) {
		return nil, fmt.Errorf("回收站设置不能位于 .codex 路径")
	}
	if info, statErr := os.Lstat(abs); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, fmt.Errorf("回收站设置必须是普通文件")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("检查回收站设置失败: %w", statErr)
	}
	return &TrashSettingsStore{path: abs}, nil
}

// Load 读取用户设置；文件不存在时返回系统默认设置。
func (s *TrashSettingsStore) Load(ctx context.Context) (TrashSettings, error) {
	if s == nil {
		return TrashSettings{}, fmt.Errorf("回收站设置存储为空")
	}
	if err := ctx.Err(); err != nil {
		return TrashSettings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return TrashSettings{Version: trashSettingsVersion}, nil
	}
	if err != nil {
		return TrashSettings{}, fmt.Errorf("读取回收站设置失败: %w", err)
	}
	var settings TrashSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return TrashSettings{}, fmt.Errorf("解析回收站设置失败: %w", err)
	}
	settings, err = normalizeTrashSettings(settings)
	if err != nil {
		return TrashSettings{}, err
	}
	return settings, nil
}

// Save 校验设置后原子写入；自定义路径必须是现存且权限位显示可写的目录。
func (s *TrashSettingsStore) Save(ctx context.Context, settings TrashSettings) error {
	if s == nil {
		return fmt.Errorf("回收站设置存储为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	settings, err := validateTrashSettings(settings, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化回收站设置失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("创建回收站设置目录失败: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".trash-settings-*.tmp")
	if err != nil {
		return fmt.Errorf("创建回收站设置临时文件失败: %w", err)
	}
	temporary := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return fmt.Errorf("保护回收站设置权限失败: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return fmt.Errorf("写入回收站设置失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return fmt.Errorf("同步回收站设置失败: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("关闭回收站设置失败: %w", err)
	}
	if err := os.Rename(temporary, s.path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("发布回收站设置失败: %w", err)
	}
	return nil
}

// Path 返回设置文件的绝对路径。
func (s *TrashSettingsStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// DetectTrashAtPath 对用户指定路径进行只读探测，不创建目录或探针文件。
func DetectTrashAtPath(goos, goarch, path string) TrashDetection {
	result := TrashDetection{OS: goos, Architecture: goarch, ReadOnly: true}
	path = strings.TrimSpace(path)
	if path == "" {
		result.Reason = "自定义回收站路径为空"
		return result
	}
	if goos == "windows" {
		result.Reason = "Windows 使用 Shell 原生回收站，不能通过目录路径覆盖"
		return result
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		result.Reason = "解析自定义回收站路径失败: " + err.Error()
		return result
	}
	result.CandidatePath = filepath.Clean(abs)
	if pathHasCodexComponent(result.CandidatePath) {
		result.Reason = "回收站路径不能位于 .codex"
		return result
	}
	info, err := os.Lstat(result.CandidatePath)
	if err != nil {
		if os.IsNotExist(err) {
			result.Reason = "自定义回收站目录不存在"
		} else {
			result.Reason = "自定义回收站目录不可读取: " + err.Error()
		}
		return result
	}
	result.Exists = true
	result.IsDirectory = info.IsDir() && info.Mode()&os.ModeSymlink == 0
	result.Writable = result.IsDirectory && info.Mode().Perm()&0o222 != 0
	if !result.IsDirectory {
		result.Reason = "自定义回收站路径不是普通目录"
		return result
	}
	if !result.Writable {
		result.Reason = "目录权限位未显示为可写；执行时仍会检查实际权限"
		return result
	}
	result.Supported = true
	result.Reason = "自定义回收站目录已发现且权限位显示可写；实际移动前仍会再次验证"
	return result
}

func validateTrashSettings(settings TrashSettings, goos, goarch string) (TrashSettings, error) {
	settings, err := normalizeTrashSettings(settings)
	if err != nil {
		return TrashSettings{}, err
	}
	if settings.CustomPath == "" {
		return settings, nil
	}
	if goos == "windows" {
		return TrashSettings{}, fmt.Errorf("Windows 使用 Shell 原生回收站，不接受自定义目录路径")
	}
	if goos == "linux" || goos == "freebsd" || goos == "openbsd" || goos == "netbsd" {
		if filepath.Base(settings.CustomPath) != "files" {
			return TrashSettings{}, fmt.Errorf("自定义回收站目录必须指向回收站 files 目录")
		}
	}
	detection := DetectTrashAtPath(goos, goarch, settings.CustomPath)
	if !detection.Supported {
		return TrashSettings{}, fmt.Errorf("自定义回收站路径不可用: %s", detection.Reason)
	}
	settings.CustomPath = detection.CandidatePath
	return settings, nil
}

func normalizeTrashSettings(settings TrashSettings) (TrashSettings, error) {
	if settings.Version == 0 {
		settings.Version = trashSettingsVersion
	}
	if settings.Version != trashSettingsVersion {
		return TrashSettings{}, fmt.Errorf("不支持的回收站设置版本: %d", settings.Version)
	}
	settings.CustomPath = strings.TrimSpace(settings.CustomPath)
	if settings.CustomPath == "" {
		return settings, nil
	}
	abs, err := filepath.Abs(settings.CustomPath)
	if err != nil {
		return TrashSettings{}, fmt.Errorf("解析自定义回收站路径失败: %w", err)
	}
	settings.CustomPath = filepath.Clean(abs)
	if pathHasCodexComponent(settings.CustomPath) {
		return TrashSettings{}, fmt.Errorf("回收站路径不能位于 .codex")
	}
	return settings, nil
}

func pathHasCodexComponent(path string) bool {
	for _, part := range strings.FieldsFunc(filepath.Clean(path), func(value rune) bool { return value == '/' || value == '\\' }) {
		if strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return false
}
