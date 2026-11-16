// 中文说明：覆盖项目关系扫描的只读、预算、忽略规则和安全边界。
package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanProjectMetadataAndIgnoreRules(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	cwd := filepath.Join(projectRoot, "internal")
	for _, directory := range []string{
		filepath.Join(projectRoot, ".git"), cwd, filepath.Join(projectRoot, "src"),
		filepath.Join(projectRoot, "node_modules", "ignored"), filepath.Join(projectRoot, "ignored-dir"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeProjectFile(t, filepath.Join(projectRoot, ".git", "HEAD"), "ref: refs/heads/feature/project-scan\n")
	writeProjectFile(t, filepath.Join(projectRoot, ".git", "config"), "[remote \"origin\"]\n\turl = https://token:secret@example.com/org/repo.git\n")
	writeProjectFile(t, filepath.Join(projectRoot, ".gitignore"), "ignored.txt\nignored-dir/\n*.log\n")
	writeProjectFile(t, filepath.Join(projectRoot, "go.mod"), "module example\n")
	writeProjectFile(t, filepath.Join(projectRoot, "README.md"), "readme\n")
	writeProjectFile(t, filepath.Join(projectRoot, "keep.txt"), "keep\n")
	writeProjectFile(t, filepath.Join(projectRoot, "ignored.txt"), "ignored\n")
	writeProjectFile(t, filepath.Join(projectRoot, "ignored-dir", "nested.txt"), "ignored\n")
	writeProjectFile(t, filepath.Join(projectRoot, "node_modules", "ignored", "package.json"), "ignored\n")
	writeProjectFile(t, filepath.Join(projectRoot, ".env"), "secret\n")
	writeProjectFile(t, filepath.Join(projectRoot, "notes.log"), "ignored\n")
	writeProjectFile(t, filepath.Join(projectRoot, "src", "main.go"), "package main\n")

	generated := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(projectRoot, "keep.txt"), generated, generated); err != nil {
		t.Fatal(err)
	}

	report, err := Scan(context.Background(), "thread-project", cwd, Options{RecentFileLimit: 2, KeyFileLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "detected" || report.Confidence != "high" || report.ReadOnly != true {
		t.Fatalf("unexpected scan status: %+v", report)
	}
	if report.ProjectRoot != projectRoot || report.GitRoot != projectRoot || report.WorkingDirectoryRelative != "internal" {
		t.Fatalf("unexpected roots: %+v", report)
	}
	if report.GitBranch != "feature/project-scan" {
		t.Fatalf("unexpected branch: %q", report.GitBranch)
	}
	if report.GitRemote != "https://example.com/org/repo.git" || strings.Contains(report.GitRemote, "token") || strings.Contains(report.GitRemote, "secret") {
		t.Fatalf("remote credentials were not removed: %q", report.GitRemote)
	}
	if report.FileCount != 5 {
		t.Fatalf("unexpected counted file total: %+v", report)
	}
	if report.IgnoredFileCount != 3 {
		t.Fatalf("unexpected ignored file total: %+v", report)
	}
	if len(report.KeyFiles) != 3 || report.KeyFiles[0].RelativePath != ".gitignore" || report.KeyFiles[1].RelativePath != "README.md" || report.KeyFiles[2].RelativePath != "go.mod" {
		t.Fatalf("unexpected key files: %+v", report.KeyFiles)
	}
	if len(report.RecentFiles) != 2 || report.RecentFiles[0].RelativePath != "src/main.go" {
		t.Fatalf("unexpected recent files: %+v", report.RecentFiles)
	}
	if !contains(report.TopLevelDirs, "internal") || !contains(report.TopLevelDirs, "src") {
		t.Fatalf("top-level directories missing: %+v", report.TopLevelDirs)
	}
	if containsPrefix(report.Limitations, "项目没有") {
		t.Fatalf("unexpected .gitignore absence warning: %+v", report.Limitations)
	}
	if !containsPrefix(report.Limitations, ".gitignore 当前按基础 glob") {
		t.Fatalf("missing .gitignore semantic limitation: %+v", report.Limitations)
	}
}

func TestScanBudgetMarksTruncated(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "a.txt"), "a")
	writeProjectFile(t, filepath.Join(root, "b.txt"), "b")

	report, err := Scan(context.Background(), "thread-budget", root, Options{MaxFiles: 1, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Truncated || report.Status != "truncated" || report.FileCount != 1 {
		t.Fatalf("budget was not enforced: %+v", report)
	}
	if !containsPrefix(report.Limitations, "项目扫描达到预算上限") {
		t.Fatalf("missing budget limitation: %+v", report.Limitations)
	}
}

func TestScanUnavailableAndCancellation(t *testing.T) {
	missing, err := Scan(context.Background(), "thread-missing", filepath.Join(t.TempDir(), "missing"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Status != "unavailable" || !containsPrefix(missing.Limitations, "工作目录不可安全读取") {
		t.Fatalf("missing directory should be unavailable: %+v", missing)
	}

	withoutCWD, err := Scan(context.Background(), "thread-no-cwd", "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if withoutCWD.Status != "unavailable" || !containsPrefix(withoutCWD.Limitations, "对话没有记录工作目录") {
		t.Fatalf("missing cwd should be explained: %+v", withoutCWD)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Scan(ctx, "thread-cancelled", t.TempDir(), Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan returned %v", err)
	}
}

func TestScanDoesNotFollowSymlinkedProjectEntries(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.txt")
	writeProjectFile(t, target, "outside")
	link := filepath.Join(root, "outside.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}
	report, err := Scan(context.Background(), "thread-symlink", root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.SymlinkCount != 1 || report.FileCount != 0 {
		t.Fatalf("symlink entry was treated as a regular file: %+v", report)
	}
}

func writeProjectFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
