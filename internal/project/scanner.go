// 中文说明：按需只读扫描对话工作目录及其 Git 元数据，生成可解释的项目关系证据。
package project

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
)

const (
	DefaultMaxFiles     = 20000
	DefaultMaxBytes     = 4 << 30
	DefaultRecentFiles  = 12
	DefaultKeyFiles     = 32
	maxGitMetadataBytes = 64 * 1024
	maxGitIgnoreBytes   = 1 << 20
)

// Options 限制单次项目扫描的成本；它不改变源目录。
type Options struct {
	MaxFiles        int
	MaxBytes        int64
	RecentFileLimit int
	KeyFileLimit    int
}

func (options Options) normalized() Options {
	if options.MaxFiles <= 0 || options.MaxFiles > 100000 {
		options.MaxFiles = DefaultMaxFiles
	}
	if options.MaxBytes <= 0 || options.MaxBytes > 64<<30 {
		options.MaxBytes = DefaultMaxBytes
	}
	if options.RecentFileLimit <= 0 || options.RecentFileLimit > 100 {
		options.RecentFileLimit = DefaultRecentFiles
	}
	if options.KeyFileLimit <= 0 || options.KeyFileLimit > 200 {
		options.KeyFileLimit = DefaultKeyFiles
	}
	return options
}

