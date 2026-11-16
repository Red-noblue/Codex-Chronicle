// 中文说明：只读识别服务所在设备的 Codex 数据目录；仅检查固定入口和标志项，不递归扫描会话或读取凭证。
package workspace

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CodexHomeCandidate 保留识别来源和可用状态；发现目录不授予修改权限。
type CodexHomeCandidate struct {
	Root    string   `json:"root"`
	Name    string   `json:"name"`
	Sources []string `json:"sources"`
	Status  string   `json:"status"`
	Markers []string `json:"markers"`
	Reason  string   `json:"reason,omitempty"`
}

// DiscoverCodexHomes 按 CODEX_HOME、用户主目录的顺序发现候选；不会创建目录或更改已有工作区选择。
func DiscoverCodexHomes(ctx context.Context) ([]CodexHomeCandidate, error) {
	var candidates []CodexHomeCandidate
	add := func(root, source, name string) {
		if strings.TrimSpace(root) == "" {
			return
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			candidates = append(candidates, CodexHomeCandidate{Root: root, Name: name, Sources: []string{source}, Status: "unavailable", Reason: err.Error()})
			return
		}
		for index := range candidates {
			if candidates[index].Root == abs {
				candidates[index].Sources = append(candidates[index].Sources, source)
				return
			}
		}
		candidates = append(candidates, CodexHomeCandidate{Root: abs, Name: name, Sources: []string{source}})
	}
	add(os.Getenv("CODEX_HOME"), "CODEX_HOME", "Codex · CODEX_HOME")
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		add(filepath.Join(home, ".codex"), "user_home", "本机 .codex")
	}
	for index := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inspectCodexHome(&candidates[index])
	}
	if candidates == nil {
		candidates = make([]CodexHomeCandidate, 0)
	}
	return candidates, nil
}

func inspectCodexHome(candidate *CodexHomeCandidate) {
	candidate.Markers = make([]string, 0)
	info, err := os.Lstat(candidate.Root)
	if os.IsNotExist(err) {
		candidate.Status, candidate.Reason = "missing", "目录不存在"
		return
	}
	if err != nil {
		candidate.Status, candidate.Reason = "unavailable", err.Error()
		return
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		candidate.Status, candidate.Reason = "unavailable", "候选必须是普通目录，不能是文件或符号链接"
		return
	}
	directory, err := os.Open(candidate.Root)
	if err != nil {
		candidate.Status, candidate.Reason = "unavailable", err.Error()
		return
	}
	_, readErr := directory.Readdirnames(1)
	_ = directory.Close()
	if readErr == io.EOF {
		candidate.Status, candidate.Reason = "empty", "目录为空，可手动添加"
		return
	}
	if readErr != nil {
		candidate.Status, candidate.Reason = "unavailable", readErr.Error()
		return
	}
	for _, marker := range []struct {
		name      string
		directory bool
	}{
		{"sessions", true}, {"archived_sessions", true},
		{"session_index.jsonl", false}, {"history.jsonl", false}, {"config.toml", false},
	} {
		info, err := os.Lstat(filepath.Join(candidate.Root, marker.name))
		if err == nil && info.Mode()&os.ModeSymlink == 0 && ((marker.directory && info.IsDir()) || (!marker.directory && info.Mode().IsRegular())) {
			candidate.Markers = append(candidate.Markers, marker.name)
		}
	}
	if len(candidate.Markers) == 0 {
		candidate.Status, candidate.Reason = "unrecognized", "未找到 Codex 数据标志，可核对路径后手动添加"
		return
	}
	candidate.Status = "available"
}
