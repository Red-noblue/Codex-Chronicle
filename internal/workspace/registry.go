// 中文说明：管理 Codex Chronicle 的独立工作区注册表；只保存路径和元数据，不修改或删除工作区源文件。
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const currentVersion = 1

var idPattern = regexp.MustCompile(`^workspace-[0-9a-f]{32}$`)

// Definition 描述一个彼此隔离的源扫描根和可选 archive.db。
type Definition struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Root      string    `json:"root"`
	ArchiveDB string    `json:"archive_db,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Registry 是注册表文件的持久化结构；CurrentID 决定 API 当前作用的工作区。
type Registry struct {
	Version    int          `json:"version"`
	CurrentID  string       `json:"current_id,omitempty"`
	Workspaces []Definition `json:"workspaces"`
}

// FileStore 以原子替换方式保存注册表；路径必须位于 .codex 之外。
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore 创建注册表存储，不会创建文件或目录。
func NewFileStore(path string) (*FileStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("工作区注册表路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析工作区注册表路径失败：%w", err)
	}
	if pathHasCodexComponent(abs) {
		return nil, fmt.Errorf("工作区注册表不能位于 .codex 路径")
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("工作区注册表不能是符号链接")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("检查工作区注册表失败：%w", statErr)
	}
	return &FileStore{path: filepath.Clean(abs)}, nil
}

// Path 返回注册表文件的绝对路径。
func (s *FileStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Load 读取注册表；文件不存在时返回空的 v1 注册表。
func (s *FileStore) Load(ctx context.Context) (Registry, error) {
	if s == nil {
		return Registry{}, fmt.Errorf("工作区注册表存储为空")
	}
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return Registry{Version: currentVersion, Workspaces: make([]Definition, 0)}, nil
	}
	if err != nil {
		return Registry{}, fmt.Errorf("读取工作区注册表失败：%w", err)
	}
	var registry Registry
	if err := json.Unmarshal(data, &registry); err != nil {
		return Registry{}, fmt.Errorf("解析工作区注册表失败：%w", err)
	}
	if err := Validate(ctx, &registry); err != nil {
		return Registry{}, err
	}
	return registry, nil
}

// Save 校验后原子写入注册表；不会触碰任何 Definition.Root 下的文件。
func (s *FileStore) Save(ctx context.Context, registry Registry) error {
	if s == nil {
		return fmt.Errorf("工作区注册表存储为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := Validate(ctx, &registry); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化工作区注册表失败：%w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("创建工作区注册表目录失败：%w", err)
	}
	tmp := s.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("创建工作区注册表临时文件失败：%w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("写入工作区注册表失败：%w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("同步工作区注册表失败：%w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("关闭工作区注册表失败：%w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("发布工作区注册表失败：%w", err)
	}
	return nil
}

// Validate 验证注册表结构、源根和 archive.db 的隔离边界。
func Validate(ctx context.Context, registry *Registry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if registry == nil {
		return fmt.Errorf("工作区注册表为空")
	}
	if registry.Version == 0 {
		registry.Version = currentVersion
	}
	if registry.Version != currentVersion {
		return fmt.Errorf("不支持的工作区注册表版本：%d", registry.Version)
	}
	if registry.Workspaces == nil {
		registry.Workspaces = make([]Definition, 0)
	}
	seenIDs := make(map[string]struct{}, len(registry.Workspaces))
	seenRoots := make(map[string]struct{}, len(registry.Workspaces))
	for index := range registry.Workspaces {
		definition, err := NormalizeDefinition(registry.Workspaces[index])
		if err != nil {
			return fmt.Errorf("工作区 %d 无效：%w", index, err)
		}
		if _, exists := seenIDs[definition.ID]; exists {
			return fmt.Errorf("工作区 ID 重复：%s", definition.ID)
		}
		if _, exists := seenRoots[definition.Root]; exists {
			return fmt.Errorf("工作区源根重复：%s", definition.Root)
		}
		seenIDs[definition.ID] = struct{}{}
		seenRoots[definition.Root] = struct{}{}
		registry.Workspaces[index] = definition
	}
	if registry.CurrentID != "" {
		if _, exists := seenIDs[registry.CurrentID]; !exists {
			return fmt.Errorf("当前工作区不存在：%s", registry.CurrentID)
		}
	}
	sort.Slice(registry.Workspaces, func(i, j int) bool { return registry.Workspaces[i].ID < registry.Workspaces[j].ID })
	return nil
}

// NormalizeDefinition 规范化并验证一个工作区定义。
func NormalizeDefinition(definition Definition) (Definition, error) {
	if strings.TrimSpace(definition.ID) == "" {
		var err error
		definition.ID, err = newID()
		if err != nil {
			return Definition{}, err
		}
	} else if !idPattern.MatchString(definition.ID) {
		return Definition{}, fmt.Errorf("工作区 ID 格式无效：%s", definition.ID)
	}
	if strings.TrimSpace(definition.Root) == "" {
		return Definition{}, fmt.Errorf("工作区源根不能为空")
	}
	root, err := filepath.Abs(definition.Root)
	if err != nil {
		return Definition{}, fmt.Errorf("解析工作区源根失败：%w", err)
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return Definition{}, fmt.Errorf("读取工作区源根失败：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Definition{}, fmt.Errorf("工作区源根必须是普通目录：%s", root)
	}
	definition.Root = root
	definition.Name = strings.TrimSpace(definition.Name)
	if definition.Name == "" {
		definition.Name = filepath.Base(root)
		if definition.Name == "." || definition.Name == string(filepath.Separator) || definition.Name == "" {
			definition.Name = "未命名工作区"
		}
	}
	if len([]rune(definition.Name)) > 120 {
		return Definition{}, fmt.Errorf("工作区名称过长")
	}
	if strings.TrimSpace(definition.ArchiveDB) != "" {
		databasePath, pathErr := filepath.Abs(definition.ArchiveDB)
		if pathErr != nil {
			return Definition{}, fmt.Errorf("解析 archive.db 路径失败：%w", pathErr)
		}
		databasePath = filepath.Clean(databasePath)
		if pathHasCodexComponent(databasePath) || pathsOverlap(root, databasePath) {
			return Definition{}, fmt.Errorf("archive.db 必须位于工作区源根和 .codex 之外")
		}
		definition.ArchiveDB = databasePath
	}
	now := time.Now().UTC()
	if definition.CreatedAt.IsZero() {
		definition.CreatedAt = now
	}
	if definition.UpdatedAt.IsZero() {
		definition.UpdatedAt = now
	}
	return definition, nil
}

// Add 添加工作区，不会创建或复制源目录。
func Add(ctx context.Context, registry *Registry, definition Definition) (Definition, error) {
	if registry == nil {
		return Definition{}, fmt.Errorf("工作区注册表为空")
	}
	item, err := NormalizeDefinition(definition)
	if err != nil {
		return Definition{}, err
	}
	for _, existing := range registry.Workspaces {
		if existing.ID == item.ID || existing.Root == item.Root {
			return Definition{}, fmt.Errorf("工作区已存在：%s", item.Root)
		}
	}
	registry.Version = currentVersion
	registry.Workspaces = append(registry.Workspaces, item)
	if registry.CurrentID == "" {
		registry.CurrentID = item.ID
	}
	if err := Validate(ctx, registry); err != nil {
		return Definition{}, err
	}
	return item, nil
}

// Select 切换当前工作区，不改变任何源文件。
func Select(ctx context.Context, registry *Registry, id string) (Definition, error) {
	if registry == nil || strings.TrimSpace(id) == "" {
		return Definition{}, fmt.Errorf("工作区选择参数无效")
	}
	for _, item := range registry.Workspaces {
		if item.ID == id {
			registry.CurrentID = id
			if err := Validate(ctx, registry); err != nil {
				return Definition{}, err
			}
			return item, nil
		}
	}
	return Definition{}, fmt.Errorf("未找到工作区：%s", id)
}

// Remove 从注册表中移除工作区定义；调用方必须确保不是当前工作区。
func Remove(ctx context.Context, registry *Registry, id string) (Definition, error) {
	if registry == nil || strings.TrimSpace(id) == "" {
		return Definition{}, fmt.Errorf("工作区移除参数无效")
	}
	if registry.CurrentID == id {
		return Definition{}, fmt.Errorf("不能移除当前工作区，请先切换到其他工作区")
	}
	for index, item := range registry.Workspaces {
		if item.ID == id {
			registry.Workspaces = append(registry.Workspaces[:index], registry.Workspaces[index+1:]...)
			if err := Validate(ctx, registry); err != nil {
				return Definition{}, err
			}
			return item, nil
		}
	}
	return Definition{}, fmt.Errorf("未找到工作区：%s", id)
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("生成工作区 ID 失败：%w", err)
	}
	return "workspace-" + hex.EncodeToString(raw[:]), nil
}

func pathHasCodexComponent(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return false
}

func pathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return true
	}
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
