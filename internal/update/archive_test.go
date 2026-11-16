// 中文说明：验证离线更新包的格式识别、路径安全、资源限制和计划绑定行为。
package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractArchiveZIPAndTARGZIP(t *testing.T) {
	ctx := context.Background()
	zipPath := filepath.Join(t.TempDir(), "release.zip")
	if err := writeZIP(zipPath, map[string][]byte{"bin/app": []byte("zip-app"), "nested/readme.txt": []byte("readme")}); err != nil {
		t.Fatal(err)
	}
	zipDestination := filepath.Join(t.TempDir(), "zip-output")
	if err := ExtractArchive(ctx, zipPath, zipDestination, ExtractionLimits{}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(zipDestination, "bin", "app"), "zip-app")
	assertFileContent(t, filepath.Join(zipDestination, "nested", "readme.txt"), "readme")

	tarPath := filepath.Join(t.TempDir(), "release.tar.gz")
	if err := writeTARGZIP(tarPath, map[string][]byte{"bin/app": []byte("tar-app")}); err != nil {
		t.Fatal(err)
	}
	tarDestination := filepath.Join(t.TempDir(), "tar-output")
	if err := ExtractArchive(ctx, tarPath, tarDestination, ExtractionLimits{}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(tarDestination, "bin", "app"), "tar-app")
}

func TestExtractArchiveRejectsTraversalLinksAndLimits(t *testing.T) {
	tests := []struct {
		name   string
		write  func(string) error
		want   string
		limits ExtractionLimits
	}{
		{name: "zip traversal", write: func(path string) error { return writeZIP(path, map[string][]byte{"../escape": []byte("bad")}) }, want: "越出"},
		{name: "zip duplicate", write: func(path string) error {
			return writeZIPEntries(path, []zipEntry{{name: "same", data: []byte("a")}, {name: "same", data: []byte("b")}})
		}, want: "重复"},
		{name: "zip size", write: func(path string) error { return writeZIP(path, map[string][]byte{"large": []byte("12345")}) }, want: "大小", limits: ExtractionLimits{MaxBytes: 4, MaxFileBytes: 4}},
		{name: "zip symlink", write: func(path string) error { return writeZIPSymlink(path) }, want: "特殊文件"},
		{name: "tar hardlink", write: func(path string) error { return writeTARHardlink(path) }, want: "条目类型"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "bad.zip")
			if strings.HasPrefix(tc.name, "tar") {
				archivePath = filepath.Join(t.TempDir(), "bad.tar")
			}
			if err := tc.write(archivePath); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "output")
			err := ExtractArchive(context.Background(), archivePath, destination, tc.limits)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
			if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
				t.Fatalf("failed extraction left destination: %v", statErr)
			}
		})
	}
}

func TestExtractArchiveRejectsSymlinkDestinationParent(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "release.zip")
	if err := writeZIP(archivePath, map[string][]byte{"file": []byte("content")}); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err := ExtractArchive(context.Background(), archivePath, filepath.Join(link, "output"), ExtractionLimits{})
	if err == nil || !strings.Contains(err.Error(), "父路径") {
		t.Fatalf("symlink destination parent was accepted: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "output")); !os.IsNotExist(statErr) {
		t.Fatalf("symlink target was modified: %v", statErr)
	}
}

func TestPrepareArchiveBindsExtractedBundleToPlan(t *testing.T) {
	root := makeBundle(t, "1.0.0", "压缩版本")
	archivePath := filepath.Join(t.TempDir(), "release.zip")
	if err := zipDirectory(archivePath, root); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(filepath.Join(t.TempDir(), "install"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := manager.PrepareArchive(context.Background(), archivePath, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plan.BundleRoot, filepath.Join(manager.Root(), BundlesDirName)+string(filepath.Separator)) {
		t.Fatalf("archive bundle was not isolated under manager root: %s", plan.BundleRoot)
	}
	if _, err := os.Stat(filepath.Join(plan.BundleRoot, ManifestName)); err != nil {
		t.Fatalf("extracted manifest missing: %v", err)
	}
	result, err := manager.ActivateByID(context.Background(), plan.ID, VerifyVersionRoot)
	if err != nil || result.ActiveVersion != "1.0.0" {
		t.Fatalf("archive activation failed: %+v err=%v", result, err)
	}
}

type zipEntry struct {
	name string
	data []byte
}

func writeZIP(path string, entries map[string][]byte) error {
	list := make([]zipEntry, 0, len(entries))
	for name, data := range entries {
		list = append(list, zipEntry{name: name, data: data})
	}
	return writeZIPEntries(path, list)
}

func writeZIPEntries(path string, entries []zipEntry) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		part, createErr := writer.Create(entry.name)
		if createErr != nil {
			_ = file.Close()
			return createErr
		}
		if _, writeErr := part.Write(entry.data); writeErr != nil {
			_ = file.Close()
			return writeErr
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeZIPSymlink(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	header := &zip.FileHeader{Name: "link", Method: zip.Store}
	header.SetMode(os.ModeSymlink | 0o777)
	part, err := writer.CreateHeader(header)
	if err == nil {
		_, err = part.Write([]byte("outside"))
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func writeTARGZIP(path string, entries map[string][]byte) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, data := range entries {
		header := &tar.Header{Name: name, Mode: 0o700, Size: int64(len(data))}
		if err := tarWriter.WriteHeader(header); err != nil {
			return closeArchiveWriters(file, tarWriter, gzipWriter, err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			return closeArchiveWriters(file, tarWriter, gzipWriter, err)
		}
	}
	return closeArchiveWriters(file, tarWriter, gzipWriter, nil)
}

func writeTARHardlink(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer := tar.NewWriter(file)
	if err := writer.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "target"}); err != nil {
		_ = file.Close()
		return err
	}
	return closeArchiveWriters(file, writer, nil, nil)
}

func closeArchiveWriters(file *os.File, tarWriter *tar.Writer, gzipWriter *gzip.Writer, prior error) error {
	err := prior
	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if gzipWriter != nil {
		if closeErr := gzipWriter.Close(); err == nil {
			err = closeErr
		}
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func zipDirectory(destination, root string) error {
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		part, err := writer.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = part.Write(data)
		return err
	})
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

// newGZIPWriter is kept in a helper to make the TAR test close order explicit.
