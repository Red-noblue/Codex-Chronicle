// 中文说明：写入前保守检查明确历史引用；不将引用自动扩展成删除授权。
package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func CheckDependencies(ctx context.Context, root string, targets []string) error {
	selected := map[string]bool{}
	for _, path := range targets {
		if !filepath.IsLocal(filepath.FromSlash(path)) {
			return fmt.Errorf("目标历史路径超出工作区")
		}
		selected[filepath.ToSlash(filepath.Clean(path))] = true
	}
	all := map[string]Source{}
	for _, dir := range []string{"sessions", "archived_sessions"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("历史目录含符号链接，不能证明完整依赖")
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			all[relative] = Source{Relative: relative, Path: path}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for target := range selected {
		all[target] = Source{Relative: target, Path: filepath.Join(root, filepath.FromSlash(target))}
	}
	byID := map[string][]string{}
	headers := map[string]historyRecord{}
	for relative, source := range all {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := readHeader(source)
		inSessions := strings.HasPrefix(relative, "sessions/") || strings.HasPrefix(relative, "archived_sessions/")
		if errors.Is(err, errNoIdentity) {
			if inSessions {
				return fmt.Errorf("历史身份缺失 %s，不能证明完整依赖", relative)
			}
			continue
		}
		if err != nil {
			// 通用文件归档也复用生命周期计划，目录外的非 JSON 样本不冒充 Codex rollout。
			// 标准会话目录中的损坏身份以及任意文件系统错误始终阻断。
			var syntax *json.SyntaxError
			if !inSessions && errors.As(err, &syntax) {
				continue
			}
			return fmt.Errorf("无法读取历史身份 %s，不能证明完整依赖: %w", relative, err)
		}
		headers[relative] = h
		id := physicalID(source, h.Payload.ID)
		byID[id] = append(byID[id], relative)
	}
	for relative, h := range headers {
		if h.Payload.Base == nil && !unresolvedBase(h) {
			continue
		}
		if selected[relative] {
			return fmt.Errorf("目标包含 history_base 历史前缀引用，暂不允许修改、移动或删除")
		}
		if h.Payload.Base == nil {
			return fmt.Errorf("存在不完整的历史引用，无法证明目标没有被共享")
		}
		owners := byID[h.Payload.Base.ThreadID]
		if len(owners) != 1 {
			return fmt.Errorf("存在缺失或多义历史引用，无法证明目标没有被共享")
		}
		if selected[owners[0]] {
			return fmt.Errorf("目标文件仍被 %s 引用，拒绝修改或删除", relative)
		}
	}
	return nil
}
