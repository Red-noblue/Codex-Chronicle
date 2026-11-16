// 中文说明：提供本地归档文件的预览、解密和无覆盖恢复；本文件不修改真实 .codex 控制面。
package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"codex-chronicle.local/internal/lifecycle"
)

// RestoreFile 描述一个待恢复的普通文件；控制面快照由 adapter 单独处理。
type RestoreFile struct {
	RelativePath string `json:"relative_path"`
	TargetPath   string `json:"target_path"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
}

// RestorePreview 是恢复执行前的只读冲突清单。
type RestorePreview struct {
	Version              int           `json:"version"`
	ArchiveDirectory     string        `json:"archive_directory"`
	TargetRoot           string        `json:"target_root"`
	Encrypted            bool          `json:"encrypted"`
	Files                []RestoreFile `json:"files"`
	Conflicts            []string      `json:"conflicts,omitempty"`
	ControlPlaneSnapshot bool          `json:"control_plane_snapshot"`
	Ready                bool          `json:"ready"`
}

// RestoreResult 是文件恢复发布后的可验证结果。
type RestoreResult struct {
	ArchiveDirectory     string        `json:"archive_directory"`
	TargetRoot           string        `json:"target_root"`
	Encrypted            bool          `json:"encrypted"`
	RestoredFiles        []RestoreFile `json:"restored_files"`
	Manifest             Manifest      `json:"manifest"`
	Verified             bool          `json:"verified"`
	ControlPlaneSnapshot bool          `json:"control_plane_snapshot"`
}

// VerifyRestoredFiles 只读核对恢复目标中的普通文件；用于进程中断后的恢复 journal 续接。
func VerifyRestoredFiles(ctx context.Context, files []RestoreFile) error {
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(file.TargetPath)
		if err != nil {
			return fmt.Errorf("恢复目标缺少文件 %s: %w", file.RelativePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("恢复目标文件 %s 不是普通文件", file.RelativePath)
		}
		if info.Size() != file.SizeBytes {
			return fmt.Errorf("恢复目标文件 %s 大小不匹配", file.RelativePath)
		}
		actual, hashErr := hashFile(ctx, file.TargetPath)
		if hashErr != nil {
			return fmt.Errorf("读取恢复目标文件 %s 失败: %w", file.RelativePath, hashErr)
		}
		if actual != file.SHA256 {
			return fmt.Errorf("恢复目标文件 %s 哈希不匹配", file.RelativePath)
		}
	}
	return nil
}

// PreviewLocalRestore 校验本地归档并列出目标路径冲突，不写入归档或目标目录。
func PreviewLocalRestore(ctx context.Context, directory, targetRoot, password string) (RestorePreview, error) {
	manifest, archiveRoot, target, err := loadRestoreManifest(ctx, directory, targetRoot, password)
	if err != nil {
		return RestorePreview{}, err
	}
	preview := RestorePreview{
		Version: 1, ArchiveDirectory: archiveRoot, TargetRoot: target, Encrypted: manifest.Encrypted,
		Files: make([]RestoreFile, 0, len(manifest.Assets)), Conflicts: make([]string, 0),
		ControlPlaneSnapshot: manifest.ControlPlaneSnapshot != nil,
	}
	for _, asset := range manifest.Assets {
		targetPath, pathErr := restoreTargetPath(target, asset.RelativePath)
		if pathErr != nil {
			preview.Conflicts = append(preview.Conflicts, fmt.Sprintf("%s: %v", asset.RelativePath, pathErr))
			continue
		}
		item := RestoreFile{RelativePath: asset.RelativePath, TargetPath: targetPath, SizeBytes: asset.SizeBytes, SHA256: asset.SHA256}
		preview.Files = append(preview.Files, item)
		if info, statErr := os.Lstat(targetPath); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				preview.Conflicts = append(preview.Conflicts, fmt.Sprintf("%s: 目标是符号链接，拒绝覆盖", asset.RelativePath))
			} else if !info.Mode().IsRegular() {
				preview.Conflicts = append(preview.Conflicts, fmt.Sprintf("%s: 目标不是普通文件", asset.RelativePath))
			} else {
				preview.Conflicts = append(preview.Conflicts, fmt.Sprintf("%s: 目标文件已存在，拒绝覆盖", asset.RelativePath))
			}
		} else if !os.IsNotExist(statErr) {
			preview.Conflicts = append(preview.Conflicts, fmt.Sprintf("%s: 检查目标失败: %v", asset.RelativePath, statErr))
		}
	}
	preview.Ready = len(preview.Conflicts) == 0
	return preview, nil
}

// RestoreLocalFiles 将归档普通文件恢复到目标根；目标中的既有文件不会被覆盖。
// 目标根可以是已存在的 fixture 目录，但真实 .codex 路径会被拒绝。
func RestoreLocalFiles(ctx context.Context, directory, targetRoot, password string) (RestoreResult, error) {
	preview, err := PreviewLocalRestore(ctx, directory, targetRoot, password)
	if err != nil {
		return RestoreResult{}, err
	}
	if !preview.Ready {
		return RestoreResult{}, fmt.Errorf("归档恢复存在目标冲突: %s", strings.Join(preview.Conflicts, "；"))
	}
	manifest, _, target, err := loadRestoreManifest(ctx, directory, targetRoot, password)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := ensureRestoreParent(target); err != nil {
		return RestoreResult{}, err
	}
	parent := filepath.Dir(target)
	stage, err := os.MkdirTemp(parent, ".codex-chronicle-restore-*")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("创建恢复临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	for _, file := range preview.Files {
		if err := ctx.Err(); err != nil {
			return RestoreResult{}, err
		}
		source := filepath.Join(preview.ArchiveDirectory, "payload", filepath.FromSlash(file.RelativePath))
		staged := filepath.Join(stage, filepath.FromSlash(file.RelativePath))
		if err := ensureRestoreParent(staged); err != nil {
			return RestoreResult{}, err
		}
		if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
			return RestoreResult{}, fmt.Errorf("创建恢复临时文件目录失败: %w", err)
		}
		if err := ensureRestoreParent(staged); err != nil {
			return RestoreResult{}, err
		}
		asset := lifecycle.AssetFingerprint{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, SHA256: file.SHA256}
		if manifest.Encrypted {
			if err := decryptAndVerifyToFile(ctx, source, staged, asset, manifest, password); err != nil {
				return RestoreResult{}, err
			}
		} else if err := copyAndVerify(ctx, source, staged, asset); err != nil {
			return RestoreResult{}, err
		}
	}
	if err := publishRestoreStage(ctx, stage, target, preview.Files); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{
		ArchiveDirectory: preview.ArchiveDirectory, TargetRoot: target, Encrypted: manifest.Encrypted,
		RestoredFiles: preview.Files, Manifest: manifest, Verified: true,
		ControlPlaneSnapshot: manifest.ControlPlaneSnapshot != nil,
	}, nil
}

func loadRestoreManifest(ctx context.Context, directory, targetRoot, password string) (Manifest, string, string, error) {
	archiveRoot, err := filepath.Abs(directory)
	if err != nil {
		return Manifest{}, "", "", fmt.Errorf("解析归档目录失败: %w", err)
	}
	archiveRoot = filepath.Clean(archiveRoot)
	if err := ensureArchiveRoot(archiveRoot); err != nil {
		return Manifest{}, "", "", err
	}
	target, err := filepath.Abs(targetRoot)
	if err != nil {
		return Manifest{}, "", "", fmt.Errorf("解析恢复目标失败: %w", err)
	}
	target = filepath.Clean(target)
	if strings.TrimSpace(targetRoot) == "" {
		return Manifest{}, "", "", fmt.Errorf("恢复目标不能为空")
	}
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return Manifest{}, "", "", fmt.Errorf("恢复目标根必须是普通目录")
		}
	} else if !os.IsNotExist(statErr) {
		return Manifest{}, "", "", fmt.Errorf("检查恢复目标根失败: %w", statErr)
	}
	if err := ensureRestoreParent(target); err != nil {
		return Manifest{}, "", "", err
	}
	archiveCanonical, err := resolveRestorePath(archiveRoot)
	if err != nil {
		return Manifest{}, "", "", fmt.Errorf("解析归档真实路径失败: %w", err)
	}
	targetCanonical, err := resolveRestorePath(target)
	if err != nil {
		return Manifest{}, "", "", fmt.Errorf("解析恢复目标真实路径失败: %w", err)
	}
	if hasCodexPathComponent(archiveCanonical) || hasCodexPathComponent(targetCanonical) {
		return Manifest{}, "", "", fmt.Errorf("恢复目标和归档目录不能位于 .codex 路径")
	}
	if pathWithin(archiveCanonical, targetCanonical) || pathWithin(targetCanonical, archiveCanonical) {
		return Manifest{}, "", "", fmt.Errorf("归档目录和恢复目标不能相互包含")
	}
	var manifest Manifest
	if strings.TrimSpace(password) != "" {
		manifest, err = VerifyLocalEncrypted(ctx, archiveRoot, password)
	} else {
		manifest, err = VerifyLocal(ctx, archiveRoot)
	}
	if err != nil {
		return Manifest{}, "", "", fmt.Errorf("校验归档失败: %w", err)
	}
	if manifest.Encrypted && strings.TrimSpace(password) == "" {
		return Manifest{}, "", "", fmt.Errorf("这是加密归档，恢复需要密码")
	}
	if manifest.CloudBackup != nil {
		return Manifest{}, "", "", fmt.Errorf("这是完整云备份，请使用会话库云端备份的“下载并校验”；尚不支持直接恢复到 Codex")
	}
	return manifest, archiveRoot, target, nil
}

func ensureArchiveRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("读取归档目录失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("归档根必须是普通目录")
	}
	return ensureRestoreParent(path)
}

func restoreTargetPath(root, relative string) (string, error) {
	clean, err := safePayloadRelative(relative)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("恢复路径越出目标根")
	}
	if err := ensureRestoreParent(target); err != nil {
		return "", err
	}
	if err := ensureNoRestoreSymlinkBelowRoot(root, target); err != nil {
		return "", err
	}
	return target, nil
}

func ensureRestoreParent(path string) error {
	current := filepath.Clean(filepath.Dir(path))
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				resolved, resolveErr := filepath.EvalSymlinks(current)
				if resolveErr != nil {
					return fmt.Errorf("解析恢复路径父目录符号链接失败: %w", resolveErr)
				}
				resolvedInfo, statErr := os.Stat(resolved)
				if statErr != nil || !resolvedInfo.IsDir() {
					return fmt.Errorf("恢复路径父目录符号链接目标不是目录: %s", current)
				}
			} else if !info.IsDir() {
				return fmt.Errorf("恢复路径父目录不安全: %s", current)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查恢复路径父目录失败: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func resolveRestorePath(path string) (string, error) {
	clean := filepath.Clean(path)
	missing := make([]string, 0)
	current := clean
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return clean, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func ensureNoRestoreSymlinkBelowRoot(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("恢复路径越出目标根")
	}
	current := filepath.Clean(root)
	for _, part := range strings.Split(filepath.Dir(relative), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("检查恢复路径失败: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("恢复路径包含符号链接: %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("恢复路径父项不是目录: %s", current)
		}
	}
	return nil
}

func publishRestoreStage(ctx context.Context, stage, target string, files []RestoreFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	targetCreated := false
	if info, err := os.Lstat(target); os.IsNotExist(err) {
		if err := os.Mkdir(target, 0o700); err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("恢复目标在预览后出现，拒绝覆盖")
			}
			return fmt.Errorf("创建恢复目标目录失败: %w", err)
		}
		targetCreated = true
	} else if err != nil {
		return fmt.Errorf("检查恢复目标失败: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("恢复目标不是普通目录")
	}
	created := make([]string, 0, len(files))
	createdDirs := make([]string, 0)
	rollback := func() error {
		var first error
		for index := len(created) - 1; index >= 0; index-- {
			if removeErr := os.Remove(created[index]); removeErr != nil && !os.IsNotExist(removeErr) && first == nil {
				first = removeErr
			}
		}
		for index := len(createdDirs) - 1; index >= 0; index-- {
			if removeErr := os.Remove(createdDirs[index]); removeErr != nil && !os.IsNotExist(removeErr) && first == nil {
				first = removeErr
			}
		}
		if targetCreated {
			if removeErr := os.Remove(target); removeErr != nil && !os.IsNotExist(removeErr) && first == nil {
				first = removeErr
			}
		}
		return first
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			_ = rollback()
			return err
		}
		source := filepath.Join(stage, filepath.FromSlash(file.RelativePath))
		targetPath := filepath.Join(target, filepath.FromSlash(file.RelativePath))
		if err := ensureNoRestoreSymlinkBelowRoot(target, targetPath); err != nil {
			_ = rollback()
			return err
		}
		newDirs, err := ensureRestoreDirectories(target, filepath.Dir(targetPath))
		createdDirs = append(createdDirs, newDirs...)
		if err != nil {
			_ = rollback()
			return fmt.Errorf("创建恢复目标父目录失败: %w", err)
		}
		if err := linkOrCopyNoReplace(source, targetPath); err != nil {
			_ = rollback()
			return fmt.Errorf("发布恢复文件 %s 失败: %w", file.RelativePath, err)
		}
		created = append(created, targetPath)
	}
	return nil
}

func ensureRestoreDirectories(root, directory string) ([]string, error) {
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("恢复目录越出目标根")
	}
	current := filepath.Clean(root)
	missing := make([]string, 0)
	if relative != "." {
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			if part == "." || part == "" {
				continue
			}
			current = filepath.Join(current, part)
			info, statErr := os.Lstat(current)
			if statErr == nil {
				if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					return nil, fmt.Errorf("恢复目标父目录不安全: %s", current)
				}
				continue
			}
			if !os.IsNotExist(statErr) {
				return nil, fmt.Errorf("检查恢复目标父目录失败: %w", statErr)
			}
			missing = append(missing, current)
		}
	}
	created := make([]string, 0, len(missing))
	for _, path := range missing {
		if err := os.Mkdir(path, 0o700); err != nil {
			if os.IsExist(err) {
				info, statErr := os.Lstat(path)
				if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					return created, fmt.Errorf("恢复目标父目录在创建时变得不安全: %s", path)
				}
				continue
			}
			return created, err
		}
		created = append(created, path)
	}
	return created, nil
}

func linkOrCopyNoReplace(source, target string) error {
	linkErr := os.Link(source, target)
	if linkErr == nil {
		return os.Remove(source)
	} else if !os.IsExist(linkErr) {
		// 某些文件系统或 Windows 配置不支持硬链接，退化到 O_EXCL 流式复制。
		input, openErr := os.Open(source)
		if openErr != nil {
			return openErr
		}
		defer input.Close()
		output, createErr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return createErr
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			_ = os.Remove(target)
			return copyErr
		}
		if closeErr != nil {
			_ = os.Remove(target)
			return closeErr
		}
		return os.Remove(source)
	}
	return linkErr
}

func decryptAndVerifyToFile(ctx context.Context, source, target string, expected lifecycle.AssetFingerprint, manifest Manifest, password string) error {
	if strings.TrimSpace(password) == "" || manifest.Encryption == nil {
		return fmt.Errorf("解密归档参数无效")
	}
	salt, err := hex.DecodeString(manifest.Encryption.Salt)
	if err != nil || len(salt) == 0 {
		return fmt.Errorf("归档加密盐无效")
	}
	key := deriveKey([]byte(password), salt, manifest.Encryption.Iterations, 32)
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("打开加密归档文件失败: %w", err)
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建恢复临时文件失败: %w", err)
	}
	cleanup := func() { _ = output.Close(); _ = os.Remove(target) }
	gcm, err := newGCM(key)
	if err != nil {
		cleanup()
		return err
	}
	header := make([]byte, len(encryptedPayloadMagic))
	if _, err := io.ReadFull(input, header); err != nil || string(header) != string(encryptedPayloadMagic) {
		cleanup()
		return fmt.Errorf("加密归档头无效")
	}
	hash := sha256.New()
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			cleanup()
			return err
		}
		var length [4]byte
		if _, err := io.ReadFull(input, length[:]); err != nil {
			cleanup()
			return fmt.Errorf("读取加密归档分块长度失败: %w", err)
		}
		plainLength := uint64(length[0])<<24 | uint64(length[1])<<16 | uint64(length[2])<<8 | uint64(length[3])
		if plainLength == 0 {
			break
		}
		if plainLength > encryptionChunkBytes {
			cleanup()
			return fmt.Errorf("加密归档分块过大")
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err := io.ReadFull(input, nonce); err != nil {
			cleanup()
			return fmt.Errorf("读取加密归档 nonce 失败: %w", err)
		}
		ciphertext := make([]byte, int(plainLength)+gcm.Overhead())
		if _, err := io.ReadFull(input, ciphertext); err != nil {
			cleanup()
			return fmt.Errorf("读取加密归档分块失败: %w", err)
		}
		plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			cleanup()
			return fmt.Errorf("解密归档分块失败: %w", err)
		}
		if _, err := output.Write(plaintext); err != nil {
			cleanup()
			return fmt.Errorf("写入恢复临时文件失败: %w", err)
		}
		_, _ = hash.Write(plaintext)
		copied += int64(len(plaintext))
	}
	var trailing [1]byte
	if read, err := input.Read(trailing[:]); err != io.EOF || read != 0 {
		cleanup()
		return fmt.Errorf("加密归档存在结束标记后的额外数据")
	}
	if err := output.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(target)
		return err
	}
	if copied != expected.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		_ = os.Remove(target)
		return fmt.Errorf("恢复文件 %s 哈希或大小不匹配", expected.RelativePath)
	}
	return nil
}
