// 中文说明：从 rollout 估算最近一个模型窗口；只保留记录位置和估算长度，不缓存历史正文。
package searchtext

import (
	"bytes"
	"encoding/json"
	"io"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

const defaultContextWindow = 128_000
const maxContextWindow = 2_000_000

type contextRecord struct {
	start, tokens int64
}

// RecentContextStart 返回保留范围的起始字节，向外对齐到完整记录。
// 压缩点不截断原文；窗口用不完时继续向前取，摘要本身走独立的索引规则。
func RecentContextStart(input io.Reader) (int64, error) {
	window := int64(defaultContextWindow)
	var records []contextRecord
	var head int
	var tokens int64
	var responseItems bool
	_, err := catalog.ScanReaderContent(input, func(frame domain.JSONLFrame, raw []byte) error {
		if !frame.ValidJSON || frame.Pending || frame.LineEnding == "none" {
			return nil
		}
		switch frame.TopLevelType {
		case "event_msg":
			if bytes.Contains(raw, []byte(`"token_count"`)) {
				var event struct {
					Payload struct {
						Type string `json:"type"`
						Info struct {
							Window int64 `json:"model_context_window"`
						} `json:"info"`
					} `json:"payload"`
				}
				if json.Unmarshal(raw, &event) == nil && event.Payload.Type == "token_count" && event.Payload.Info.Window > 0 {
					window = min(event.Payload.Info.Window, maxContextWindow)
				}
			}
			if responseItems {
				return nil // 同一条消息和工具记录常另外写成事件，优先用模型输入输出记录估算。
			}
		case "response_item":
		default:
			return nil
		}
		cost := contextRecordCost(frame, raw)
		if cost == 0 {
			return nil
		}
		if frame.TopLevelType == "response_item" && !responseItems {
			responseItems = true
			records, head, tokens = records[:0], 0, 0
		}
		records = append(records, contextRecord{frame.StartOffset, cost})
		tokens += cost
		// 较晚的记录可能报告不同的窗口大小，预留到支持的最大值再丢弃前缀位置。
		for head+1 < len(records) && tokens-records[head].tokens >= maxContextWindow {
			tokens -= records[head].tokens
			head++
		}
		if head > 0 && head >= len(records)/2 {
			records = records[:copy(records, records[head:])]
			head = 0
		}
		return nil
	})
	if err != nil || tokens < window {
		return 0, err
	}
	for i := len(records) - 1; i >= head; i-- {
		window -= records[i].tokens
		if window <= 0 {
			return records[i].start, nil
		}
	}
	return 0, nil
}

func contextRecordCost(frame domain.JSONLFrame, raw []byte) int64 {
	event, ok := compat.ParseFullReaderEvent(frame, raw)
	if !ok || event.Kind == "injected_context" {
		return 0
	}
	text := event.Text
	switch event.Role {
	case "user", "assistant":
	case "tool":
		if event.Tool != nil {
			if isToolOutput(event.Kind) && event.Tool.Output != "" {
				text = event.Tool.Output
			} else if event.Kind == "command_execution" {
				// 老日志只有完成事件时，估算命令和返回正文，避开结构化字段。
				return estimateContextTokens(event.Tool.Target) + estimateContextTokens(event.Tool.Output)
			}
		}
	default:
		return 0
	}
	return estimateContextTokens(text)
}

func estimateContextTokens(text string) int64 {
	if text == "" {
		return 0
	}
	// 英文和常见代码约四个 ASCII 字符一 token；其它字符按一字一 token 粗估。
	var ascii, other int64
	for _, r := range text {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return 4 + (ascii+3)/4 + other
}
