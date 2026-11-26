// 中文说明：安全解压离线更新包，拒绝路径穿越、链接和特殊文件，并限制展开资源。
package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ExtractionLimits 防止恶意或意外的离线包耗尽文件数、磁盘和内存预算。
type ExtractionLimits struct {
	MaxFiles     int
	MaxBytes     int64
	MaxFileBytes int64
	MaxPathBytes int
}

// DefaultExtractionLimits 是离线更新包的保守默认上限；发布包可通过 API 显式使用更小的上限。
var DefaultExtractionLimits = ExtractionLimits{
	MaxFiles:     100000,
	MaxBytes:     2 << 30,
	MaxFileBytes: 1 << 30,
	MaxPathBytes: 4096,
}

type archiveFormat uint8

const (
	archiveZIP archiveFormat = iota + 1
	archiveTAR
	archiveTarGZIP
)

// archiveExpandedBytes 在写入前估算完整展开体积，ZIP 和 TAR 共用现有资源上限。
func archiveExpandedBytes(ctx context.Context, archivePath string, limits ExtractionLimits) (int64, error) {
	source, err := safeArchiveFile(archivePath)
	if err != nil {
		return 0, err
	}
	format, err := detectArchiveFormat(source)
	if err != nil {
		return 0, err
	}
	limits = normalizeExtractionLimits(limits)
	var total int64
	count := 0
	add := func(size uint64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > limits.MaxFiles || size > uint64(limits.MaxFileBytes) || size > uint64(limits.MaxBytes-total) {
			return errors.New("更新包展开体积或条目数超过限制")
		}
		total += int64(size)
		return nil
	}
	if format == archiveZIP {
		reader, err := zip.OpenReader(source)
		if err != nil {
			return 0, err
		}
		defer reader.Close()
		for _, entry := range reader.File {
			if err := add(entry.UncompressedSize64); err != nil {
				return 0, err
			}
		}
		return total, nil
	}
	file, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var input io.Reader = file
	if format == archiveTarGZIP {
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return 0, err
		}
		defer compressed.Close()
		input = compressed
	}
	reader := tar.NewReader(input)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		header, err := reader.Next()
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return 0, err
		}
		if header.Size < 0 {
			return 0, errors.New("更新包文件大小无效")
		}
		if err := add(uint64(header.Size)); err != nil {
			return 0, err
		}
	}
}

// ExtractArchive 将 ZIP/TAR/TAR.GZ 包解压到一个全新的目录。
// destination 已存在、包含符号链接或解压中途失败时，函数不会覆盖原有目录。
func ExtractArchive(ctx context.Context, archivePath, destination string, limits ExtractionLimits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	limits = normalizeExtractionLimits(limits)
	source, err := safeArchiveFile(archivePath)
	if err != nil {
		return err
	}
	destination, err = prepareExtractionRoot(destination)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(destination)
		}
	}()
	format, err := detectArchiveFormat(source)
	if err != nil {
		return err
	}
	state := extractionState{root: destination, limits: limits, seen: make(map[string]struct{})}
	switch format {
	case archiveZIP:
		if err := extractZIP(ctx, source, &state); err != nil {
			return err
		}
	case archiveTAR:
		if err := extractTAR(ctx, source, &state, nil); err != nil {
			return err
		}
	case archiveTarGZIP:
		file, openErr := os.Open(source)
		if openErr != nil {
			return openErr
		}
		gzipReader, gzipErr := gzip.NewReader(file)
		if gzipErr != nil {
			_ = file.Close()
			return fmt.Errorf("打开 gzip 更新包失败：%w", gzipErr)
		}
		err = extractTAR(ctx, source, &state, gzipReader)
		closeGzipErr := gzipReader.Close()
		closeFileErr := file.Close()
		if err != nil {
			return err
		}
		if closeGzipErr != nil {
			return closeGzipErr
		}
		if closeFileErr != nil {
			return closeFileErr
		}
	default:
		return errors.New("不支持的离线包格式")
	}
	if err := syncDirectory(destination); err != nil {
		return fmt.Errorf("同步解压目录失败：%w", err)
	}
	committed = true
	return nil
}

type extractionState struct {
	root       string
	limits     ExtractionLimits
	seen       map[string]struct{}
	fileCount  int
	totalBytes int64
}

