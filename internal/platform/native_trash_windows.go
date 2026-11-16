// 中文说明：使用 Windows Shell 回收站和 `$I/$R` 条目实现可恢复移动；不执行永久删除。
//go:build windows

package platform

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"codex-chronicle.local/internal/application"
)

const (
	foDelete           = 0x0003
	fofSilent          = 0x0004
	fofNoConfirmation  = 0x0010
	fofAllowUndo       = 0x0040
	fofNoErrorUI       = 0x0400
	fofNoConfirmMkdir  = 0x0200
	maxRecycleScanTime = 2 * time.Second
)

var shell32 = syscall.NewLazyDLL("Shell32.dll")

// WindowsRecycleBinTrash 将普通文件或目录交给 Windows Shell 回收站，并记录可恢复的 $R 条目。
type WindowsRecycleBinTrash struct{}

// NewWindowsRecycleBinTrash 创建 Windows 原生 provider；不会扫描或修改回收站。
func NewWindowsRecycleBinTrash() (*WindowsRecycleBinTrash, error) {
	return &WindowsRecycleBinTrash{}, nil
}

func newNativeSystemTrash() (TrashProvider, error) {
	return NewWindowsRecycleBinTrash()
}

// Move 使用 SHFileOperationW 的 FOF_ALLOWUNDO 语义移入 Windows 回收站。
func (d *WindowsRecycleBinTrash) Move(ctx context.Context, operationID, sourceRoot, relativePath string) (TrashReceipt, error) {
	if err := ctx.Err(); err != nil {
		return TrashReceipt{}, err
	}
	if d == nil || strings.TrimSpace(operationID) == "" {
		return TrashReceipt{}, fmt.Errorf("Windows 回收站或操作 ID 无效")
	}
	source, err := application.ResolveReadOnlyPath(sourceRoot, relativePath)
	if err != nil {
		return TrashReceipt{}, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return TrashReceipt{}, err
	}
	if err := validateTrashItem(source, info); err != nil {
		return TrashReceipt{}, fmt.Errorf("Windows 回收站目标无效: %w", err)
	}
	before := time.Now().Add(-2 * time.Second)
	if err := shellMoveToRecycleBin(source); err != nil {
		return TrashReceipt{}, err
	}
	entry, err := findRecycleEntry(ctx, source, before)
	if err != nil {
		return TrashReceipt{}, fmt.Errorf("文件已进入 Windows 回收站，但无法建立恢复回执: %w", err)
	}
	return TrashReceipt{OperationID: operationID, RelativePath: filepath.ToSlash(relativePath), TrashPath: entry.dataPath}, nil
}

// Restore 校验 $I 中的原始路径后，将 $R 文件恢复到源根；拒绝覆盖新文件。
func (d *WindowsRecycleBinTrash) Restore(ctx context.Context, receipt TrashReceipt, sourceRoot string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil || receipt.OperationID == "" || receipt.RelativePath == "" {
		return fmt.Errorf("Windows 回收站恢复回执无效")
	}
	target, err := restoreDestination(sourceRoot, receipt.RelativePath)
	if err != nil {
		return err
	}
	dataPath, err := filepath.Abs(receipt.TrashPath)
	if err != nil || !isRecycleDataPath(dataPath) {
		return fmt.Errorf("Windows 回收站恢复路径无效")
	}
	metadataPath := filepath.Join(filepath.Dir(dataPath), "$I"+strings.TrimPrefix(filepath.Base(dataPath), "$R"))
	metadata, err := readRecycleMetadata(metadataPath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(metadata.originalPath), filepath.Clean(target)) {
		return fmt.Errorf("Windows 回收站原始路径与恢复目标不匹配")
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("恢复目标已存在，拒绝覆盖: %s", receipt.RelativePath)
	} else if !os.IsNotExist(err) {
		return err
	}
	info, err := os.Lstat(dataPath)
	if err != nil {
		return fmt.Errorf("Windows 回收站数据不存在: %w", err)
	}
	if err := validateTrashItem(dataPath, info); err != nil {
		return fmt.Errorf("Windows 回收站数据无效: %w", err)
	}
	if err := ensureNoSymlinkParent(sourceRoot, target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建 Windows 恢复目录失败: %w", err)
	}
	if err := movePathNoReplace(dataPath, target); err != nil {
		return fmt.Errorf("恢复 Windows 回收站文件失败: %w", err)
	}
	if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理 Windows 回收站元数据失败: %w", err)
	}
	return nil
}

type recycleEntry struct {
	dataPath string
	metadata recycleMetadata
}

type recycleMetadata struct {
	originalPath string
	deletedAt    time.Time
}

type shellFileOperation struct {
	window             uintptr
	function           uint32
	from               *uint16
	to                 *uint16
	flags              uint16
	anyOperationsAbort int32
	nameMappings       uintptr
	progressTitle      *uint16
}