// Scan 从 conversation.cwd 开始查找项目根并统计文件元数据，不读取普通文件正文。
func Scan(ctx context.Context, conversationID, cwd string, options Options) (domain.ProjectRelationReport, error) {
	options = options.normalized()
	report := domain.ProjectRelationReport{
		ConversationID: conversationID,
		RequestedPath:  strings.TrimSpace(cwd),
		Status:         "unavailable",
		Confidence:     "low",
		ReadOnly:       true,
		GeneratedAt:    time.Now().UTC(),
		TopLevelDirs:   make([]string, 0),
		KeyFiles:       make([]domain.ProjectFileSummary, 0),
		RecentFiles:    make([]domain.ProjectFileSummary, 0),
		Evidence:       make([]domain.ProjectEvidence, 0),
		Limitations:    make([]string, 0),
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if strings.TrimSpace(cwd) == "" {
		report.Limitations = append(report.Limitations, "对话没有记录工作目录，无法自动扫描项目关系。")
		return report, nil
	}
	absoluteCWD, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return report, fmt.Errorf("解析项目工作目录失败: %w", err)
	}
	if err := ensureSafeDirectory(absoluteCWD); err != nil {
		report.Limitations = append(report.Limitations, "工作目录不可安全读取："+err.Error())
		return report, nil
	}
	projectRoot, gitRoot, gitBranch, gitRemote, metadataLimitations := findProjectRoot(absoluteCWD)
	report.ProjectRoot = projectRoot
	report.GitRoot = gitRoot
	report.GitBranch = gitBranch
	report.GitRemote = gitRemote
	report.Limitations = append(report.Limitations, metadataLimitations...)
	if relative, relErr := filepath.Rel(projectRoot, absoluteCWD); relErr == nil && relative != "." {
		report.WorkingDirectoryRelative = filepath.ToSlash(relative)
	}
	if gitRoot != "" {
		report.Evidence = append(report.Evidence, domain.ProjectEvidence{Kind: "git_root", Value: gitRoot, Confidence: "high"})
		report.Confidence = "high"
	} else {
		report.Limitations = append(report.Limitations, "未发现 Git 根目录，项目根按对话工作目录处理。")
		report.Evidence = append(report.Evidence, domain.ProjectEvidence{Kind: "working_directory", Value: projectRoot, Confidence: "medium"})
		report.Confidence = "medium"
	}
	if report.GitBranch != "" {
		report.Evidence = append(report.Evidence, domain.ProjectEvidence{Kind: "git_branch", Value: report.GitBranch, Confidence: "medium"})
	}
	if report.GitRemote != "" {
		report.Evidence = append(report.Evidence, domain.ProjectEvidence{Kind: "git_remote", Value: report.GitRemote, Confidence: "medium"})
	}

	ignoreRules, ignoreWarning := readIgnoreRules(projectRoot)
	if ignoreWarning != "" {
		report.Limitations = append(report.Limitations, ignoreWarning)
	}
	keyNames := map[string]struct{}{
		"go.mod": {}, "go.work": {}, "package.json": {}, "pnpm-workspace.yaml": {}, "cargo.toml": {},
		"pyproject.toml": {}, "requirements.txt": {}, "pom.xml": {}, "build.gradle": {}, "makefile": {},
		"dockerfile": {}, "readme": {}, "readme.md": {}, ".gitignore": {},
	}
	keyFiles := make([]domain.ProjectFileSummary, 0, options.KeyFileLimit)
	recentFiles := make([]domain.ProjectFileSummary, 0, options.RecentFileLimit)
	topDirs := make(map[string]struct{})
	walkErr := filepath.WalkDir(projectRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			report.Limitations = appendLimited(report.Limitations, filepath.ToSlash(path)+"：读取失败，已跳过", 32)
			return nil
		}
		if path == projectRoot {
			return nil
		}
		relative := filepath.ToSlash(strings.TrimPrefix(path, projectRoot+string(filepath.Separator)))
		base := filepath.Base(path)
		if entry.Type()&os.ModeSymlink != 0 {
			report.SymlinkCount++
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if defaultExcludedDirectory(base) || ignoreRules.matches(relative, true) {
				return filepath.SkipDir
			}
			if slash := strings.IndexByte(relative, '/'); slash >= 0 {
				topDirs[relative[:slash]] = struct{}{}
			} else {
				topDirs[relative] = struct{}{}
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if ignoreRules.matches(relative, false) || defaultExcludedFile(base) {
			report.IgnoredFileCount++
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			report.Limitations = appendLimited(report.Limitations, relative+"：无法读取文件元数据", 32)
			return nil
		}
		if report.FileCount >= options.MaxFiles || report.TotalBytes+info.Size() > options.MaxBytes {
			report.Truncated = true
			return filepath.SkipAll
		}
		item := domain.ProjectFileSummary{RelativePath: relative, Kind: classifyProjectFile(base), SizeBytes: info.Size(), ModTime: info.ModTime().UTC()}
		report.FileCount++
		report.TotalBytes += info.Size()
		if _, ok := keyNames[strings.ToLower(base)]; ok || item.Kind == "manifest" {
			if len(keyFiles) < options.KeyFileLimit {
				keyFiles = append(keyFiles, item)
			}
		}
		recentFiles = append(recentFiles, item)
		return nil
	})
	if walkErr != nil {
		return report, walkErr
	}
	sort.Strings(report.TopLevelDirs)
	for directory := range topDirs {
		report.TopLevelDirs = append(report.TopLevelDirs, directory)
	}
	sort.Strings(report.TopLevelDirs)
	sort.Slice(keyFiles, func(i, j int) bool { return keyFiles[i].RelativePath < keyFiles[j].RelativePath })
	sort.Slice(recentFiles, func(i, j int) bool {
		if recentFiles[i].ModTime.Equal(recentFiles[j].ModTime) {
			return recentFiles[i].RelativePath < recentFiles[j].RelativePath
		}
		return recentFiles[i].ModTime.After(recentFiles[j].ModTime)
	})
	if len(recentFiles) > options.RecentFileLimit {
		recentFiles = recentFiles[:options.RecentFileLimit]
	}
	report.KeyFiles = keyFiles
	report.RecentFiles = recentFiles
	if report.Truncated {
		report.Status = "truncated"
		report.Limitations = append(report.Limitations, fmt.Sprintf("项目扫描达到预算上限（最多 %d 个文件或 %d 字节），统计不是完整项目快照。", options.MaxFiles, options.MaxBytes))
	} else {
		report.Status = "detected"
	}
	return report, nil
}

func ensureSafeDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("工作目录是符号链接")
	}
	if !info.IsDir() {
		return fmt.Errorf("工作目录不是目录")
	}
	return nil
}

