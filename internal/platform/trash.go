// 中文说明：提供可注入的回收站接口、系统目录 provider 和生命周期补偿实现。
// FilesystemTrash 已覆盖 Linux/macOS 的目录语义；Windows 原生 Shell provider 仍单独保留。
package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"codex-chronicle.local/internal/lifecycle"
)

// TrashReceipt 记录可恢复移动的源路径和隔离路径。
type TrashReceipt struct {
	OperationID  string `json:"operation_id"`
	RelativePath string `json:"relative_path"`
	TrashPath    string `json:"trash_path"`
}

// TrashProvider 抽象系统回收站或测试隔离区。
type TrashProvider interface {
	Move(ctx context.Context, operationID, sourceRoot, relativePath string) (TrashReceipt, error)
	Restore(ctx context.Context, receipt TrashReceipt, sourceRoot string) error
}

// DirectoryTrash 将文件移动到指定隔离目录，适合合成 fixture 和 provider 回归。
type DirectoryTrash struct {
	Root string
}

// FilesystemTrash 使用用户指定的系统回收站目录保存可恢复文件。
// Linux 遵循 freedesktop Trash 的 files/info 目录约定；macOS 使用用户 .Trash 目录。
type FilesystemTrash struct {
	Root     string
	InfoRoot string
	OS       string
}

// NewFilesystemTrash 创建一个显式平台的文件系统回收站 provider。
// 该构造函数只操作调用方明确提供的目录，适合真实系统回收站和临时 fixture。
func NewFilesystemTrash(root, goos string) (*FilesystemTrash, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("回收站目录不能为空")
	}
	if goos != "darwin" && goos != "linux" && goos != "freebsd" && goos != "openbsd" && goos != "netbsd" {
		return nil, fmt.Errorf("平台 %s 没有可用的文件系统回收站实现", goos)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析回收站目录失败: %w", err)
	}
	if err := ensureNoSymlinkRoot(abs); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("创建回收站目录失败: %w", err)
	}
	if err := ensureNoSymlinkRoot(abs); err != nil {
		return nil, err
	}
	infoRoot := ""
	if goos != "darwin" {
		infoRoot = filepath.Join(filepath.Dir(abs), "info")
		if err := ensureNoSymlinkRoot(infoRoot); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(infoRoot, 0o700); err != nil {
			return nil, fmt.Errorf("创建回收站元数据目录失败: %w", err)
		}
		if err := ensureNoSymlinkRoot(infoRoot); err != nil {
			return nil, err
		}
	}
	return &FilesystemTrash{Root: abs, InfoRoot: infoRoot, OS: goos}, nil
}

// NewSystemTrash 根据只读平台探测结果创建系统回收站 provider。
// Windows 需要后续 Shell API provider，当前会明确返回不支持。
func NewSystemTrash() (*FilesystemTrash, error) {
	detection := DetectSystemTrash()
	if !detection.Supported {
		return nil, fmt.Errorf("当前系统回收站不可用: %s", detection.Reason)
	}
	return NewFilesystemTrash(detection.CandidatePath, runtime.GOOS)
}

// NewDirectoryTrash 创建目录回收站；不会自动扫描或删除目录中已有内容。
func NewDirectoryTrash(root string) (*DirectoryTrash, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析回收站目录失败: %w", err)
	}
	if err := ensureNoSymlinkRoot(abs); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("创建回收站目录失败: %w", err)
	}
	if err := ensureNoSymlinkRoot(abs); err != nil {
		return nil, err
	}
	return &DirectoryTrash{Root: abs}, nil
}

