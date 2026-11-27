// 中文说明：按发布包身份保存可续传片段；严格验证范围响应，完整摘要通过后才发布为可预检的更新包。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const partialDownloadsDir = "partials"
const partialDownloadRetention = 7 * 24 * time.Hour

type partialDownload struct {
	Release OnlineRelease `json:"release"`
	ETag    string        `json:"etag,omitempty"`
}

type DownloadProgress struct {
	Version       string `json:"version"`
	ReceivedBytes int64  `json:"received_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	Resumed       bool   `json:"resumed"`
}

func sameOnlineRelease(a, b OnlineRelease) bool {
	return a.Version == b.Version && a.BuildID == b.BuildID && a.OS == b.OS && a.Arch == b.Arch && a.URL == b.URL && strings.EqualFold(a.SHA256, b.SHA256) && a.SizeBytes == b.SizeBytes
}

func (m *Manager) downloadOnline(ctx context.Context, release OnlineRelease) (downloaded DownloadedRelease, err error) {
	finish := BeginDiagnostic(m.root, "download", release.Version)
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return DownloadedRelease{}, err
	}
	m.mu.Lock()
	config := m.online
	m.mu.Unlock()
	config, err = normalizeOnlineConfig(config)
	if err != nil {
		return DownloadedRelease{}, err
	}
	if err := validateOnlineRelease(release, config.MaxDownloadBytes); err != nil {
		return DownloadedRelease{}, err
	}
	if release.OS != runtime.GOOS || release.Arch != runtime.GOARCH {
		return DownloadedRelease{}, fmt.Errorf("在线发布包目标平台 %s/%s 与当前 %s/%s 不匹配", release.OS, release.Arch, runtime.GOOS, runtime.GOARCH)
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\n%s\n%s\n%s\n%s", release.URL, release.SHA256, release.SizeBytes, release.Version, release.BuildID, release.OS, release.Arch)))
	path := filepath.Join(m.root, partialDownloadsDir, "resume-"+hex.EncodeToString(identity[:12])+".part")
	lease, err := acquireUpdateLock(path + ".lock")
	if err != nil {
		return DownloadedRelease{}, err
	}
	defer func() {
		_ = lease.Close()
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			_ = os.Remove(path + ".lock")
		}
	}()
	meta := partialDownload{Release: release}
	if info, err := os.Lstat(path + ".json"); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 16<<10 {
			return DownloadedRelease{}, errors.New("续传元数据不是有效普通文件")
		}
		if err := readJSON(path+".json", &meta); err != nil || !sameOnlineRelease(meta.Release, release) {
			return DownloadedRelease{}, errors.New("续传片段与更新包身份不一致")
		}
	} else if !os.IsNotExist(err) {
		return DownloadedRelease{}, err
	}
	var offset int64
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Size() > release.SizeBytes {
			return DownloadedRelease{}, errors.New("续传片段不是有效普通文件")
		}
		offset = info.Size()
	} else if !os.IsNotExist(err) {
		return DownloadedRelease{}, err
	}
	if err := m.requireDiskSpace(release.SizeBytes-offset, "下载更新包"); err != nil {
		return DownloadedRelease{}, err
	}
	m.setDownloadProgress(DownloadProgress{Version: release.Version, ReceivedBytes: offset, TotalBytes: release.SizeBytes, Resumed: offset != 0})
	defer func() { m.mu.Lock(); m.downloadProgress = nil; m.mu.Unlock() }()
	if offset < release.SizeBytes {
		client, err := onlineHTTPClient(config)
		if err != nil {
			return DownloadedRelease{}, err
		}
		// 元数据请求保持短超时，大文件允许持续下载；上层生命周期仍可随时取消。
		if config.HTTPClient == nil {
			copy := *client
			copy.Timeout = 20 * time.Minute
			client = &copy
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, release.URL, nil)
		if err != nil {
			return DownloadedRelease{}, err
		}
		request.Header.Set("Accept", "application/octet-stream, application/zip, application/gzip, application/x-tar")
		request.Header.Set("Accept-Encoding", "identity")
		if offset > 0 {
			request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			if meta.ETag != "" {
				request.Header.Set("If-Range", meta.ETag)
			}
		}
		response, err := client.Do(request)
		if err != nil {
			return DownloadedRelease{}, fmt.Errorf("下载在线发布包失败：%w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
			return DownloadedRelease{}, fmt.Errorf("在线发布包返回 HTTP %d", response.StatusCode)
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			return DownloadedRelease{}, errors.New("下载服务器返回了不支持续传的内容编码")
		}
		etag := response.Header.Get("ETag")
		if response.StatusCode == http.StatusPartialContent {
			if !validDownloadRange(response.Header.Get("Content-Range"), offset, release.SizeBytes) || meta.ETag != "" && etag != meta.ETag {
				_ = os.Remove(path)
				_ = os.Remove(path + ".json")
				return DownloadedRelease{}, errors.New("续传响应范围或文件标识不匹配，未追加下载数据")
			}
		} else {
			// 忽略 Range 或 If-Range 不匹配时，服务器会返回完整内容，必须从头覆盖。
			offset = 0
			if err := m.requireDiskSpace(release.SizeBytes, "重新下载更新包"); err != nil {
				return DownloadedRelease{}, err
			}
		}
		if response.ContentLength > release.SizeBytes-offset {
			return DownloadedRelease{}, errors.New("在线发布包超过预期大小")
		}
		meta.ETag = ""
		if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) {
			meta.ETag = etag
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return DownloadedRelease{}, err
		}
		if offset == 0 {
			err = file.Truncate(0)
			if err == nil {
				err = file.Sync()
			}
		}
		if err == nil {
			_, err = file.Seek(offset, io.SeekStart)
		}
		if err != nil {
			_ = file.Close()
			return DownloadedRelease{}, err
		}
		if err := writeAtomicJSON(ctx, path+".json", meta); err != nil {
			_ = file.Close()
			return DownloadedRelease{}, err
		}
		progress := DownloadProgress{Version: release.Version, ReceivedBytes: offset, TotalBytes: release.SizeBytes, Resumed: offset > 0}
		m.setDownloadProgress(progress)
		idle := time.AfterFunc(time.Minute, func() { _ = response.Body.Close() })
		writer := &downloadWriter{file: file, progress: progress, manager: m, idle: idle}
		n, copyErr := io.CopyBuffer(writer, io.LimitReader(response.Body, release.SizeBytes-offset+1), make([]byte, 1<<20))
		idle.Stop()
		syncErr, closeErr := file.Sync(), file.Close()
		if n+offset > release.SizeBytes {
			_ = os.Remove(path)
			_ = os.Remove(path + ".json")
			return DownloadedRelease{}, errors.New("在线发布包超过预期大小")
		}
		if err := errors.Join(copyErr, syncErr, closeErr, ctx.Err()); err != nil {
			return DownloadedRelease{}, fmt.Errorf("下载尚未完成，已保留可续传数据：%w", err)
		}
		if n+offset != release.SizeBytes {
			return DownloadedRelease{}, fmt.Errorf("下载尚未完成：收到 %d，期望 %d；下次将尝试续传", n+offset, release.SizeBytes)
		}
	}
	digest, size, err := hashFile(ctx, path)
	if err != nil {
		return DownloadedRelease{}, err
	}
	if size != release.SizeBytes || !strings.EqualFold(digest, release.SHA256) {
		_ = os.Remove(path)
		_ = os.Remove(path + ".json")
		if offset > 0 {
			return DownloadedRelease{}, errors.New("续传内容校验不一致，已丢弃片段，下次从头下载")
		}
		return DownloadedRelease{}, fmt.Errorf("%w：在线发布包 SHA-256 不匹配", ErrInvalidOnlinePackage)
	}
	id, err := newID("download")
	if err != nil {
		return DownloadedRelease{}, err
	}
	target := filepath.Join(m.root, BundlesDirName, id+".archive")
	if err := os.Rename(path, target); err != nil {
		return DownloadedRelease{}, err
	}
	_ = os.Remove(path + ".json")
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		_ = os.Remove(target)
		return DownloadedRelease{}, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		_ = os.Remove(target)
		return DownloadedRelease{}, err
	}
	return DownloadedRelease{Release: release, Path: target, SizeBytes: size, SHA256: digest}, nil
}

func validDownloadRange(value string, offset, total int64) bool {
	startEnd, length, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	start, end, okRange := strings.Cut(startEnd, "-")
	a, errA := strconv.ParseInt(start, 10, 64)
	b, errB := strconv.ParseInt(end, 10, 64)
	c, errC := strconv.ParseInt(length, 10, 64)
	return strings.HasPrefix(value, "bytes ") && ok && okRange && errA == nil && errB == nil && errC == nil && a == offset && b == total-1 && c == total
}

type downloadWriter struct {
	file     *os.File
	manager  *Manager
	progress DownloadProgress
	idle     *time.Timer
	last     time.Time
}

func (w *downloadWriter) Write(data []byte) (int, error) {
	n, err := w.file.Write(data)
	w.idle.Reset(time.Minute)
	w.progress.ReceivedBytes += int64(n)
	if time.Since(w.last) >= time.Second || w.progress.ReceivedBytes >= w.progress.TotalBytes {
		w.manager.setDownloadProgress(w.progress)
		w.last = time.Now()
	}
	return n, err
}

func (m *Manager) setDownloadProgress(progress DownloadProgress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downloadProgress = &progress
}

// recoverPartialDownloads 保留近期可续传片段，只清理可验证归属且超过七天的本应用片段。
func (m *Manager) recoverPartialDownloads(ctx context.Context) error {
	root := filepath.Join(m.root, partialDownloadsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !generatedName(entry.Name(), "resume-", ".part.json") {
			continue
		}
		metaPath := filepath.Join(root, entry.Name())
		info, err := entry.Info()
		if err != nil || info.Size() > 16<<10 || time.Since(info.ModTime()) < partialDownloadRetention {
			continue
		}
		var meta partialDownload
		if err := readJSON(metaPath, &meta); err != nil || validateOnlineRelease(meta.Release, DefaultOnlineDownloadBytes) != nil {
			continue
		}
		identity := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\n%s\n%s\n%s\n%s", meta.Release.URL, meta.Release.SHA256, meta.Release.SizeBytes, meta.Release.Version, meta.Release.BuildID, meta.Release.OS, meta.Release.Arch)))
		if entry.Name() != "resume-"+hex.EncodeToString(identity[:12])+".part.json" {
			continue
		}
		path := strings.TrimSuffix(metaPath, ".json")
		lease, err := acquireUpdateLock(path + ".lock")
		if err != nil {
			if errors.Is(err, errUpdateBusy) {
				continue
			}
			return err
		}
		file, statErr := os.Lstat(path)
		if statErr == nil && (!file.Mode().IsRegular() || time.Since(file.ModTime()) < partialDownloadRetention) || statErr != nil && !os.IsNotExist(statErr) {
			_ = lease.Close()
			continue
		}
		removeErr := os.Remove(path)
		if removeErr == nil || os.IsNotExist(removeErr) {
			removeErr = os.Remove(metaPath)
		}
		_ = lease.Close()
		if removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		_ = os.Remove(path + ".lock")
	}
	return nil
}
