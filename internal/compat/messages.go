// 中文说明：从 rollout JSONL 流式提取可审阅的消息和事件窗口，并保留原始行定位证据。
package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
)

const maxEventPreviewBytes = 64 * 1024

// ReadRolloutEventsPath 以分页方式读取单个 rollout 的事件，不保存完整正文。
// total 是可展示事件总数，items 只包含 [offset, offset+limit) 窗口。
func ReadRolloutEventsPath(ctx context.Context, path string, offset, limit int) (items []domain.ConversationEvent, total int, scan domain.JSONLScanSummary, err error) {
	if offset < 0 || limit <= 0 {
		return nil, 0, scan, fmt.Errorf("事件分页参数无效")
	}
	input, err := os.Open(path)
	if err != nil {
		return nil, 0, scan, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, 0, scan, err
	}
	items = make([]domain.ConversationEvent, 0, limit)
	scan, err = catalog.ScanReaderContent(io.LimitReader(input, info.Size()), func(frame domain.JSONLFrame, content []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		event, ok := parseConversationEvent(frame, content)
		if !ok {
			return nil
		}
		if total >= offset && len(items) < limit {
			items = append(items, event)
		}
		total++
		return nil
	})
	if err != nil {
		return nil, total, scan, err
	}
	return items, total, scan, nil
}

func parseConversationEvent(frame domain.JSONLFrame, content []byte) (domain.ConversationEvent, bool) {
	return parseConversationProjection(frame, content, false)
}