// Move 将普通文件或完整目录移动到系统回收站，并在 Linux 写入恢复所需的 .trashinfo。
func (d *FilesystemTrash) Move(ctx context.Context, operationID, sourceRoot, relativePath string) (TrashReceipt, error) {
	if err := ctx.Err(); err != nil {
		return TrashReceipt{}, err
	}
	if d == nil || strings.TrimSpace(operationID) == "" {
		return TrashReceipt{}, fmt.Errorf("回收站或操作 ID 无效")
	}
	source, err := restoreDestination(sourceRoot, relativePath)
	if err != nil {
		return TrashReceipt{}, err
	}
	if err := ensureNoSymlinkParent(sourceRoot, source); err != nil {
		return TrashReceipt{}, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return TrashReceipt{}, err
	}
	if err := validateTrashItem(source, info); err != nil {
		return TrashReceipt{}, fmt.Errorf("回收站目标无效: %w", err)
	}
	name := filesystemTrashName(operationID, relativePath)
	target := filepath.Join(d.Root, name)
	if err := ensureNoSymlinkParent(d.Root, target); err != nil {
		return TrashReceipt{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		return TrashReceipt{}, fmt.Errorf("回收站目标已存在: %s", relativePath)
	} else if !os.IsNotExist(err) {
		return TrashReceipt{}, err
	}
	receipt := TrashReceipt{OperationID: operationID, RelativePath: filepath.ToSlash(relativePath), TrashPath: target}
	if d.InfoRoot != "" {
		if err := d.writeTrashInfo(source, target); err != nil {
			return TrashReceipt{}, err
		}
	}
	if err := movePathNoReplace(source, target); err != nil {
		if d.InfoRoot != "" {
			_ = os.Remove(filepath.Join(d.InfoRoot, filepath.Base(target)+".trashinfo"))
		}
		return TrashReceipt{}, fmt.Errorf("移动资产到系统回收站失败: %w", err)
	}
	return receipt, nil
}

// Restore 根据回收站回执恢复文件，拒绝覆盖新建目标。
func (d *FilesystemTrash) Restore(ctx context.Context, receipt TrashReceipt, sourceRoot string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil || receipt.OperationID == "" || receipt.RelativePath == "" {
		return fmt.Errorf("恢复回执无效")
	}
	trashPath, err := filepath.Abs(receipt.TrashPath)
	root, rootErr := filepath.Abs(d.Root)
	if err != nil || rootErr != nil || !pathWithinRoot(root, trashPath) {
		return fmt.Errorf("恢复回执越出回收站根")
	}
	if err := ensureNoSymlinkParent(root, trashPath); err != nil {
		return err
	}
	target, err := restoreDestination(sourceRoot, receipt.RelativePath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(trashPath)
	if err != nil {
		return fmt.Errorf("回收站源不存在: %w", err)
	}
	if err := validateTrashItem(trashPath, info); err != nil {
		return fmt.Errorf("恢复回收站源无效: %w", err)
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("恢复目标已存在，拒绝覆盖: %s", receipt.RelativePath)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ensureNoSymlinkParent(sourceRoot, target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建恢复目标目录失败: %w", err)
	}
	if err := movePathNoReplace(trashPath, target); err != nil {
		return fmt.Errorf("恢复资产失败: %w", err)
	}
	if d.InfoRoot != "" {
		if err := os.Remove(filepath.Join(d.InfoRoot, filepath.Base(trashPath)+".trashinfo")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("恢复后清理回收站元数据失败: %w", err)
		}
	}
	return nil
}

func (d *FilesystemTrash) writeTrashInfo(source, target string) error {
	original, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	// freedesktop Trash 规范要求 Path 是 URI 转义后的绝对路径，不包含 file:// scheme。
	originalPath := (&url.URL{Path: original}).EscapedPath()
	data := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n", originalPath, time.Now().Format("2006-01-02T15:04:05"))
	infoPath := filepath.Join(d.InfoRoot, filepath.Base(target)+".trashinfo")
	file, err := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("写入回收站元数据失败: %w", err)
	}
	if _, err := file.WriteString(data); err != nil {
		_ = file.Close()
		_ = os.Remove(infoPath)
		return fmt.Errorf("写入回收站元数据失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(infoPath)
		return fmt.Errorf("同步回收站元数据失败: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(infoPath)
		return fmt.Errorf("关闭回收站元数据失败: %w", err)
	}
	return nil
}

func filesystemTrashName(operationID, relativePath string) string {
	digest := sha256.Sum256([]byte(operationID + "\x00" + filepath.ToSlash(relativePath)))
	extension := filepath.Ext(filepath.Base(relativePath))
	if len(extension) > 16 {
		extension = ""
	}
	return "codex-chronicle-" + hex.EncodeToString(digest[:10]) + extension
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// Move 原子地把一个普通文件或完整目录移入 operation 专属隔离目录，不覆盖已有目标。
func (d *DirectoryTrash) Move(ctx context.Context, operationID, sourceRoot, relativePath string) (TrashReceipt, error) {
	if err := ctx.Err(); err != nil {
		return TrashReceipt{}, err
	}
	if d == nil || strings.TrimSpace(operationID) == "" {
		return TrashReceipt{}, fmt.Errorf("回收站或操作 ID 无效")
	}
	source, err := restoreDestination(sourceRoot, relativePath)
	if err != nil {
		return TrashReceipt{}, err
	}
	if err := ensureNoSymlinkParent(sourceRoot, source); err != nil {
		return TrashReceipt{}, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return TrashReceipt{}, err
	}
	if err := validateTrashItem(source, info); err != nil {
		return TrashReceipt{}, fmt.Errorf("回收站目标无效: %w", err)
	}
	target := filepath.Join(d.Root, operationID, "payload", filepath.FromSlash(relativePath))
	if err := ensureNoSymlinkParent(d.Root, target); err != nil {
		return TrashReceipt{}, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return TrashReceipt{}, fmt.Errorf("创建回收站目标目录失败: %w", err)
	}
	if _, err := os.Lstat(target); err == nil {
		return TrashReceipt{}, fmt.Errorf("回收站目标已存在: %s", relativePath)
	} else if !os.IsNotExist(err) {
		return TrashReceipt{}, fmt.Errorf("检查回收站目标失败: %w", err)
	}
	if err := movePathNoReplace(source, target); err != nil {
		return TrashReceipt{}, fmt.Errorf("移动资产到回收站失败: %w", err)
	}
	return TrashReceipt{OperationID: operationID, RelativePath: filepath.ToSlash(relativePath), TrashPath: target}, nil
}

// Restore 把隔离文件放回原始相对路径；目标已存在时拒绝覆盖。
func (d *DirectoryTrash) Restore(ctx context.Context, receipt TrashReceipt, sourceRoot string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil || receipt.OperationID == "" || receipt.RelativePath == "" {
		return fmt.Errorf("恢复回执无效")
	}
	root, err := filepath.Abs(d.Root)
	if err != nil {
		return fmt.Errorf("解析回收站根失败: %w", err)
	}
	trashPath, err := filepath.Abs(receipt.TrashPath)
	if err != nil || !pathWithinRoot(root, trashPath) {
		return fmt.Errorf("恢复回执越出回收站根")
	}
	if err := ensureNoSymlinkParent(root, trashPath); err != nil {
		return err
	}
	target, err := restoreDestination(sourceRoot, receipt.RelativePath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(trashPath)
	if err != nil {
		return fmt.Errorf("回收站源不存在: %w", err)
	}
	if err := validateTrashItem(trashPath, info); err != nil {
		return fmt.Errorf("恢复回收站源无效: %w", err)
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("恢复目标已存在，拒绝覆盖: %s", receipt.RelativePath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查恢复目标失败: %w", err)
	}
	if err := ensureNoSymlinkParent(sourceRoot, target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建恢复目标目录失败: %w", err)
	}
	if err := movePathNoReplace(trashPath, target); err != nil {
		return fmt.Errorf("恢复资产失败: %w", err)
	}
	return nil
}

func validateTrashItem(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("不接受符号链接")
	}
	if info.Mode().IsRegular() {
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("只接受普通文件或目录")
	}
	return filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		childInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if childInfo.Mode()&os.ModeSymlink != 0 || (!entry.IsDir() && !childInfo.Mode().IsRegular()) {
			return fmt.Errorf("目录包含符号链接或特殊文件: %s", current)
		}
		return nil
	})
}

func movePathNoReplace(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return moveFileNoReplace(source, target)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("移动源必须是普通文件或目录")
	}
	return renamePathNoReplace(source, target)
}

// ExecuteTrash 按计划移动全部资产；中途失败时回滚已经移动的资产。
func moveFileNoReplace(source, target string) error {
	original, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("读取移动源失败: %w", err)
	}
	if original.Mode()&os.ModeSymlink != 0 || !original.Mode().IsRegular() {
		return fmt.Errorf("移动源必须是普通文件")
	}
	if err := os.Link(source, target); err != nil {
		return fmt.Errorf("创建不覆盖移动目标失败（源和目标需位于同一文件系统且支持硬链接）: %w", err)
	}
	targetInfo, targetErr := os.Lstat(target)
	sourceInfo, sourceErr := os.Lstat(source)
	if targetErr != nil || sourceErr != nil || !os.SameFile(original, targetInfo) || !os.SameFile(original, sourceInfo) {
		if targetErr == nil && os.SameFile(original, targetInfo) {
			_ = os.Remove(target)
		}
		return fmt.Errorf("移动期间源文件发生变化，已保留源文件")
	}
	if err := os.Remove(source); err != nil {
		if currentTarget, statErr := os.Lstat(target); statErr == nil && os.SameFile(original, currentTarget) {
			_ = os.Remove(target)
		}
		return fmt.Errorf("移除已链接源文件失败，已尝试清理目标: %w", err)
	}
	return nil
}

func ExecuteTrash(ctx context.Context, plan lifecycle.OperationPlan, provider TrashProvider) ([]TrashReceipt, error) {
	return ExecuteTrashWithJournal(ctx, plan, provider, nil)
}

// ExecuteTrashWithJournal 按文件移动并在每次成功移动后回调，供 journal 逐项落盘回执。
// 回调失败时会把当前批次已移动的文件按逆序尝试恢复。
func ExecuteTrashWithJournal(ctx context.Context, plan lifecycle.OperationPlan, provider TrashProvider, onReceipt func(TrashReceipt) error) ([]TrashReceipt, error) {
	if provider == nil {
		return nil, fmt.Errorf("未提供回收站 provider")
	}
	if !containsTrash(plan) {
		return nil, fmt.Errorf("计划没有回收站动作")
	}
	if err := lifecycle.RevalidatePlan(ctx, plan); err != nil {
		return nil, err
	}
	fileAssets := plan.FileAssets()
	receipts := make([]TrashReceipt, 0, len(fileAssets))
	for _, asset := range fileAssets {
		receipt, err := provider.Move(ctx, plan.OperationID, plan.Root, asset.RelativePath)
		if err != nil {
			for index := len(receipts) - 1; index >= 0; index-- {
				_ = provider.Restore(context.Background(), receipts[index], plan.Root)
			}
			return nil, fmt.Errorf("回收站操作失败并已尝试回滚: %w", err)
		}
		receipts = append(receipts, receipt)
		if onReceipt != nil {
			if callbackErr := onReceipt(receipt); callbackErr != nil {
				for index := len(receipts) - 1; index >= 0; index-- {
					_ = provider.Restore(context.Background(), receipts[index], plan.Root)
				}
				return nil, fmt.Errorf("记录回收站回执失败并已尝试回滚: %w", callbackErr)
			}
		}
	}
	return receipts, nil
}

func containsTrash(plan lifecycle.OperationPlan) bool {
	for _, action := range plan.Actions {
		if action == lifecycle.ActionTrash {
			return true
		}
	}
	return false
}

func ensureNoSymlinkParent(root, target string) error {
	if err := ensureNoSymlinkRoot(root); err != nil {
		return err
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("回收站目标越出根目录")
	}
	current := root
	parts := strings.Split(filepath.Dir(relative), string(filepath.Separator))
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("回收站路径包含符号链接")
		}
	}
	return nil
}

func ensureNoSymlinkRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("解析回收站根失败: %w", err)
	}
	current := filepath.Clean(abs)
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("回收站根路径包含符号链接或非目录: %s", current)
			}
			return nil
		}
		if !os.IsNotExist(statErr) {
			return fmt.Errorf("检查回收站根失败: %w", statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func restoreDestination(root, relative string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("恢复路径越出源根")
	}
	current := abs
	for _, part := range strings.Split(filepath.Dir(clean), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("恢复路径包含符号链接")
		}
	}
	return filepath.Join(abs, clean), nil
}
