// 中文说明：从一条完整 JSONL 事件提取搜索文字，索引和源文件扫描共用相同的顺序和匹配规则。
package searchtext

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
)

// Record 返回完整的规范化文字。对象键按字典序处理，数组沿用原有顺序。
func Record(frame domain.JSONLFrame, content []byte) string {
	event, ok := compat.ParseFullReaderEvent(frame, content)
	return recordWithEvent(content, event, ok)
}

func recordWithEvent(content []byte, event domain.ConversationEvent, parsed bool) string {
	var decoded any
	if err := json.Unmarshal(content, &decoded); err != nil {
		// 有效 JSON 可能包含 float64 无法表示的数字，不能因此遗漏同一行的正文。
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return ""
		}
	}
	var text strings.Builder
	text.Grow(min(len(content), 16<<10))
	appendStrings(&text, decoded)
	projected := Normalize(text.String())
	// 阅读视图还会拼接消息片段、解码工具参数或展示作为对象键保存的改动路径。
	// 把这些可能不连续出现在原始字段中的文字也纳入候选。
	if parsed {
		reading := Normalize(reader.SearchableText(event))
		if !strings.Contains(projected, reading) {
			projected = reading + "\n" + projected
		}
	}
	return projected
}

func appendStrings(text *strings.Builder, value any) {
	switch value := value.(type) {
	case string:
		if value != "" {
			if text.Len() != 0 {
				text.WriteByte(' ')
			}
			text.WriteString(value)
		}
	case []any:
		for _, item := range value {
			appendStrings(text, item)
		}
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			switch key {
			case "encrypted_content", "base_instructions", "input_schema":
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			appendStrings(text, value[key])
		}
	}
}

// Snippet 只截取命中附近的字符，避免将整条长记录转成 rune 切片。
func Snippet(text, needle string) string {
	at := strings.Index(text, needle)
	if at < 0 {
		return ""
	}
	start, end := at, at+len(needle)
	for n := 0; n < 80 && start > 0; n++ {
		_, size := utf8.DecodeLastRuneInString(text[:start])
		start -= size
	}
	for n := 0; n < 160 && end < len(text); n++ {
		_, size := utf8.DecodeRuneInString(text[end:])
		end += size
	}
	return strings.Clone(text[start:end])
}
