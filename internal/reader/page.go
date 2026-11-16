// 中文说明：从当前字节继续有界读取，支持大行诊断、跨文件定位、追加和失效游标。
package reader

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

func EncodePosition(p Position) string {
	b, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(b)
}

// SearchableText 统一阅读搜索的正文口径，包含已记录的工具输出与文件差异。
func SearchableText(event domain.ConversationEvent) string {
	var text strings.Builder
	text.WriteString(event.Text)
	if event.Tool != nil {
		text.WriteString("\n" + event.Tool.Target + "\n" + event.Tool.Output)
	}
	for _, change := range event.Changes {
		text.WriteString("\n" + change.Path + "\n" + change.MovePath + "\n" + change.Diff)
	}
	return text.String()
}
func DecodePosition(raw string) (Position, error) {
	var p Position
	if len(raw) > 4096 {
		return p, fmt.Errorf("游标过长")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return p, fmt.Errorf("游标无效")
	}
	err = json.Unmarshal(b, &p)
	if err != nil || p.Offset < 0 || p.Path == "" {
		return p, fmt.Errorf("游标无效")
	}
	return p, nil
}

// boundedLine 在长行中也只保留指定大小，返回实际消耗字节数，防止 ReadBytes 无界分配。
func boundedLine(r *bufio.Reader, max int) ([]byte, int64, bool, error) {
	var out []byte
	var consumed int64
	oversized := false
	for {
		part, err := r.ReadSlice('\n')
		consumed += int64(len(part))
		if len(out)+len(part) <= max && !oversized {
			out = append(out, part...)
		} else {
			oversized = true
			out = nil
		}
		if consumed >= int64(max) && err == bufio.ErrBufferFull {
			return nil, consumed, true, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if oversized && err == nil {
			out = []byte{'\n'}
		}
		return out, consumed, oversized, err
	}
}

func guard(path string, offset int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	n := offset
	if n > 256 {
		n = 256
	}
	b := make([]byte, n)
	if _, err := f.ReadAt(b, offset-n); err != nil && err != io.EOF {
		return ""
	}
	return shortHash(b)
}

func ReadPage(ctx context.Context, plan Plan, start Position, limit int, locate FrameLocator, full bool) (Page, error) {
	page := Page{Items: []domain.ConversationEvent{}, Complete: plan.Complete, Diagnostics: append([]string{}, plan.Diagnostics...)}
	page.SourceStamp = Stamp(plan)
	page.Versions, page.Source, page.Mainline = plan.Versions, plan.Leaf, plan.Mainline
	if plan.Projection != nil {
		page.Diagnostics = append(page.Diagnostics, plan.Projection.Diagnostics...)
		if len(plan.Projection.Diagnostics) > 0 {
			page.Complete = false
		}
		if start.ProjectionStamp != "" && start.ProjectionStamp != plan.Projection.Revision {
			return page, fmt.Errorf("有效历史已变化，请重新加载当前历史")
		}
	}
	if len(plan.Segments) == 0 {
		return page, nil
	}
	if limit <= 0 || limit > 80 {
		limit = 80
	}
	index := 0
	if start.Path == "" {
		start.Path = plan.Segments[0].Relative
	}
	for index < len(plan.Segments) && plan.Segments[index].Relative != start.Path {
		index++
	}
	if index == len(plan.Segments) {
		return page, fmt.Errorf("定位不属于当前会话的已验证历史")
	}
	initial := start
	start.Source = plan.Leaf
	textBytes := 0
	stop := false
	for ; index < len(plan.Segments) && !stop; index++ {
		s := plan.Segments[index]
		if start.Path != s.Relative {
			start = Position{Path: s.Relative, Source: plan.Leaf}
		}
		version, size, err := Version(s.Path)
		if err != nil {
			return page, err
		}
		if !snapshotReadable(s, version) {
			return page, fmt.Errorf("历史来源已改写，请重新加载")
		}
		page.SourceChanged = page.SourceChanged || version != s.Version
		if start.Offset > s.End {
			return page, fmt.Errorf("来源已截断，请重新加载")
		}
		if start.Version != "" && start.Version != version {
			oldParts, newParts := strings.Split(start.Version, ":"), strings.Split(version, ":")
			appendOnly := size > start.Size && len(oldParts) == 3 && len(newParts) == 3 && oldParts[2] == newParts[2] && start.Guard != "" && start.Guard == guard(s.Path, start.Offset)
			if !appendOnly {
				return page, fmt.Errorf("来源版本已变化，请重新定位")
			}
		}
		f, err := os.Open(s.Path)
		if err != nil {
			return page, err
		}
		if start.Offset > 0 && !start.Skipping {
			var b [1]byte
			if _, err := f.ReadAt(b[:], start.Offset-1); err != nil || b[0] != '\n' {
				f.Close()
				return page, fmt.Errorf("定位不是完整记录边界")
			}
		}
		if _, err := f.Seek(start.Offset, io.SeekStart); err != nil {
			f.Close()
			return page, err
		}
		r := bufio.NewReader(io.LimitReader(f, s.End-start.Offset))
		for start.Offset < s.End {
			if err := ctx.Err(); err != nil {
				f.Close()
				return page, err
			}
			before := start
			if span := plan.Projection.excludedRange(s.Relative, before.Offset); span != nil {
				// 排除区间直接跳过，避免逐行重读已撤回的长输出。
				start.Offset = min(span.End, s.End)
				start.Line = span.EndLine
				start.LastHash = ""
				start.Turn = ""
				_, _ = f.Seek(start.Offset, io.SeekStart)
				r.Reset(io.LimitReader(f, s.End-start.Offset))
				continue
			}
			lineKnown := start.Offset == 0 || start.Line > 0
			b, n, oversized, readErr := boundedLine(r, MaxLineBytes)
			page.BytesRead += n
			if readErr != nil && readErr != io.EOF {
				f.Close()
				return page, readErr
			}
			if n == 0 {
				break
			}
			if readErr == io.EOF && !bytes.HasSuffix(b, []byte{'\n'}) && !oversized && !start.Skipping {
				page.Pending = true
				page.Diagnostics = append(page.Diagnostics, "来源末尾尚未完成一行，等待写入完成")
				stop = true
				break
			}
			start.Offset += n
			if oversized || start.Skipping {
				page.Diagnostics = append(page.Diagnostics, "有记录超过 8 MiB，已跳过正文渲染，可查看原始文件")
				start.Skipping = !bytes.HasSuffix(b, []byte{'\n'})
				if !start.Skipping && lineKnown {
					start.Line++
				}
			} else {
				if lineKnown {
					start.Line++
				}
				var wire struct {
					Type    string `json:"type"`
					Payload struct {
						Type string `json:"type"`
						Turn string `json:"turn_id"`
					} `json:"payload"`
				}
				_ = json.Unmarshal(b, &wire)
				if wire.Payload.Turn != "" {
					if wire.Payload.Turn != start.Turn {
						start.LastHash = ""
					}
					start.Turn = wire.Payload.Turn
					start.Inferred = false
				}
				frame := domain.JSONLFrame{LineNumber: start.Line, StartOffset: before.Offset, EndOffset: start.Offset, ValidJSON: json.Valid(b), SHA256: shortHash(b)}
				e, ok := compat.ParseReaderEvent(frame, b)
				if full {
					e, ok = compat.ParseFullReaderEvent(frame, b)
				}
				if ok {
					if plan.Projection != nil {
						if turn := plan.Projection.turnAt(plan, s.Relative, before.Offset); turn != nil {
							start.Turn, start.Inferred = turn.ID, turn.Inferred
						}
					}
					// 仅去重已知双写格式，普通用户连续发送相同内容仍保留。
					hash := shortHash([]byte(e.Text))
					if e.Truncated {
						if whole, ok := compat.ParseFullReaderEvent(frame, b); ok {
							hash = shortHash([]byte(whole.Text))
						}
					}
					mirrored := (wire.Type == "response_item" && start.LastType == "event_msg") || (wire.Type == "event_msg" && start.LastType == "response_item")
					duplicate := plan.Projection == nil && mirrored && start.LastRole == e.Role && start.LastHash == hash && (e.Role == "user" || e.Role == "assistant")
					start.LastHash = hash
					start.LastType = wire.Type
					start.LastRole = e.Role
					if duplicate {
						start.LastHash = ""
						if page.BytesRead >= PageReadBytes {
							stop = true
							break
						}
						continue
					}
					e.RelativePath = s.Relative
					e.Anchor = shortHash(b)
					e.SourceVersion = version
					e.ID = shortHash([]byte(fmt.Sprintf("%s:%d:%s", s.Relative, before.Offset, e.Anchor)))
					if plan.Projection == nil && e.Role == "user" && e.Kind != "injected_context" && e.Kind != "agent_task" && wire.Payload.Turn == "" && (start.Turn == "" || start.Inferred) {
						start.Turn = e.ID
						start.Inferred = true
					}
					e.TurnID = start.Turn
					e.TurnInferred = start.Inferred
					if pair := plan.Projection.ToolAt(s.Relative, before.Offset); pair != nil {
						e.Status, e.ExitCode = pair.Status, pair.ExitCode
						for i := range e.Changes {
							if e.Changes[i].Status == "unconfirmed" {
								e.Changes[i].Status = pair.Status
							}
						}
						if pair.Result != nil && e.Tool != nil {
							e.Tool.Result = pair.Result
							// 普通页只携带证据地址；展开工具时再读取关联输出，避免结果突破页预算。
							if full {
								result, n, err := ReadReference(ctx, plan, *pair.Result, full)
								page.BytesRead += n
								if err != nil {
									f.Close()
									return page, err
								}
								e.Tool.Output = result.Text
								e.Tool.OutputTruncated = result.Truncated
							}
						}
					}

					page.Items = append(page.Items, e)
					textBytes += len(e.Text)
				}
				if wire.Payload.Type == "task_complete" || wire.Payload.Type == "turn_aborted" {
					start.Turn = ""
					start.Inferred = false
					start.LastHash = ""
				}
			}
			if len(page.Items) >= limit || page.BytesRead >= PageReadBytes || textBytes >= PageTextBytes {
				stop = true
				break
			}
		}
		f.Close()
		if after, _, err := Version(s.Path); err != nil || !snapshotReadable(s, after) {
			return page, fmt.Errorf("读取期间历史来源已改写，请重新加载")
		} else if after != version {
			page.SourceChanged = true
		}
		start.Version = version
		start.Size = size
		start.Guard = guard(s.Path, start.Offset)
		if plan.Projection != nil {
			start.ProjectionStamp = plan.Projection.Revision
		}
		if stop || index == len(plan.Segments)-1 || start.Offset < s.End {
			page.Next = EncodePosition(start)
		}
		if start.Offset >= s.End && index+1 < len(plan.Segments) {
			next := Position{Path: plan.Segments[index+1].Relative, Source: plan.Leaf, ProjectionStamp: start.ProjectionStamp}
			page.Next = EncodePosition(next)
		}
		page.HasMore = start.Offset < s.End || index+1 < len(plan.Segments)
	}
	// 上一页通过 frame 索引定位，不从文件头重新解析。
	if locate != nil {
		if offset, line, err := locate(initial.Path, initial.Offset, 60); err == nil && offset < initial.Offset {
			page.Previous = EncodePosition(Position{Path: initial.Path, Offset: offset, Line: line, Source: plan.Leaf, ProjectionStamp: start.ProjectionStamp})
		} else if initial.Offset == 0 {
			for i, segment := range plan.Segments {
				if segment.Relative == initial.Path && i > 0 {
					prior := plan.Segments[i-1]
					if offset, line, err := locate(prior.Relative, prior.End, 60); err == nil {
						page.Previous = EncodePosition(Position{Path: prior.Relative, Offset: offset, Line: line, Source: plan.Leaf, ProjectionStamp: start.ProjectionStamp})
					}
					break
				}
			}
		}
	}
	return page, nil
}

// LocateBefore 在 frame 索引缺失时仅读取目标前的有界字节窗口，不顺扫完整前缀。
func LocateBefore(source Source, offset int64, count int) (int64, uint64, error) {
	f, err := os.Open(source.Path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	if offset > info.Size() {
		offset = info.Size()
	}
	start := offset - int64(MaxLineBytes)
	if start < 0 {
		start = 0
	}
	b := make([]byte, offset-start)
	if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
		return 0, 0, err
	}
	positions := []int64{}
	if start == 0 {
		positions = append(positions, 0)
	}
	for i, c := range b {
		if c == '\n' && start+int64(i)+1 < offset {
			positions = append(positions, start+int64(i)+1)
		}
	}
	if len(positions) == 0 {
		if start == 0 {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("附近记录超过读取上限，无法定位")
	}
	index := len(positions) - count
	if index < 0 {
		index = 0
	}
	return positions[index], 0, nil
}

// Stamp 只编码路径和来源版本，不含正文或本机绝对路径。
func Stamp(plan Plan) string {
	var value strings.Builder
	for _, s := range plan.Segments {
		value.WriteString(s.Relative)
		value.WriteString(s.Version)
		value.WriteString(fmt.Sprint(s.End))
	}
	return shortHash([]byte(value.String()))
}