func extractZIP(ctx context.Context, source string, state *extractionState) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	reader, err := zip.OpenReader(source)
	if err != nil {
		return fmt.Errorf("打开 ZIP 更新包失败：%w", err)
	}
	defer reader.Close()
	if info.Size() != 0 && len(reader.File) > state.limits.MaxFiles {
		return fmt.Errorf("离线包条目数超过限制：%d", state.limits.MaxFiles)
	}
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, isDir, err := normalizeArchiveEntry(entry.Name, entry.FileInfo().IsDir(), state.limits)
		if err != nil {
			return err
		}
		if err := state.beginEntry(name); err != nil {
			return err
		}
		if entry.Mode()&os.ModeSymlink != 0 || entry.Mode()&os.ModeNamedPipe != 0 || entry.Mode()&os.ModeDevice != 0 || entry.Mode()&os.ModeSocket != 0 {
			return fmt.Errorf("ZIP 包包含不允许的特殊文件：%s", name)
		}
		if isDir {
			if err := state.makeDirectory(name); err != nil {
				return err
			}
			continue
		}
		if entry.UncompressedSize64 > uint64(state.limits.MaxFileBytes) || entry.UncompressedSize64 > uint64(maxInt64(state.limits.MaxBytes)) {
			return fmt.Errorf("ZIP 文件超过展开大小限制：%s", name)
		}
		file, openErr := entry.Open()
		if openErr != nil {
			return fmt.Errorf("打开 ZIP 文件 %s 失败：%w", name, openErr)
		}
		copyErr := state.writeFile(ctx, name, file, int64(entry.UncompressedSize64), entry.Mode().Perm())
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func extractTAR(ctx context.Context, source string, state *extractionState, reader io.Reader) error {
	var input io.Reader = reader
	var file *os.File
	if input == nil {
		var err error
		file, err = os.Open(source)
		if err != nil {
			return err
		}
		defer file.Close()
		input = file
	}
	tarReader := tar.NewReader(input)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取 TAR 更新包失败：%w", err)
		}
		isDir := header.Typeflag == tar.TypeDir
		name, normalizedDir, normalizeErr := normalizeArchiveEntry(header.Name, isDir, state.limits)
		if normalizeErr != nil {
			return normalizeErr
		}
		if normalizedDir != isDir {
			return fmt.Errorf("TAR 条目类型不一致：%s", header.Name)
		}
		if err := state.beginEntry(name); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := state.makeDirectory(name); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > state.limits.MaxFileBytes || header.Size > state.limits.MaxBytes-state.totalBytes {
				return fmt.Errorf("TAR 文件超过展开大小限制：%s", name)
			}
			if err := state.writeFile(ctx, name, tarReader, header.Size, os.FileMode(header.Mode)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("TAR 包包含不允许的条目类型 %d：%s", header.Typeflag, name)
		}
	}
}

func (state *extractionState) beginEntry(name string) error {
	if _, exists := state.seen[name]; exists {
		return fmt.Errorf("离线包包含重复条目：%s", name)
	}
	state.seen[name] = struct{}{}
	state.fileCount++
	if state.fileCount > state.limits.MaxFiles {
		return fmt.Errorf("离线包条目数超过限制：%d", state.limits.MaxFiles)
	}
	return nil
}

