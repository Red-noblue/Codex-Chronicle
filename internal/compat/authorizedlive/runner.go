// 中文说明：桥接已审阅 schema 的会话执行器；旧模式仅保留合成 ID，全会话模式须由启动配置明确开启。
package authorizedlive

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

//go:embed executor.py
var script string

var allowed = []string{
	"00000000-0000-4000-8000-000000000101",
	"00000000-0000-4000-8000-000000000102",
	"00000000-0000-4000-8000-000000000103",
	"00000000-0000-4000-8000-000000000104",
}

// Config 必须由启动参数显式提供；不接受 HTTP 请求扩大授权或替换执行程序。
type Config struct {
	Python           string
	Bridge           string
	Socket           string
	AllConversations bool
}

var conversationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func AllowedIDs() []string { return append([]string(nil), allowed...) }

func Allows(id string) bool {
	for _, candidate := range allowed {
		if id == candidate {
			return true
		}
	}
	return false
}

// Allows 保留旧实验入口的固定范围；全会话模式也只接受原始 UUID，不能把路径或查询作为目标。
func (c Config) Allows(id string) bool {
	return (c.AllConversations && conversationIDPattern.MatchString(id)) || Allows(id)
}

func (c Config) AllowsAction(trash, permanent bool) bool {
	return trash != permanent && (!permanent || c.AllConversations)
}

// Validate 只接受当前用户真实根和 macOS 原生废纸篓；其他平台继续使用只读/副本能力。
func (c Config) Validate(root string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if runtime.GOOS != "darwin" || filepath.Clean(root) != filepath.Join(home, ".codex") {
		return fmt.Errorf("真实会话执行目前适配当前用户 macOS 的 ~/.codex；其他工作区保持只读")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != filepath.Clean(root) {
		return fmt.Errorf("真实源根身份不匹配或包含符号链接")
	}
	for _, path := range []string{c.Python, c.Bridge} {
		info, err := os.Lstat(path)
		if !filepath.IsAbs(path) || err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			return fmt.Errorf("受限执行需要绝对路径的可执行 Python 和原生废纸篓 bridge")
		}
	}
	if c.Socket == "" {
		return fmt.Errorf("必须配置现有 Codex daemon socket，核对目标是否已加载")
	}
	return nil
}

// Run 不经 shell 执行；错误仅返回执行器的错误字段，避免把对话内容写入日志。
func (c Config) Run(ctx context.Context, action, work string, extra ...string) (json.RawMessage, error) {
	args := append([]string{"-c", script, action, "--work", work}, extra...)
	if c.AllConversations {
		args = append(args, "--all-conversations")
	}
	if action == "apply" || action == "recover" {
		args = append(args, "--bridge", c.Bridge)
	}
	command := exec.CommandContext(ctx, c.Python, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(stderr.Bytes(), &failure) == nil && failure.Error != "" {
			return nil, fmt.Errorf("%s", failure.Error)
		}
		return nil, fmt.Errorf("受限执行器 %s 失败: %w", action, err)
	}
	if !json.Valid(stdout.Bytes()) {
		return nil, fmt.Errorf("受限执行器没有返回有效结果")
	}
	return append(json.RawMessage(nil), stdout.Bytes()...), nil
}

// PlanDigest 把确认绑定到完整影响清单；执行前仍由 Python 复核源、行快照、备份和活动状态。
func PlanDigest(work string) (string, json.RawMessage, error) {
	data, err := os.ReadFile(filepath.Join(work, "plan.json"))
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(data)
	value := hex.EncodeToString(digest[:])
	expected, err := os.ReadFile(filepath.Join(work, "plan.sha256"))
	if err != nil || strings.TrimSpace(string(expected)) != value {
		return "", nil, fmt.Errorf("受限预览已变化或损坏")
	}
	return value, data, nil
}
