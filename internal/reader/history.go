// 中文说明：验证明确的历史前缀引用，区分物理文件、历史版本和子代理，不猜测缺失前文。
package reader

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type historyBase struct {
	ThreadID   string `json:"thread_id"`
	EndOrdinal int64  `json:"end_ordinal_exclusive"`
	EndByte    int64  `json:"end_byte_offset"`
}
type header struct {
	ID   string       `json:"id"`
	Mode string       `json:"history_mode"`
	Base *historyBase `json:"history_base"`
}
type historyRecord struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Ordinal *int64 `json:"ordinal"`
	Payload header `json:"payload"`
}

var errNoIdentity = errors.New("缺少 session_meta 身份")

func physicalID(source Source, logical string) string {
	stem := strings.TrimSuffix(filepath.Base(source.Relative), ".jsonl")
	if pos := strings.LastIndex(stem, "_"); pos >= 0 && len(stem[pos+1:]) == 36 {
		return stem[pos+1:]
	}
	return logical
}

func unresolvedBase(h historyRecord) bool {
	return h.Payload.Mode == "paginated" && h.Payload.Base == nil && (h.Ordinal == nil || *h.Ordinal != 0)
}

func shortHash(value []byte) string {
	h := sha256.Sum256(value)
	return hex.EncodeToString(h[:16])
}

// Version 为只读来源快照；追加时还会校验游标附近的原始字节。
func Version(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("来源不是普通文件")
	}
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	if end := bytes.IndexByte(buf[:n], '\n'); end >= 0 {
		n = end + 1
	}
	return fmt.Sprintf("%d:%d:%s", info.Size(), info.ModTime().UnixNano(), shortHash(buf[:n])), info.Size(), nil
}

func readHeader(source Source) (historyRecord, error) {
	var record historyRecord
	f, err := os.Open(source.Path)
	if err != nil {
		return record, err
	}
	defer f.Close()
	b, _, _, err := boundedLine(bufio.NewReader(io.LimitReader(f, 128<<10)), 128<<10)
	if err != nil && err != io.EOF {
		return record, err
	}
	if err := json.Unmarshal(b, &record); err != nil {
		return record, err
	}
	if record.Payload.ID == "" {
		record.Payload.ID = record.ID
	}
	if record.Type != "session_meta" || record.Payload.ID == "" {
		return record, errNoIdentity
	}
	return record, nil
}

// BuildPlan 只在当前工作区传入的库存中解析引用；多义引用和循环均不展开。
func BuildPlan(ctx context.Context, sources []Source, targets []string) Plan {
	return BuildPlanForSource(ctx, sources, targets, "")
}