func (state *extractionState) makeDirectory(name string) error {
	path := filepath.Join(state.root, filepath.FromSlash(name))
	if err := ensureExtractionParent(state.root, path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("解压路径与目录冲突：%s", name)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Mkdir(path, 0o700)
}

func (state *extractionState) writeFile(ctx context.Context, name string, source io.Reader, declaredSize int64, mode os.FileMode) error {
	if declaredSize < 0 || declaredSize > state.limits.MaxFileBytes || declaredSize > state.limits.MaxBytes-state.totalBytes {
		return fmt.Errorf("离线文件超过展开大小限制：%s", name)
	}
	path := filepath.Join(state.root, filepath.FromSlash(name))
	if err := ensureExtractionParent(state.root, path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("解压路径重复或冲突：%s", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, extractionMode(mode))
	if err != nil {
		return fmt.Errorf("创建解压文件 %s 失败：%w", name, err)
	}
	hashBytes := int64(0)
	buffer := make([]byte, 4<<20)
	copyErr := error(nil)
	for {
		if err := ctx.Err(); err != nil {
			copyErr = err
			break
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			hashBytes += int64(count)
			if hashBytes > declaredSize || hashBytes > state.limits.MaxFileBytes || hashBytes > state.limits.MaxBytes-state.totalBytes {
				copyErr = fmt.Errorf("解压文件实际大小超过限制：%s", name)
				break
			}
			if _, writeErr := file.Write(buffer[:count]); writeErr != nil {
				copyErr = writeErr
				break
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			copyErr = readErr
			break
		}
	}
	if copyErr == nil && hashBytes != declaredSize {
		copyErr = fmt.Errorf("解压文件大小与包头不一致：%s", name)
	}
	if syncErr := file.Sync(); copyErr == nil && syncErr != nil {
		copyErr = syncErr
	}
	if closeErr := file.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("解压文件 %s 失败：%w", name, copyErr)
	}
	state.totalBytes += hashBytes
	return nil
}

func normalizeArchiveEntry(value string, directory bool, limits ExtractionLimits) (string, bool, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || len(value) > limits.MaxPathBytes {
		return "", false, errors.New("离线包条目路径为空、过长或包含 NUL")
	}
	if strings.Contains(value, "\\") {
		return "", false, fmt.Errorf("离线包条目使用反斜杠路径，拒绝跨平台歧义：%s", value)
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || (len(value) >= 2 && value[1] == ':') {
		return "", false, fmt.Errorf("离线包条目不是相对路径：%s", value)
	}
	trimmed := strings.TrimSuffix(value, "/")
	if trimmed == "" {
		return "", false, errors.New("离线包包含无效根目录条目")
	}
	clean := path.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") || clean != trimmed {
		return "", false, fmt.Errorf("离线包条目越出或混淆解压根：%s", value)
	}
	return clean, directory || strings.HasSuffix(value, "/"), nil
}

func normalizeExtractionLimits(limits ExtractionLimits) ExtractionLimits {
	defaults := DefaultExtractionLimits
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = defaults.MaxFiles
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaults.MaxBytes
	}
	if limits.MaxFileBytes <= 0 {
		limits.MaxFileBytes = defaults.MaxFileBytes
	}
	if limits.MaxPathBytes <= 0 {
		limits.MaxPathBytes = defaults.MaxPathBytes
	}
	if limits.MaxFileBytes > limits.MaxBytes {
		limits.MaxFileBytes = limits.MaxBytes
	}
	return limits
}

func safeArchiveFile(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("离线包不可读：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("离线包必须是普通文件，不能是链接或特殊文件")
	}
	return abs, nil
}

func prepareExtractionRoot(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if hasCodexComponent(abs) {
		return "", errors.New("解压目录不能位于 .codex 路径")
	}
	if _, err := os.Lstat(abs); err == nil {
		return "", errors.New("解压目录必须不存在，拒绝覆盖已有目录")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := ensureSafeDirectoryChain(filepath.Dir(abs)); err != nil {
		return "", err
	}
	if err := os.Mkdir(abs, 0o700); err != nil {
		return "", err
	}
	return abs, nil
}

func ensureSafeDirectoryChain(path string) error {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	missing := make([]string, 0)
	current := abs
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("解压目标父路径不安全：%s", current)
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return statErr
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return errors.New("无法解析解压目标父路径")
		}
		current = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		if err := os.Mkdir(missing[index], 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(missing[index])
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("解压目标父路径创建后不安全：%s", missing[index])
		}
	}
	return nil
}

func ensureExtractionParent(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("解压路径越出目标目录")
	}
	parent := filepath.Dir(target)
	if parent == root {
		return nil
	}
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		if info, statErr := os.Lstat(current); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("解压父路径不安全：%s", current)
			}
			continue
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func extractionMode(mode os.FileMode) os.FileMode {
	perm := mode.Perm() & 0o700
	if perm == 0 {
		return 0o600
	}
	return perm
}

func detectArchiveFormat(path string) (archiveFormat, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".zip":
		return archiveZIP, nil
	case ".tar":
		return archiveTAR, nil
	case ".gz", ".tgz":
		return archiveTarGZIP, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var header [4]byte
	count, err := io.ReadFull(file, header[:])
	if err != nil && err != io.ErrUnexpectedEOF {
		return 0, err
	}
	if count >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		return archiveTarGZIP, nil
	}
	if count >= 4 && header[0] == 'P' && header[1] == 'K' && header[2] == 0x03 && header[3] == 0x04 {
		return archiveZIP, nil
	}
	return 0, errors.New("无法识别离线包格式；请使用 ZIP、TAR 或 TAR.GZ")
}

func maxInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
