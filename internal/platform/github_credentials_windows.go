//go:build windows

// 中文说明：通过 Windows Credential Manager 读写 GitHub 凭证；不使用临时文件或命令行参数传递令牌。
package platform

import (
	"context"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"codex-chronicle.local/internal/archive"
)

const (
	credTypeGeneric          = 1
	credPersistLocalMachine  = 2
	errorNotFound            = syscall.Errno(1168)
	maxWindowsCredentialSize = 512
)

type windowsCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32       = syscall.NewLazyDLL("Advapi32.dll")
	procCredRead   = advapi32.NewProc("CredReadW")
	procCredWrite  = advapi32.NewProc("CredWriteW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

func loadSystemGitHubCredential(ctx context.Context, service, account string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target, err := windowsCredentialTarget(service, account)
	if err != nil {
		return "", err
	}
	var credentialPtr *windowsCredential
	result, _, callErr := procCredRead.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&credentialPtr)))
	runtime.KeepAlive(target)
	if result == 0 {
		if callErr == errorNotFound {
			return "", archive.ErrGitHubCredentialNotFound
		}
		return "", fmt.Errorf("读取 Windows Credential Manager 凭证失败")
	}
	if credentialPtr == nil {
		return "", archive.ErrGitHubCredentialNotFound
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(credentialPtr)))
	if credentialPtr.CredentialBlob == nil || credentialPtr.CredentialBlobSize == 0 {
		return "", archive.ErrGitHubCredentialNotFound
	}
	if credentialPtr.CredentialBlobSize > maxWindowsCredentialSize {
		return "", fmt.Errorf("Windows Credential Manager 凭证过大")
	}
	bytes := unsafe.Slice(credentialPtr.CredentialBlob, credentialPtr.CredentialBlobSize)
	return string(bytes), nil
}

func saveSystemGitHubCredential(ctx context.Context, service, account, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len([]byte(token)) > maxWindowsCredentialSize {
		return fmt.Errorf("Windows Credential Manager 凭证过大")
	}
	target, err := windowsCredentialTarget(service, account)
	if err != nil {
		return err
	}
	userName, err := syscall.UTF16PtrFromString(account)
	if err != nil {
		return fmt.Errorf("Windows 凭证账户无效")
	}
	blob := []byte(token)
	credential := windowsCredential{
		Type: credTypeGeneric, TargetName: target, CredentialBlobSize: uint32(len(blob)), Persist: credPersistLocalMachine,
		UserName: userName,
	}
	if len(blob) > 0 {
		credential.CredentialBlob = &blob[0]
	}
	result, _, _ := procCredWrite.Call(uintptr(unsafe.Pointer(&credential)), 0)
	runtime.KeepAlive(blob)
	runtime.KeepAlive(target)
	runtime.KeepAlive(userName)
	if result == 0 {
		return fmt.Errorf("写入 Windows Credential Manager 凭证失败")
	}
	return nil
}

func deleteSystemGitHubCredential(ctx context.Context, service, account string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := windowsCredentialTarget(service, account)
	if err != nil {
		return err
	}
	result, _, callErr := procCredDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	runtime.KeepAlive(target)
	if result == 0 && callErr != errorNotFound {
		return fmt.Errorf("删除 Windows Credential Manager 凭证失败")
	}
	return nil
}

func windowsCredentialTarget(service, account string) (*uint16, error) {
	target := "CodexChronicle/" + service + "/" + account
	value, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return nil, fmt.Errorf("Windows 凭证目标无效")
	}
	return value, nil
}
