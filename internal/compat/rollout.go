// 中文说明：从多版本 Codex rollout JSONL 提取安全的只读对话投影和诊断信息。
package compat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
)

const (
	maxPreviewBytes = 1024
	maxDiagnostic   = 8
)

// AnalyzeRolloutPath 以只读方式分析一个 rollout，并返回 framing 聚合结果。
func AnalyzeRolloutPath(path string) (domain.RolloutSummary, domain.JSONLScanSummary, error) {
	input, err := os.Open(path)
	if err != nil {
		return domain.RolloutSummary{}, domain.JSONLScanSummary{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return domain.RolloutSummary{}, domain.JSONLScanSummary{}, err
	}
	return AnalyzeRollout(io.LimitReader(input, info.Size()), filepath.ToSlash(path))
}

// AnalyzeRollout 从 JSONL 流中提取元数据、消息计数、token 统计和状态诊断。
// 它不会保存正文；正文仍应通过原始文件的字节范围接口读取。
func AnalyzeRollout(reader io.Reader, fallbackSource string) (domain.RolloutSummary, domain.JSONLScanSummary, error) {
	projection := domain.RolloutSummary{Status: domain.StatusStable}
	sessionMetaSeen := false
	turns := make(map[string]struct{})
	userMessageSeen := make(map[string]struct{})
	assistantMessageSeen := make(map[string]struct{})
	scan, err := catalog.ScanReaderContent(reader, func(frame domain.JSONLFrame, content []byte) error {
		if !frame.ValidJSON {
			if len(projection.Diagnostics) < maxDiagnostic {
				code := "JSONL_INVALID"
				severity := domain.SeverityError
				message := "该行不是有效 JSON，当前只能诊断和读取"
				if frame.Pending {
					code = "JSONL_INCOMPLETE"
					severity = domain.SeverityWarning
					message = "文件末尾存在未完成 JSON 行，可能仍在写入或被中断"
				}
				projection.Diagnostics = append(projection.Diagnostics, domain.Diagnostic{
					Code: code, Severity: severity, Message: message,
					Line: frame.LineNumber, Evidence: fmt.Sprintf("字节范围 %d-%d", frame.StartOffset, frame.EndOffset), ReadOnly: true,
				})
			}
			if frame.Pending && projection.Status == domain.StatusStable {
				projection.Status = domain.StatusUnknown
			} else if projection.Status == domain.StatusStable {
				projection.Status = domain.StatusUnknown
			}
			return nil
		}

		projection.EventCount++
		var record map[string]json.RawMessage
		if err := json.Unmarshal(content, &record); err != nil {
			return nil
		}
		payload := rawObject(record["payload"])
		topType := rawString(record["type"])
		payloadType := rawString(payload["type"])
		if topType == "session_meta" {
			// fork 会把祖先的 session_meta 一并复制；首条才是本文件的身份，不能被继承历史覆盖。
			if sessionMetaSeen {
				return nil
			}
			sessionMetaSeen = true
		}
		if payloadType == "thread_rolled_back" || topType == "session_meta" && (len(payload["history_base"]) > 0 || rawString(payload["history_mode"]) == "standalone") {
			projection.HistoryEdited = true
		}
		when := firstTime(record["timestamp"], payload["timestamp"])
		if !when.IsZero() {
			if projection.StartedAt.IsZero() || when.Before(projection.StartedAt) {
				projection.StartedAt = when
			}
			if projection.UpdatedAt.IsZero() || when.After(projection.UpdatedAt) {
				projection.UpdatedAt = when
			}
		}

		if topType == "session_meta" {
			projection.SourceID = firstNonEmpty(rawString(payload["id"]), rawString(record["id"]))
			projection.SessionID = firstNonEmpty(rawString(payload["session_id"]), rawString(record["session_id"]))
			projection.CWD = firstNonEmpty(rawString(payload["cwd"]), rawString(record["cwd"]))
			projection.ModelProvider = firstNonEmpty(rawString(payload["model_provider"]), rawString(record["model_provider"]))
			projection.CLIVersion = firstNonEmpty(rawString(payload["cli_version"]), rawString(record["cli_version"]))
			projection.ThreadID = firstNonEmpty(rawString(payload["thread_id"]), rawString(record["thread_id"]))
			projection.ThreadOrigin = threadOrigin(payload)
		}

		projection.CWD = firstNonEmpty(projection.CWD, rawString(payload["cwd"]), rawString(record["cwd"]))
		projection.Model = firstNonEmpty(projection.Model, rawString(payload["model"]), rawString(record["model"]))
		projection.ModelProvider = firstNonEmpty(projection.ModelProvider, rawString(payload["model_provider"]), rawString(record["model_provider"]))
		projection.ThreadID = firstNonEmpty(projection.ThreadID, rawString(payload["thread_id"]), rawString(record["thread_id"]))
		if metadata := rawObject(payload["internal_chat_message_metadata_passthrough"]); metadata != nil {
			projection.ThreadID = firstNonEmpty(projection.ThreadID, rawString(metadata["thread_id"]))
		}

		turnID := firstNonEmpty(rawString(payload["turn_id"]), rawString(record["turn_id"]))
		if turnID != "" {
			turns[turnID] = struct{}{}
		}

		role := firstNonEmpty(rawString(payload["role"]), rawString(record["role"]))
		if payloadType == "message" || role != "" {
			text := extractMessageText(payload["content"])
			if text != "" && role == "user" && !IsInjectedContext(text) {
				if addMessageFingerprint(userMessageSeen, text, frame.LineNumber) {
					projection.UserMessages++
					if projection.FirstUserText == "" {
						projection.FirstUserText = limitPreview(text)
					}
				}
			} else if role == "assistant" {
				if addMessageFingerprint(assistantMessageSeen, text, frame.LineNumber) {
					projection.AssistantMessages++
				}
			}
		}
		if payloadType == "user_message" || (topType == "event_msg" && payloadType == "user_message") {
			text := firstNonEmpty(rawString(payload["message"]), rawString(record["message"]))
			if text != "" && !IsInjectedContext(text) {
				if addMessageFingerprint(userMessageSeen, text, frame.LineNumber) {
					projection.UserMessages++
					if projection.FirstUserText == "" {
						projection.FirstUserText = limitPreview(text)
					}
				}
			}
		}
		if topType == "event_msg" && payloadType == "item_completed" {
			if event, ok := ParseFullReaderEvent(frame, content); ok {
				if event.Role == "user" && event.Kind != "injected_context" && addMessageFingerprint(userMessageSeen, event.Text, frame.LineNumber) {
					projection.UserMessages++
					if projection.FirstUserText == "" {
						projection.FirstUserText = limitPreview(event.Text)
					}
				}
				if event.Role == "assistant" && addMessageFingerprint(assistantMessageSeen, event.Text, frame.LineNumber) {
					projection.AssistantMessages++
				}
			}
		}
		if projection.FirstUserText != "" && projection.Title == "" {
			projection.Title = titleFromText(projection.FirstUserText)
		}
		updateTokenUsage(&projection.TokenUsage, record, payload)
		return nil
	})
	if err != nil {
		return projection, scan, err
	}
	projection.TurnCount = len(turns)
	if projection.SourceID == "" {
		projection.SourceID = projection.ThreadID
	}
	if projection.SourceID == "" {
		projection.SourceID = fallbackSource
		projection.Orphan = true
		projection.Status = domain.StatusUnknown
		projection.Diagnostics = appendDiagnostic(projection.Diagnostics, domain.Diagnostic{
			Code: "ROLL_OUT_ID_MISSING", Severity: domain.SeverityWarning,
			Message:  "未发现 session/thread 标识，暂按独立 JSONL 资产展示，不能推断对话关系",
			ReadOnly: true,
		})
	}
	projection.ConversationID = stableConversationID(projection.SourceID)
	if projection.Title == "" {
		projection.Title = firstNonEmpty(OriginTitle(projection.ThreadOrigin), filepath.Base(fallbackSource))
	}
	if scan.PendingLines > 0 && projection.Status == domain.StatusStable {
		projection.Status = domain.StatusUnknown
	}
	if scan.InvalidUTF8Lines > 0 {
		projection.Diagnostics = appendDiagnostic(projection.Diagnostics, domain.Diagnostic{
			Code: "JSONL_INVALID_UTF8", Severity: domain.SeverityError,
			Message: "文件包含非法 UTF-8 字节，正文解析被限制为只读诊断", ReadOnly: true,
		})
		if projection.Status == domain.StatusStable {
			projection.Status = domain.StatusUnknown
		}
	}
	return projection, scan, nil
}

func addMessageFingerprint(seen map[string]struct{}, text string, line uint64) bool {
	key := strings.TrimSpace(text)
	if key == "" {
		key = fmt.Sprintf("line:%d", line)
	}
	if _, exists := seen[key]; exists {
		return false
	}
	seen[key] = struct{}{}
	return true
}

func rawObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstTime(values ...json.RawMessage) time.Time {
	for _, raw := range values {
		if text := rawString(raw); text != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
				return parsed.UTC()
			}
			if seconds, err := strconv.ParseInt(text, 10, 64); err == nil {
				return time.Unix(seconds, 0).UTC()
			}
		}
		var number float64
		if json.Unmarshal(raw, &number) == nil && number > 0 {
			if number > 1e12 {
				return time.UnixMilli(int64(number)).UTC()
			}
			return time.Unix(int64(number), 0).UTC()
		}
	}
	return time.Time{}
}

func extractMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			if object := rawObject(item); object != nil {
				if text := rawString(object["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return rawString(raw)
}

func updateTokenUsage(target *domain.TokenUsage, record, payload map[string]json.RawMessage) {
	for _, object := range []map[string]json.RawMessage{
		rawObject(payload["info"]), rawObject(payload["usage"]), rawObject(payload["thread_token_usage"]),
		rawObject(record["usage"]), rawObject(record["thread_token_usage"]),
	} {
		if object == nil {
			continue
		}
		if nested := rawObject(object["total_token_usage"]); nested != nil {
			object = nested
		}
		candidate := domain.TokenUsage{
			InputTokens:           rawInt64(object["input_tokens"]),
			CachedInputTokens:     rawInt64(object["cached_input_tokens"]),
			OutputTokens:          rawInt64(object["output_tokens"]),
			ReasoningOutputTokens: rawInt64(object["reasoning_output_tokens"]),
			TotalTokens:           rawInt64(object["total_tokens"]),
		}
		if candidate.TotalTokens >= target.TotalTokens {
			*target = candidate
		}
	}
}

func rawInt64(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return number
	}
	return 0
}

func limitPreview(value string) string {
	return limitTextBytes(value, maxPreviewBytes)
}

func limitTextBytes(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + "…"
}

func titleFromText(value string) string {
	return limitTextBytes(strings.ReplaceAll(value, "\n", " "), 96)
}

func appendDiagnostic(items []domain.Diagnostic, item domain.Diagnostic) []domain.Diagnostic {
	if len(items) >= maxDiagnostic {
		return items
	}
	return append(items, item)
}

func stableConversationID(source string) string {
	digest := sha256.Sum256([]byte(source))
	return "conversation-" + hex.EncodeToString(digest[:16])
}

// StableConversationID 为搜索和其他只读投影提供与 rollout 分析一致的逻辑 ID。
func StableConversationID(source string) string {
	return stableConversationID(source)
}