func findProjectRoot(cwd string) (projectRoot, gitRoot, branch, remote string, limitations []string) {
	projectRoot = cwd
	current := cwd
	for {
		gitPath := filepath.Join(current, ".git")
		if info, err := os.Lstat(gitPath); err == nil {
			gitRoot = current
			projectRoot = current
			if info.IsDir() {
				branch, remote, limitations = readGitMetadata(gitPath)
			} else {
				limitations = append(limitations, "发现 Git worktree 的 .git 文件，未解析其 gitdir 目标。")
			}
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return
}

func readGitMetadata(gitPath string) (branch, remote string, limitations []string) {
	head, err := readLimited(filepath.Join(gitPath, "HEAD"), maxGitMetadataBytes)
	if err == nil {
		head = strings.TrimSpace(head)
		if strings.HasPrefix(head, "ref: refs/heads/") {
			branch = strings.TrimPrefix(head, "ref: refs/heads/")
		} else if head != "" {
			branch = "分离 HEAD"
		}
	} else {
		limitations = append(limitations, "Git HEAD 无法读取，分支证据缺失。")
	}
	config, err := readLimited(filepath.Join(gitPath, "config"), maxGitMetadataBytes)
	if err == nil {
		remote = parseOriginURL(config)
	} else {
		limitations = append(limitations, "Git config 无法读取，远程仓库证据缺失。")
	}
	return
}

func parseOriginURL(config string) string {
	section := ""
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section == `[remote "origin"]` && strings.HasPrefix(line, "url") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return sanitizeRemoteURL(parts[1])
			}
		}
	}
	return ""
}

// sanitizeRemoteURL 去掉远程地址中可能携带的账号、密码或访问令牌。
func sanitizeRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil {
		parsed.User = nil
		if parsed.Scheme != "" || parsed.Host != "" {
			return parsed.String()
		}
	}
	// Git 常见的 scp 风格地址不是标准 URL；仅去掉 @ 前的用户部分。
	if at := strings.IndexByte(raw, '@'); at > 0 && !strings.Contains(raw[:at], "/") {
		return raw[at+1:]
	}
	return raw
}

func readLimited(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return string(data[:limit]), fmt.Errorf("元数据超过读取预算")
	}
	return string(data), nil
}

type ignoreRules []ignoreRule

type ignoreRule struct {
	pattern   string
	negated   bool
	anchored  bool
	directory bool
}

func readIgnoreRules(root string) (ignoreRules, string) {
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "项目没有 .gitignore，扫描只使用内置安全排除目录。"
		}
		return nil, ".gitignore 无法读取，未应用其中规则。"
	}
	if len(data) > maxGitIgnoreBytes {
		return nil, ".gitignore 超过读取预算，未应用其中规则。"
	}
	rules := make(ignoreRules, 0)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			rule.negated = true
			line = strings.TrimPrefix(line, "!")
		}
		if strings.HasSuffix(line, "/") {
			rule.directory = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			rule.anchored = true
			line = strings.TrimPrefix(line, "/")
		}
		if line != "" {
			rule.pattern = filepath.ToSlash(line)
			rules = append(rules, rule)
		}
	}
	if len(rules) > 0 {
		return rules, ".gitignore 当前按基础 glob 和否定规则解析，复杂 Git 忽略语义可能与 Git 实际结果不同。"
	}
	return rules, ""
}

func (rules ignoreRules) matches(relative string, directory bool) bool {
	ignored := false
	for _, rule := range rules {
		if rule.directory && !directory {
			continue
		}
		matched := false
		if rule.anchored || strings.Contains(rule.pattern, "/") {
			matched = globMatch(rule.pattern, relative)
		} else {
			matched = globMatch(rule.pattern, filepath.Base(relative))
		}
		if matched {
			ignored = !rule.negated
		}
	}
	return ignored
}

func globMatch(pattern, value string) bool {
	matched, err := filepath.Match(filepath.FromSlash(pattern), filepath.FromSlash(value))
	return err == nil && matched
}

func defaultExcludedDirectory(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".codex", "node_modules", "vendor", "target", ".venv", "venv", "__pycache__", ".idea", ".vscode", "dist", "build":
		return true
	default:
		return false
	}
}

func defaultExcludedFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".pem") || lower == ".env" || strings.HasPrefix(lower, ".env.")
}

func classifyProjectFile(name string) string {
	lower := strings.ToLower(name)
	switch lower {
	case "go.mod", "go.work", "package.json", "pnpm-workspace.yaml", "cargo.toml", "pyproject.toml", "requirements.txt", "pom.xml", "build.gradle", "makefile", "dockerfile":
		return "manifest"
	case "readme", "readme.md", "readme.rst":
		return "documentation"
	default:
		return "file"
	}
}

func appendLimited(values []string, value string, limit int) []string {
	if len(values) >= limit {
		return values
	}
	return append(values, value)
}