// BuildPlanForSource 固定一个物理历史；只拼接明确 history_base，不拼接独立版本。
func BuildPlanForSource(ctx context.Context, sources []Source, targets []string, selected string) Plan {
	p := Plan{Complete: true, Diagnostics: []string{}}
	byPath := map[string]Source{}
	headers := map[string]historyRecord{}
	headerErrors := map[string]error{}
	byID := map[string][]string{}
	loaded := map[string]bool{}
	loadHeader := func(source Source) {
		if loaded[source.Relative] {
			return
		}
		loaded[source.Relative] = true
		if h, err := readHeader(source); err == nil {
			headers[source.Relative] = h
			if source.ID == "" {
				id := physicalID(source, h.Payload.ID)
				byID[id] = append(byID[id], source.Relative)
			}
		} else {
			headerErrors[source.Relative] = err
		}
	}
	for _, source := range sources {
		if ctx.Err() != nil {
			p.Complete = false
			p.Diagnostics = append(p.Diagnostics, "历史解析已取消")
			return p
		}
		byPath[source.Relative] = source
		if source.ID != "" {
			id := physicalID(source, source.ID)
			byID[id] = append(byID[id], source.Relative)
		}
	}
	// 库存已有身份时只打开目标及其引用链；合成样本缺少库存身份时才补读未知头。
	for _, target := range targets {
		if source, ok := byPath[target]; ok {
			loadHeader(source)
		}
	}
	resolve := func(id string) []string {
		for _, source := range sources {
			if source.ID == "" {
				loadHeader(source)
			}
		}
		return byID[id]
	}
	issue := func(s string) { p.Complete = false; p.Diagnostics = append(p.Diagnostics, s) }
	visiting := map[string]bool{}
	added := map[string]int{}
	var visit func(string, int64, int)
	visit = func(relative string, end int64, depth int) {
		if depth > 32 || visiting[relative] {
			issue("历史引用存在循环或层级超过 32：" + relative)
			return
		}
		source, ok := byPath[relative]
		if !ok {
			issue("历史来源缺失：" + relative)
			return
		}
		version, size, err := Version(source.Path)
		if err != nil {
			issue("来源不可读：" + relative)
			return
		}
		if end < 0 {
			end = size
		}
		if end > size {
			issue("历史引用超过文件大小：" + relative)
			return
		}
		visiting[relative] = true
		defer delete(visiting, relative)
		loadHeader(source)
		if err := headerErrors[relative]; err != nil && (!errors.Is(err, errNoIdentity) || strings.HasPrefix(relative, "sessions/") || strings.HasPrefix(relative, "archived_sessions/")) {
			issue("历史身份不可解析：" + relative)
		}
		h, has := headers[relative]
		if has && (h.Payload.Base != nil || unresolvedBase(h)) {
			base := h.Payload.Base
			if base == nil || h.Payload.Mode != "paginated" || h.Ordinal == nil || base.EndByte < 0 || base.EndOrdinal < 0 || *h.Ordinal != base.EndOrdinal {
				issue("历史前缀引用格式或序号不完整：" + relative)
			} else if candidates := resolve(base.ThreadID); len(candidates) != 1 {
				issue("历史前缀引用缺失或有多个候选：" + relative)
			} else if err := validateBoundary(byPath[candidates[0]], *base); err != nil {
				issue("历史前缀边界无效：" + relative + "（" + err.Error() + "）")
			} else {
				visit(candidates[0], base.EndByte, depth+1)
			}
		}
		if index, ok := added[relative]; ok {
			if end > p.Segments[index].End {
				p.Segments[index].End = end
			}
			return
		}
		added[relative] = len(p.Segments)
		p.Segments = append(p.Segments, Segment{Source: source, End: end, ThreadID: h.Payload.ID, Version: version, Guard: guard(source.Path, end)})
	}
	// 被本次其他版本引用的文件只按验证后的前缀读取，不作为完整独立历史重复加入。
	dependent := map[string]bool{}
	for _, t := range targets {
		if b := headers[t].Payload.Base; b != nil {
			if candidates := resolve(b.ThreadID); len(candidates) == 1 {
				dependent[candidates[0]] = true
			}
		}
	}
	ordered := append([]string(nil), targets...)
	sort.Strings(ordered)
	leaves := 0
	for _, t := range ordered {
		if !dependent[t] {
			leaves++
		}
	}
	if leaves > 1 {
		p.Diagnostics = append(p.Diagnostics, "存在多份独立历史，尚未确认当前主线；请选择历史来源，未将它们拼接")
		if selected == "" {
			p.Complete = false
		}
	}
	for _, t := range ordered {
		if !dependent[t] && p.Leaf == "" {
			p.Leaf = t
		}
	}
	if leaves == 1 {
		p.Mainline = p.Leaf
	}
	for _, t := range ordered {
		p.Versions = append(p.Versions, HistoryVersion{Path: t, Mainline: t == p.Mainline})
		if t == selected {
			p.Leaf = t
		}
	}
	if selected != "" && p.Leaf != selected {
		issue("指定历史来源不属于该会话")
		return p
	}
	for _, t := range ordered {
		if t == p.Leaf {
			visit(t, -1, 0)
		}
	}
	if len(p.Segments) == 0 && len(ordered) > 0 {
		p.Leaf = ordered[0]
		visit(ordered[0], -1, 0)
	}
	return p
}

func validateBoundary(source Source, base historyBase) error {
	f, err := os.Open(source.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if base.EndByte > info.Size() {
		return fmt.Errorf("字节越界")
	}
	if base.EndByte == 0 {
		h, err := readHeader(source)
		if err != nil {
			return err
		}
		if h.Ordinal == nil || *h.Ordinal != base.EndOrdinal {
			return fmt.Errorf("空前缀序号不符")
		}
		return nil
	}
	length := base.EndByte
	if length > MaxLineBytes {
		length = MaxLineBytes
	}
	b := make([]byte, length)
	if _, err := f.ReadAt(b, base.EndByte-length); err != nil {
		return err
	}
	if b[len(b)-1] != '\n' {
		return fmt.Errorf("引用未结束于完整行")
	}
	b = bytes.TrimSuffix(b, []byte{'\n'})
	pos := bytes.LastIndexByte(b, '\n')
	if pos >= 0 {
		b = b[pos+1:]
	} else if base.EndByte > length {
		return fmt.Errorf("边界记录过大")
	}
	var record historyRecord
	if json.Unmarshal(b, &record) != nil || record.Ordinal == nil || *record.Ordinal != base.EndOrdinal-1 {
		return fmt.Errorf("结束序号不符")
	}
	return nil
}
