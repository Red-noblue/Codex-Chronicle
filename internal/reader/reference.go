// 中文说明：按精确证据地址按需读取关联结果，不访问工具参数中的文件，不接受任意磁盘路径。
package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

func ReadReference(ctx context.Context, plan Plan, ref domain.EventReference, full bool) (domain.ConversationEvent, int64, error) {
	if err := ctx.Err(); err != nil {
		return domain.ConversationEvent{}, 0, err
	}
	for _, s := range plan.Segments {
		if s.Relative != ref.Path {
			continue
		}
		if ref.Start < 0 || ref.End <= ref.Start || ref.End > s.End || ref.End-ref.Start > MaxLineBytes {
			return domain.ConversationEvent{}, 0, fmt.Errorf("关联证据超过范围")
		}
		v, _, err := Version(s.Path)
		if err != nil || !snapshotReadable(s, v) {
			return domain.ConversationEvent{}, 0, fmt.Errorf("关联结果来源已变化")
		}
		f, err := os.Open(s.Path)
		if err != nil {
			return domain.ConversationEvent{}, 0, err
		}
		b := make([]byte, ref.End-ref.Start)
		_, err = f.ReadAt(b, ref.Start)
		f.Close()
		if err != nil {
			return domain.ConversationEvent{}, int64(len(b)), err
		}
		after, _, err := Version(s.Path)
		if err != nil || !snapshotReadable(s, after) {
			return domain.ConversationEvent{}, int64(len(b)), fmt.Errorf("关联结果读取期间发生变化")
		}
		e, ok := compat.ParseReaderEvent(domain.JSONLFrame{ValidJSON: json.Valid(b), LineNumber: ref.Line, StartOffset: ref.Start, EndOffset: ref.End}, b)
		if full {
			e, ok = compat.ParseFullReaderEvent(domain.JSONLFrame{ValidJSON: json.Valid(b), LineNumber: ref.Line, StartOffset: ref.Start, EndOffset: ref.End}, b)
		}
		if !ok {
			return e, int64(len(b)), fmt.Errorf("关联证据无法解析")
		}
		e.RelativePath, e.Anchor, e.SourceVersion = ref.Path, shortHash(b), v
		return e, int64(len(b)), nil
	}
	return domain.ConversationEvent{}, 0, fmt.Errorf("关联证据不属于当前历史")
}

// snapshotReadable 允许来源在固定快照末尾继续追加；前缀身份与边界护栏必须保持一致。
// 它不是源文件的全量密码学校验，同大小改写仍由版本变化触发失效。
func snapshotReadable(s Segment, version string) bool {
	if version == s.Version {
		return true
	}
	oldParts, newParts := strings.Split(s.Version, ":"), strings.Split(version, ":")
	if len(oldParts) != 3 || len(newParts) != 3 || oldParts[2] != newParts[2] || s.Guard == "" {
		return false
	}
	oldSize, e1 := strconv.ParseInt(oldParts[0], 10, 64)
	newSize, e2 := strconv.ParseInt(newParts[0], 10, 64)
	return e1 == nil && e2 == nil && newSize > oldSize && guard(s.Path, s.End) == s.Guard
}

// DiscoverSources 仅发现 JSONL，不读取凭证、项目源码或任意符号链接目标。
func DiscoverSources(ctx context.Context, root string) ([]Source, error) {
	sources := []Source{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && strings.HasSuffix(path, ".jsonl") {
			rel, _ := filepath.Rel(root, path)
			sources = append(sources, Source{Relative: filepath.ToSlash(rel), Path: path})
		}
		return nil
	})
	return sources, err
}
