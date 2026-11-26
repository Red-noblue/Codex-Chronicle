// 中文说明：集中定义搜索入口共用的文本规范化规则，保证索引搜索和按需扫描语义一致。
package searchtext

import (
	"strings"
	"unicode"
)

// Normalize 将 JSON 转义换行、真实换行和连续 Unicode 空白折叠为同一搜索形式。
// 该函数只用于匹配，不改变原始 JSONL、snippet 或字节范围证据。
func Normalize(value string) string {
	value = strings.ReplaceAll(value, `\n`, " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	var builder strings.Builder
	builder.Grow(len(value))
	space := false
	for _, character := range strings.ToLower(value) {
		if unicode.IsSpace(character) {
			space = true
			continue
		}
		if space && builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		space = false
		builder.WriteRune(character)
	}
	return builder.String()
}
