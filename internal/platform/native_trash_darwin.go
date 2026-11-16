//go:build darwin && cgo

// 中文说明：通过 Foundation 原生回收站 API 移动 macOS 文件，并保留可验证的恢复回执。
package platform

/*
#cgo LDFLAGS: -framework Foundation
#include <stdlib.h>
#include "native_trash_darwin.h"
*/
import "C"

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

type darwinNativeTrash struct{}

func newNativeSystemTrash() (TrashProvider, error) {
	return &darwinNativeTrash{}, nil
}

func (d *darwinNativeTrash) Move(ctx context.Context, operationID, sourceRoot, relativePath string) (TrashReceipt, error) {
	if err := ctx.Err(); err != nil {
		return TrashReceipt{}, err
	}
	if d == nil || strings.TrimSpace(operationID) == "" {
		return TrashReceipt{}, fmt.Errorf("macOS 回收站或操作 ID 无效")
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
	if err := validateDarwinTrashItem(source, info); err != nil {
		return TrashReceipt{}, err
	}
	trashRoot, err := darwinTrashDirectory(source)
	if err != nil {
		return TrashReceipt{}, fmt.Errorf("定位 macOS 系统回收站失败: %w", err)
	}
	trashPath, err := darwinTrashItem(source)
	if err != nil {
		return TrashReceipt{}, fmt.Errorf("移入 macOS 系统回收站失败: %w", err)
	}
	if !pathWithinRoot(trashRoot, trashPath) {
		if restoreErr := moveDarwinTrashItemNoReplace(trashPath, source); restoreErr != nil {
			return TrashReceipt{}, fmt.Errorf("文件已移动到未预期的回收站路径 %q，自动恢复失败: %w", trashPath, restoreErr)
		}
		return TrashReceipt{}, fmt.Errorf("Foundation 返回的回收站路径不属于源文件所在卷的系统回收站")
	}
	return TrashReceipt{OperationID: operationID, RelativePath: filepath.ToSlash(relativePath), TrashPath: trashPath}, nil
}

func (d *darwinNativeTrash) Restore(ctx context.Context, receipt TrashReceipt, sourceRoot string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil || strings.TrimSpace(receipt.OperationID) == "" || strings.TrimSpace(receipt.RelativePath) == "" {
		return fmt.Errorf("macOS 回收站恢复回执无效")
	}
	target, err := restoreDestination(sourceRoot, receipt.RelativePath)
	if err != nil {
		return err
	}
	trashRoot, err := darwinTrashDirectory(sourceRoot)
	if err != nil {
		return fmt.Errorf("定位 macOS 系统回收站失败: %w", err)
	}
	trashPath, err := filepath.Abs(receipt.TrashPath)
	if err != nil || !pathWithinRoot(trashRoot, trashPath) {
		return fmt.Errorf("macOS 回收站恢复路径无效")
	}
	if err := ensureNoSymlinkParent(trashRoot, trashPath); err != nil {
		return err
	}
	info, err := os.Lstat(trashPath)
	if err != nil {
		return fmt.Errorf("macOS 回收站源不存在: %w", err)
	}
	if err := validateDarwinTrashItem(trashPath, info); err != nil {
		return err
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
		return fmt.Errorf("创建 macOS 恢复目录失败: %w", err)
	}
	if err := moveDarwinTrashItemNoReplace(trashPath, target); err != nil {
		return fmt.Errorf("恢复 macOS 回收站文件失败: %w", err)
	}
	return nil
}

// validateDarwinTrashItem 同时支持普通文件和完整回收包目录，拒绝目录中的链接或特殊文件。
func validateDarwinTrashItem(path string, info os.FileInfo) error {
	if info.Mode().IsRegular() {
		return nil
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("macOS 回收站只接受普通文件或无链接目录")
	}
	return filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("回收文件夹包含链接或特殊文件")
		}
		return nil
	})
}

func moveDarwinTrashItemNoReplace(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return moveFileNoReplace(source, target)
	}
	// macOS 的排他重命名拒绝覆盖已存在的目录，避免检查与移动之间的覆盖窗口。
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_EXCL)
}

func darwinTrashDirectory(itemPath string) (string, error) {
	return findDarwinTrashDirectory(itemPath, false)
}

// NativeTrashDirectory 以只读方式定位源文件所在卷的废纸篓，不创建目录。
func NativeTrashDirectory(itemPath string) (string, error) {
	return findDarwinTrashDirectory(itemPath, true)
}

func findDarwinTrashDirectory(itemPath string, readOnly bool) (string, error) {
	item := C.CString(itemPath)
	defer C.free(unsafe.Pointer(item))
	var result *C.char
	var failure *C.char
	var status C.int
	if readOnly {
		status = C.cc_trash_directory_readonly(item, &result, &failure)
	} else {
		status = C.cc_trash_directory(item, &result, &failure)
	}
	defer C.free(unsafe.Pointer(result))
	defer C.free(unsafe.Pointer(failure))
	if status != 0 || result == nil {
		return "", darwinNativeError(failure, "Foundation 未返回系统回收站路径")
	}
	return filepath.Clean(C.GoString(result)), nil
}

func darwinTrashItem(sourcePath string) (string, error) {
	source := C.CString(sourcePath)
	defer C.free(unsafe.Pointer(source))
	var result *C.char
	var failure *C.char
	status := C.cc_trash_item(source, &result, &failure)
	defer C.free(unsafe.Pointer(result))
	defer C.free(unsafe.Pointer(failure))
	if status != 0 || result == nil {
		return "", darwinNativeError(failure, "Foundation 未返回回收站目标路径")
	}
	return filepath.Clean(C.GoString(result)), nil
}

func darwinNativeError(message *C.char, fallback string) error {
	if message != nil {
		if value := strings.TrimSpace(C.GoString(message)); value != "" {
			return fmt.Errorf("%s", value)
		}
	}
	return fmt.Errorf("%s", fallback)
}