func shellMoveToRecycleBin(source string) error {
	fromValues := syscall.StringToUTF16(source + "\x00")
	from := &fromValues[0]
	operation := shellFileOperation{
		function: foDelete,
		from:     from,
		flags:    fofSilent | fofNoConfirmation | fofAllowUndo | fofNoErrorUI | fofNoConfirmMkdir,
	}
	proc := shell32.NewProc("SHFileOperationW")
	result, _, callErr := proc.Call(uintptr(unsafe.Pointer(&operation)))
	if result != 0 {
		return fmt.Errorf("Windows Shell 回收站操作失败（代码 %d）: %v", result, callErr)
	}
	if operation.anyOperationsAbort != 0 {
		return fmt.Errorf("Windows Shell 回收站操作被中止")
	}
	return nil
}

func findRecycleEntry(ctx context.Context, source string, notBefore time.Time) (recycleEntry, error) {
	root := filepath.Join(filepath.VolumeName(source)+string(filepath.Separator), "$Recycle.Bin")
	deadline := time.Now().Add(maxRecycleScanTime)
	for {
		entry, err := scanRecycleRoot(root, source, notBefore)
		if err == nil {
			return entry, nil
		}
		if time.Now().After(deadline) {
			return recycleEntry{}, err
		}
		select {
		case <-ctx.Done():
			return recycleEntry{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func scanRecycleRoot(root, source string, notBefore time.Time) (recycleEntry, error) {
	users, err := os.ReadDir(root)
	if err != nil {
		return recycleEntry{}, fmt.Errorf("读取 Windows 回收站失败: %w", err)
	}
	var candidate recycleEntry
	for _, user := range users {
		if !user.IsDir() {
			continue
		}
		directory := filepath.Join(root, user.Name())
		files, readErr := os.ReadDir(directory)
		if readErr != nil {
			continue
		}
		for _, item := range files {
			name := item.Name()
			if len(name) < 3 || !strings.HasPrefix(name, "$I") {
				continue
			}
			metadataPath := filepath.Join(directory, name)
			metadata, metadataErr := readRecycleMetadata(metadataPath)
			if metadataErr != nil || !strings.EqualFold(filepath.Clean(metadata.originalPath), filepath.Clean(source)) || metadata.deletedAt.Before(notBefore) {
				continue
			}
			dataPath := filepath.Join(directory, "$R"+strings.TrimPrefix(name, "$I"))
			if info, statErr := os.Lstat(dataPath); statErr != nil || !info.Mode().IsRegular() {
				continue
			}
			if candidate.dataPath == "" || metadata.deletedAt.After(candidate.metadata.deletedAt) {
				candidate = recycleEntry{dataPath: dataPath, metadata: metadata}
			}
		}
	}
	if candidate.dataPath == "" {
		return recycleEntry{}, fmt.Errorf("未找到对应的 Windows 回收站 $I/$R 条目")
	}
	return candidate, nil
}

func readRecycleMetadata(path string) (recycleMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return recycleMetadata{}, fmt.Errorf("读取 Windows 回收站元数据失败: %w", err)
	}
	if len(data) < 24 {
		return recycleMetadata{}, fmt.Errorf("Windows 回收站元数据过短")
	}
	version := binary.LittleEndian.Uint64(data[:8])
	if version == 0 {
		return recycleMetadata{}, fmt.Errorf("Windows 回收站元数据版本无效")
	}
	fileTime := binary.LittleEndian.Uint64(data[16:24])
	pathOffset := 24
	pathBytes := data[pathOffset:]
	if version >= 2 && len(data) >= 28 {
		length := int(binary.LittleEndian.Uint32(data[24:28]))
		pathOffset = 28
		if length < 0 || length > (len(data)-pathOffset)/2 {
			return recycleMetadata{}, fmt.Errorf("Windows 回收站原始路径长度无效")
		}
		pathBytes = data[pathOffset : pathOffset+length*2]
	}
	values := make([]uint16, 0, len(pathBytes)/2)
	for index := 0; index+1 < len(pathBytes); index += 2 {
		values = append(values, binary.LittleEndian.Uint16(pathBytes[index:index+2]))
	}
	original := strings.TrimRight(string(utf16.Decode(values)), "\x00")
	if original == "" {
		return recycleMetadata{}, fmt.Errorf("Windows 回收站原始路径为空")
	}
	return recycleMetadata{originalPath: original, deletedAt: windowsFileTime(fileTime)}, nil
}

func windowsFileTime(value uint64) time.Time {
	const windowsToUnixSeconds = 11644473600
	seconds := int64(value/10_000_000) - windowsToUnixSeconds
	nanos := int64(value%10_000_000) * 100
	return time.Unix(seconds, nanos).UTC()
}

func isRecycleDataPath(path string) bool {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	return strings.HasPrefix(base, "$R") && strings.Contains(strings.ToLower(clean), strings.ToLower(string(filepath.Separator)+"$recycle.bin"+string(filepath.Separator)))
}