func parseConversationProjection(frame domain.JSONLFrame, content []byte, full bool) (domain.ConversationEvent, bool) {
	if !frame.ValidJSON {
		return domain.ConversationEvent{}, false
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(content, &record); err != nil {
		return domain.ConversationEvent{}, false
	}
	payload := rawObject(record["payload"])
	if payload == nil {
		payload = record
	}
	topType := rawString(record["type"])
	payloadType := rawString(payload["type"])
	role := firstNonEmpty(rawString(payload["role"]), rawString(record["role"]))
	text := ""
	item := rawObject(payload["item"])
	status := ""
	var exitCode *int
	toolName := rawString(payload["name"])
	callID := rawString(payload["call_id"])
	itemID := rawString(payload["id"])
	var changes []domain.RecordedFileChange
	switch {
	case topType == "event_msg" && payloadType == "item_completed" && item != nil:
		itemID = rawString(item["id"])
		status = executionStatus(rawString(item["status"]))
		switch rawString(item["type"]) {
		case "UserMessage", "AgentMessage":
			role = "assistant"
			if rawString(item["type"]) == "UserMessage" {
				role = "user"
			}
			text = extractRecordedText(item["content"])
			payloadType = "message"
		case "Reasoning":
			role, payloadType = "context", "reasoning"
			text = firstNonEmpty(extractRecordedText(item["summary_text"]), extractRecordedText(item["raw_content"]))
		case "CommandExecution":
			role, payloadType, toolName = "tool", "command_execution", "shell_command"
			callID = firstNonEmpty(rawString(item["call_id"]), itemID)
			text = string(payload["item"])
			if len(item["exit_code"]) > 0 && string(item["exit_code"]) != "null" {
				var code int
				if json.Unmarshal(item["exit_code"], &code) == nil {
					exitCode = &code
				}
			}
		case "FileChange":
			role, payloadType, toolName = "tool", "file_change", "apply_patch"
			callID = firstNonEmpty(rawString(item["call_id"]), itemID)
			changes = recordedChanges(item["changes"], status)
			text = "文件改动记录"
		case "ContextCompaction":
			role, payloadType, text = "context", "context_compaction", "上下文已压缩"
		case "Plan":
			role, payloadType, text = "context", "plan", extractRecordedText(item["plan"])
		default:
			return domain.ConversationEvent{}, false
		}
	case topType == "event_msg" && payloadType == "patch_apply_end":
		role, toolName = "tool", "apply_patch"
		status = executionStatus(rawString(payload["status"]))
		if string(payload["success"]) == "true" {
			status = "completed"
		} else if string(payload["success"]) == "false" {
			status = "failed"
		}
		if status == "" {
			status = "unconfirmed"
		}
		changes = recordedChanges(payload["changes"], status)
		text = "文件改动记录"
	case payloadType == "local_shell_call" || payloadType == "web_search_call":
		role = "tool"
		toolName = "shell_command"
		if payloadType == "web_search_call" {
			toolName = "web_search"
		}
		text = string(payload["action"])
		status = executionStatus(rawString(payload["status"]))
	case payloadType == "context_compacted" || topType == "compacted":
		role, payloadType = "context", "context_compaction"
		text = firstNonEmpty(rawString(payload["message"]), "上下文已压缩")
	case payloadType == "turn_aborted":
		role, text = "context", "本轮已中断"
	case payloadType == "thread_rolled_back":
		role, text = "context", "历史轮次已撤回"
	case payloadType == "function_call" || payloadType == "custom_tool_call":
		text = firstNonEmpty(rawString(payload["arguments"]), rawString(payload["input"]))
		role = "tool"
	case payloadType == "function_call_output" || payloadType == "custom_tool_call_output":
		text = firstNonEmpty(extractRecordedText(payload["output"]), rawString(payload["text"]))
		role = "tool"
	case payloadType == "agent_message":
		text = firstNonEmpty(rawString(payload["message"]), rawString(payload["text"]))
		role = firstNonEmpty(role, "assistant")
	case payloadType == "message":
		text = firstNonEmpty(extractRecordedText(payload["content"]), rawString(payload["text"]))
	case payloadType == "user_message":
		text = firstNonEmpty(rawString(payload["message"]), rawString(payload["text"]), rawString(record["message"]), rawString(record["text"]))
		role = firstNonEmpty(role, "user")
	case role != "" && len(payload["content"]) > 0:
		text = extractMessageText(payload["content"])
	default:
		text = firstNonEmpty(rawString(payload["text"]), rawString(payload["message"]), rawString(payload["summary"]))
	}
	if text == "" {
		if role == "tool" && toolName != "" {
			text = "{}"
		} else {
			return domain.ConversationEvent{}, false
		}
	}
	preview, truncated := text, false
	if !full {
		preview, truncated = limitEventText(text)
	}
	event := domain.ConversationEvent{
		ID:           fmt.Sprintf("%s:%d", frame.SHA256, frame.LineNumber),
		Role:         role,
		Kind:         firstNonEmpty(payloadType, topType),
		TopLevelType: topType,
		PayloadType:  payloadType,
		Text:         preview,
		Truncated:    truncated,
		Line:         frame.LineNumber,
		StartOffset:  frame.StartOffset,
		EndOffset:    frame.EndOffset,
		Timestamp:    firstTime(record["timestamp"], payload["timestamp"]),
		TurnID:       firstNonEmpty(rawString(payload["turn_id"]), rawString(record["turn_id"])),
		ToolName:     toolName,
		CallID:       callID,
		ItemID:       itemID,
		Status:       status,
		ExitCode:     exitCode,
		Changes:      changes,
	}
	if role == "tool" {
		presentTool(&event, text, full)
	}
	if event.Kind == "" {
		event.Kind = "event"
	}
	if role == "user" && IsInjectedContext(text) {
		event.Kind = "injected_context"
	}
	if role == "user" {
		if reply, ok := questionReplyText(text); ok {
			event.Kind, event.Text = "question_reply", reply
		}
	}
	if payloadType == "agent_message" && strings.Contains(text, "Message Type: NEW_TASK") {
		event.Kind, event.Role = "agent_task", "user"
	}
	return event, true
}

func limitEventText(value string) (string, bool) {
	data := []byte(value)
	if len(data) <= maxEventPreviewBytes {
		return value, false
	}
	data = data[:maxEventPreviewBytes]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data) + "…", true
}

// ParseReaderEvent 与旧事件接口共用投影，不让阅读器重新解释一套角色规则。
func ParseReaderEvent(frame domain.JSONLFrame, content []byte) (domain.ConversationEvent, bool) {
	return parseConversationEvent(frame, content)
}

// ParseFullReaderEvent 在单条 8 MiB 上限内读取完整投影，不改变预览 API 的限制。
func ParseFullReaderEvent(frame domain.JSONLFrame, content []byte) (domain.ConversationEvent, bool) {
	return parseConversationProjection(frame, content, true)
}

// ReadRolloutEventSourceRange 以只读方式读取事件的完整原始行，供 UI 需要展开时使用。
func ReadRolloutEventSourceRange(ctx context.Context, path string, event domain.ConversationEvent) ([]byte, error) {
	return catalog.ReadRange(ctx, path, event.StartOffset, event.EndOffset-event.StartOffset, catalog.DefaultMaxReadBytes)
}
