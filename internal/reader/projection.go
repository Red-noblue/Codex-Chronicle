// 中文说明：只缓存轮次短标题、排除区间和工具证据地址；不缓存完整正文，也不改写源文件。
package reader

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

const projectionVersion = 3
const maxProjectionEntries = 100000

type ExcludedRange struct {
	Path    string `json:"path"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	EndLine uint64 `json:"end_line"`
	Reason  string `json:"reason"`
}
type Turn struct {
	ID                string                 `json:"id"`
	Start             domain.EventReference  `json:"start"`
	End               domain.EventReference  `json:"end"`
	Caption           string                 `json:"caption,omitempty"`
	Message           *domain.EventReference `json:"message,omitempty"`
	Status            string                 `json:"status"`
	Inferred          bool                   `json:"inferred,omitempty"`
	UserMessages      int                    `json:"user_messages"`
	AssistantMessages int                    `json:"assistant_messages"`
	segment           int
}
type ToolPair struct {
	ID       string                 `json:"id"`
	Call     domain.EventReference  `json:"call"`
	Result   *domain.EventReference `json:"result,omitempty"`
	Status   string                 `json:"status"`
	ExitCode *int                   `json:"exit_code,omitempty"`
}
type FileEntry struct {
	domain.RecordedFileChange
	Source domain.EventReference `json:"source"`
	TurnID string                `json:"turn_id,omitempty"`
	CallID string                `json:"call_id,omitempty"`
}
type Projection struct {
	Version         int             `json:"version"`
	Stamp           string          `json:"stamp"`
	Revision        string          `json:"revision"`
	Ranges          []ExcludedRange `json:"ranges"`
	Turns           []Turn          `json:"turns"`
	Tools           []ToolPair      `json:"tools"`
	Files           []FileEntry     `json:"files"`
	Diagnostics     []string        `json:"diagnostics"`
	BytesRead       int64           `json:"bytes_read"`
	Pending         bool            `json:"pending"`
	rangesByPath    map[string][]ExcludedRange
	toolsByPosition map[string]int
	toolsByResult   map[string]int
	turnsByPath     map[string][]int
}

func referenceKey(r domain.EventReference) string { return fmt.Sprintf("%s:%d", r.Path, r.Start) }
func (p *Projection) initialize() {
	p.rangesByPath = map[string][]ExcludedRange{}
	p.toolsByPosition = map[string]int{}
	p.toolsByResult = map[string]int{}
	p.turnsByPath = map[string][]int{}
	var revision strings.Builder
	for _, r := range p.Ranges {
		p.rangesByPath[r.Path] = append(p.rangesByPath[r.Path], r)
		if r.Reason == "rollback" {
			fmt.Fprintf(&revision, "%s:%d:%d;", r.Path, r.Start, r.End)
		}
	}
	p.Revision = shortHash([]byte(revision.String()))
	for path, ranges := range p.rangesByPath {
		sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
		merged := []ExcludedRange{}
		for _, r := range ranges {
			if len(merged) > 0 && merged[len(merged)-1].End >= r.Start {
				if r.End > merged[len(merged)-1].End {
					merged[len(merged)-1].End, merged[len(merged)-1].EndLine = r.End, r.EndLine
				}
			} else {
				merged = append(merged, r)
			}
		}
		p.rangesByPath[path] = merged
	}
	for i, t := range p.Tools {
		p.toolsByPosition[referenceKey(t.Call)] = i
		if t.Result != nil {
			p.toolsByResult[referenceKey(*t.Result)] = i
		}
	}
	for i, t := range p.Turns {
		p.turnsByPath[t.Start.Path] = append(p.turnsByPath[t.Start.Path], i)
	}
}

func (p *Projection) CallForResult(path string, offset int64) *ToolPair {
	if p == nil {
		return nil
	}
	if i, ok := p.toolsByResult[referenceKey(domain.EventReference{Path: path, Start: offset})]; ok {
		return &p.Tools[i]
	}
	return nil
}
func (p *Projection) Excluded(path string, offset int64) bool {
	return p.excludedRange(path, offset) != nil
}
func (p *Projection) excludedRange(path string, offset int64) *ExcludedRange {
	if p == nil {
		return nil
	}
	ranges := p.rangesByPath[path]
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].Start > offset }) - 1
	if i >= 0 && offset < ranges[i].End {
		return &ranges[i]
	}
	return nil
}
func (p *Projection) ToolAt(path string, offset int64) *ToolPair {
	if p == nil {
		return nil
	}
	if i, ok := p.toolsByPosition[referenceKey(domain.EventReference{Path: path, Start: offset})]; ok {
		return &p.Tools[i]
	}
	return nil
}
func (p *Projection) turnAt(plan Plan, path string, offset int64) *Turn {
	if p == nil {
		return nil
	}
	// 轮次可能跨 history_base 的物理文件边界，按已验证段顺序定位。
	for s := len(plan.Segments) - 1; s >= 0; s-- {
		if plan.Segments[s].Relative == path {
			for j := s; j >= 0; j-- {
				indexes := p.turnsByPath[plan.Segments[j].Relative]
				at := sort.Search(len(indexes), func(i int) bool { return j == s && p.Turns[indexes[i]].Start.Start > offset }) - 1
				if at >= 0 {
					return &p.Turns[indexes[at]]
				}
			}
			break
		}
	}
	return nil
}

type mirroredMessage struct {
	hash, role, turn string
	mask             uint8
	valid            bool
}

// BuildProjection 读取固定快照至末尾再发布，后出现的 rollback 可以排除前面的多个页面。
func BuildProjection(ctx context.Context, plan Plan, progress *atomic.Int64) (*Projection, error) {
	p := &Projection{Version: projectionVersion, Stamp: Stamp(plan), Diagnostics: []string{}}
	stack := []int{}
	current := -1
	pending := domain.EventReference{}
	pendingSegment := 0
	pendingTurn := ""
	last := map[string]mirroredMessage{}
	outputs := map[string][]ToolPair{}
	endLines := make([]uint64, len(plan.Segments))
	outputCount := 0
	fileSignatures := map[string]map[string]int{}
	warn := func(text string) {
		if len(p.Diagnostics) < 20 {
			for _, old := range p.Diagnostics {
				if old == text {
					return
				}
			}
			p.Diagnostics = append(p.Diagnostics, text)
		}
	}
	startTurn := func(ref domain.EventReference, segment int, id string, inferred bool) {
		if id == "" {
			id = shortHash([]byte(referenceKey(ref)))
		}
		p.Turns = append(p.Turns, Turn{ID: id, Start: ref, Status: "unconfirmed", Inferred: inferred, segment: segment})
		current = len(p.Turns) - 1
		stack = append(stack, current)
		pending = domain.EventReference{}
		pendingTurn = ""
		last = map[string]mirroredMessage{}
	}
	for si, s := range plan.Segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		version, _, err := Version(s.Path)
		if err != nil || !snapshotReadable(s, version) {
			return nil, fmt.Errorf("历史来源已变化，请重新加载")
		}
		f, err := os.Open(s.Path)
		if err != nil {
			return nil, err
		}
		r := bufio.NewReaderSize(io.LimitReader(f, s.End), 64<<10)
		offset := int64(0)
		line := uint64(0)
		for offset < s.End {
			if len(p.Turns)+len(p.Tools)+len(p.Files)+len(p.Ranges)+outputCount > maxProjectionEntries {
				f.Close()
				return nil, fmt.Errorf("历史导航元数据超过 100000 项上限，未发布部分投影")
			}
			if err := ctx.Err(); err != nil {
				f.Close()
				return nil, err
			}
			b, n, oversized, readErr := boundedLine(r, MaxLineBytes)
			start := offset
			offset += n
			p.BytesRead += n
			if progress != nil {
				progress.Store(p.BytesRead)
			}
			if readErr != nil && readErr != io.EOF {
				f.Close()
				return nil, readErr
			}
			if n == 0 {
				break
			}
			if oversized {
				f.Close()
				return nil, fmt.Errorf("存在超过 8 MiB 的记录，无法证明完整回滚投影；请查看技术证据")
			}
			if !bytes.HasSuffix(b, []byte{'\n'}) {
				p.Pending = true
				break
			}
			line++
			var wire struct {
				Type    string `json:"type"`
				Payload struct {
					Type  string          `json:"type"`
					Turn  string          `json:"turn_id"`
					Count json.RawMessage `json:"num_turns"`
				} `json:"payload"`
			}
			if json.Unmarshal(b, &wire) != nil {
				warn("存在无法解析的记录，有效历史可能不完整")
				continue
			}
			ref := domain.EventReference{Path: s.Relative, Start: start, End: offset, Line: line}
			if wire.Type == "event_msg" && wire.Payload.Type == "thread_rolled_back" {
				var count uint32
				if string(wire.Payload.Count) == "null" || json.Unmarshal(wire.Payload.Count, &count) != nil {
					warn("忽略格式无效的撤回标记")
					continue
				}
				if count > 0 && len(stack) > 0 {
					n := min(int64(count), int64(len(stack)))
					first := p.Turns[stack[len(stack)-int(n)]]
					for _, index := range stack[len(stack)-int(n):] {
						p.Turns[index].Status = "rolled_back"
					}
					stack = stack[:len(stack)-int(n)]
					for j := first.segment; j <= si; j++ {
						lo, hi := int64(0), plan.Segments[j].End
						if j == first.segment {
							lo = first.Start.Start
						}
						if j == si {
							hi = offset
						}
						lastLine := endLines[j]
						if j == si {
							lastLine = line
						}
						p.Ranges = append(p.Ranges, ExcludedRange{Path: plan.Segments[j].Relative, Start: lo, End: hi, EndLine: lastLine, Reason: "rollback"})
					}
				}
				p.Ranges = append(p.Ranges, ExcludedRange{Path: s.Relative, Start: start, End: offset, EndLine: line, Reason: "marker"})
				if count == 0 {
					continue
				}
				current = -1
				pending = domain.EventReference{}
				pendingTurn = ""
				fileSignatures = map[string]map[string]int{}
				last = map[string]mirroredMessage{}
				continue
			}
			if wire.Type == "event_msg" && wire.Payload.Type == "task_started" && wire.Payload.Turn != "" {
				if current >= 0 && p.Turns[current].ID == wire.Payload.Turn {
					continue
				}
				startTurn(ref, si, wire.Payload.Turn, false)
				continue
			}
			if wire.Type == "event_msg" && (wire.Payload.Type == "task_complete" || wire.Payload.Type == "turn_aborted") {
				if current >= 0 && (wire.Payload.Turn == "" || wire.Payload.Turn == p.Turns[current].ID) {
					p.Turns[current].End = ref
					p.Turns[current].Status = "completed"
					if wire.Payload.Type == "turn_aborted" {
						p.Turns[current].Status = "interrupted"
					}
					current = -1
					pending = domain.EventReference{}
					last = map[string]mirroredMessage{}
				}
				continue
			}
			if wire.Type == "turn_context" && current >= 0 && wire.Payload.Turn != "" && wire.Payload.Turn != p.Turns[current].ID {
				p.Turns[current].End = ref
				current = -1
				last = map[string]mirroredMessage{}
			}
			if wire.Type == "turn_context" && current < 0 {
				pending = ref
				pendingSegment = si
				pendingTurn = wire.Payload.Turn
			}
			e, ok := compat.ParseFullReaderEvent(domain.JSONLFrame{ValidJSON: true, LineNumber: line, StartOffset: start, EndOffset: offset}, b)
			if !ok {
				continue
			}
			isUser := e.Role == "user" && e.Kind != "injected_context" && e.Kind != "agent_task"
			if e.Kind == "injected_context" || e.Role == "developer" {
				if current < 0 && pending.Path == "" {
					pending = ref
					pendingSegment = si
				}
				continue
			}
			if e.Role == "user" || e.Role == "assistant" {
				mask := uint8(1)
				if wire.Type == "event_msg" {
					mask = 2
					if wire.Payload.Type == "item_completed" {
						mask = 4
					}
				}
				h := shortHash([]byte(e.Text))
				previous := last[e.Role]
				turn := ""
				if current >= 0 {
					turn = p.Turns[current].ID
				}
				if previous.valid && previous.hash == h && previous.turn == turn && previous.mask&mask == 0 {
					p.Ranges = append(p.Ranges, ExcludedRange{Path: s.Relative, Start: start, End: offset, EndLine: line, Reason: "mirror"})
					previous.mask |= mask
					last[e.Role] = previous
					continue
				}
				if isUser && (current < 0 || p.Turns[current].Inferred && p.Turns[current].Caption != "") {
					a, seg := ref, si
					if pending.Path != "" {
						a, seg = pending, pendingSegment
					}
					id := wire.Payload.Turn
					if id == "" {
						id = pendingTurn
					}
					startTurn(a, seg, id, id == "")
					turn = p.Turns[current].ID
				}
				if isUser && current >= 0 && p.Turns[current].Caption == "" {
					p.Turns[current].Caption = shortCaption(e.Text)
					copyRef := ref
					p.Turns[current].Message = &copyRef
				}
				last[e.Role] = mirroredMessage{h, e.Role, turn, mask, true}
			}
			if current < 0 && (e.Role == "assistant" || e.Role == "tool") {
				a, seg := ref, si
				if pending.Path != "" {
					a, seg = pending, pendingSegment
				}
				id := wire.Payload.Turn
				if id == "" {
					id = pendingTurn
				}
				startTurn(a, seg, id, id == "")
			}
			if e.Role != "user" && e.Role != "context" {
				v := last["user"]
				v.valid = false
				last["user"] = v
			}
			if current >= 0 {
				if isUser {
					p.Turns[current].UserMessages++
				}
				if e.Role == "assistant" {
					p.Turns[current].AssistantMessages++
				}
			}
			if e.Role == "tool" && e.CallID != "" {
				result := e.Kind == "function_call_output" || e.Kind == "custom_tool_call_output"
				if result {
					outputCount++
					outputs[e.CallID] = append(outputs[e.CallID], ToolPair{ID: e.CallID, Call: ref, Status: e.Status, ExitCode: e.ExitCode})
				} else {
					p.Tools = append(p.Tools, ToolPair{ID: e.CallID, Call: ref, Status: e.Status, ExitCode: e.ExitCode})
				}
			}
			turnID := ""
			if current >= 0 {
				turnID = p.Turns[current].ID
			}
			suppressedChanges := 0
			for _, change := range e.Changes {
				c := change
				c.Diff = ""
				// 只对两种已知成功事件作一对一抵消；同格式的真实重复修改仍保留。
				envelope := wire.Payload.Type
				if turnID != "" && change.Status == "completed" && (envelope == "patch_apply_end" || envelope == "item_completed") {
					signature := shortHash([]byte(turnID + "\x00" + change.Path + "\x00" + change.MovePath + "\x00" + change.Kind + "\x00" + change.Diff))
					counts := fileSignatures[signature]
					if counts == nil {
						counts = map[string]int{}
						fileSignatures[signature] = counts
					}
					opposite := "patch_apply_end"
					if envelope == opposite {
						opposite = "item_completed"
					}
					if counts[opposite] > 0 {
						counts[opposite]--
						suppressedChanges++
						continue
					}
					counts[envelope]++
				}
				p.Files = append(p.Files, FileEntry{RecordedFileChange: c, Source: ref, TurnID: turnID, CallID: e.CallID})
			}
			if len(e.Changes) > 0 && suppressedChanges == len(e.Changes) {
				p.Ranges = append(p.Ranges, ExcludedRange{Path: s.Relative, Start: start, End: offset, EndLine: line, Reason: "mirror"})
			}
			if len(p.Turns)+len(p.Tools)+len(p.Files)+len(p.Ranges) > maxProjectionEntries {
				f.Close()
				return nil, fmt.Errorf("历史导航元数据超过 100000 项上限，未发布不完整投影；原始证据仍可读取")
			}
		}
		endLines[si] = line
		f.Close()
		if after, _, err := Version(s.Path); err != nil || !snapshotReadable(s, after) {
			return nil, fmt.Errorf("构建期间来源变化，请重试")
		}
	}
	if len(p.Turns)+len(p.Tools)+len(p.Files)+len(p.Ranges)+outputCount > maxProjectionEntries {
		return nil, fmt.Errorf("历史导航元数据超过 100000 项上限，未发布部分投影")
	}
	p.initialize()
	// 完整观察后才配对，重复 ID、撤回的调用和撤回的结果都不能提供成功证据。
	calls := map[string][]int{}
	for i, tool := range p.Tools {
		if !p.Excluded(tool.Call.Path, tool.Call.Start) {
			calls[tool.ID] = append(calls[tool.ID], i)
		}
	}
	for id, indexes := range calls {
		valid := []ToolPair{}
		for _, output := range outputs[id] {
			if !p.Excluded(output.Call.Path, output.Call.Start) {
				valid = append(valid, output)
			}
		}
		if len(indexes) != 1 || len(valid) > 1 {
			warn("重复的工具调用标识未自动配对")
			continue
		}
		if len(valid) == 1 {
			result := valid[0]
			tool := &p.Tools[indexes[0]]
			if !referenceBefore(plan, tool.Call, result.Call) {
				warn("工具结果早于调用，未自动配对")
				continue
			}
			tool.Result, tool.Status, tool.ExitCode = &result.Call, result.Status, result.ExitCode
			p.Ranges = append(p.Ranges, ExcludedRange{Path: result.Call.Path, Start: result.Call.Start, End: result.Call.End, EndLine: result.Call.Line, Reason: "paired"})
		}
	}
	p.initialize()
	// 显式结果优先于调用意图；失败或缺失结果不会成为已确认文件修改。
	for i := range p.Files {
		if p.Files[i].Status == "unconfirmed" && p.Files[i].CallID != "" {
			if tool := p.ToolAt(p.Files[i].Source.Path, p.Files[i].Source.Start); tool != nil && tool.Result != nil {
				p.Files[i].Status = tool.Status
			}
		}
	}
	return p, nil
}

func shortCaption(text string) string {
	r := []rune(strings.Join(strings.Fields(text), " "))
	if len(r) > 100 {
		return string(r[:100]) + "…"
	}
	return string(r)
}

func (p *Projection) Overview(offset, limit int, files bool) map[string]any {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if files {
		out := []FileEntry{}
		total := 0
		seen := map[string]bool{}
		for _, f := range p.Files {
			if p.Excluded(f.Source.Path, f.Source.Start) {
				continue
			}
			key := referenceKey(f.Source) + "\x00" + f.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			if total >= offset && len(out) < limit {
				out = append(out, f)
			}
			total++
		}
		return map[string]any{"files": out, "total": total, "has_more": offset+len(out) < total}
	}
	out := []Turn{}
	total := 0
	for _, t := range p.Turns {
		if t.Status == "rolled_back" {
			continue
		}
		if total >= offset && len(out) < limit {
			out = append(out, t)
		}
		total++
	}
	return map[string]any{"turns": out, "total": total, "has_more": offset+len(out) < total}
}
