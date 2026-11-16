// 中文说明：以流式方式扫描 JSONL，记录字节范围、事件类型和解析状态，不写入源文件。
package catalog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"codex-chronicle.local/internal/domain"
)

// ErrLimitReached 表示调用方只需要当前前缀结果，扫描器可以安全结束读取。
// 活动文件封装会继续复核源文件状态，不会把提前结束误报为稳定快照。
var ErrLimitReached = errors.New("catalog scan limit reached")

// FrameHandler 接收一条 framing 结果；返回错误会立即停止扫描。
type FrameHandler func(domain.JSONLFrame) error

// ContentFrameHandler 在 framing 结果之外接收当前行内容。内容只在回调期间有效，
// 调用方应立即解析或复制，不能把它当作持久化正文。
type ContentFrameHandler func(domain.JSONLFrame, []byte) error

// ScanReader 从 reader 中顺序读取 JSONL。reader 由调用方以只读模式打开。
func ScanReader(reader io.Reader, handler FrameHandler) (domain.JSONLScanSummary, error) {
	return ScanReaderContent(reader, func(frame domain.JSONLFrame, _ []byte) error {
		if handler == nil {
			return nil
		}
		return handler(frame)
	})
}

// ScanReaderContent 从 reader 中顺序读取 JSONL，并在每行完成 framing 后提供行内容。
// 该函数仍然只读取，不会修改输入。
func ScanReaderContent(reader io.Reader, handler ContentFrameHandler) (domain.JSONLScanSummary, error) {
	summary := domain.JSONLScanSummary{TopLevelTypes: make(map[string]uint64)}
	buffered := bufio.NewReader(reader)
	contentHash := sha256.New()
	var offset int64
	var line uint64

	for {
		raw, err := buffered.ReadBytes('\n')
		if len(raw) == 0 && err == io.EOF {
			break
		}
		if err != nil && err != io.EOF {
			return summary, fmt.Errorf("读取 JSONL 失败: %w", err)
		}

		line++
		start := offset
		offset += int64(len(raw))
		if _, hashErr := contentHash.Write(raw); hashErr != nil {
			return summary, fmt.Errorf("计算 JSONL 内容指纹失败: %w", hashErr)
		}
		content, lineEnding := trimLineEnding(raw)
		validUTF8 := utf8.Valid(content)
		validJSON, incompleteJSON, topType := parseJSONLine(content, validUTF8)
		summary.LineCount++
		if !validUTF8 {
			summary.InvalidUTF8Lines++
		}
		if !validJSON {
			summary.InvalidJSONLines++
		}
		pending := err == io.EOF && lineEnding == "none" && incompleteJSON
		if pending {
			summary.PendingLines++
		}
		if size := int64(len(raw)); size > summary.MaxLineBytes {
			summary.MaxLineBytes = size
		}
		summary.TopLevelTypes[topType]++

		digest := sha256.Sum256(raw)
		frame := domain.JSONLFrame{
			LineNumber:       line,
			StartOffset:      start,
			EndOffset:        offset,
			ContentEndOffset: offset - int64(len(raw)-len(content)),
			LineEnding:       lineEnding,
			ValidUTF8:        validUTF8,
			ValidJSON:        validJSON,
			Pending:          pending,
			TopLevelType:     topType,
			SHA256:           hex.EncodeToString(digest[:]),
		}
		if handler != nil {
			if callbackErr := handler(frame, content); callbackErr != nil {
				return summary, callbackErr
			}
		}
		if err == io.EOF {
			break
		}
	}
	summary.ContentSHA256 = hex.EncodeToString(contentHash.Sum(nil))
	return summary, nil
}

func trimLineEnding(raw []byte) ([]byte, string) {
	if bytes.HasSuffix(raw, []byte("\r\n")) {
		return raw[:len(raw)-2], "crlf"
	}
	if bytes.HasSuffix(raw, []byte("\n")) {
		return raw[:len(raw)-1], "lf"
	}
	return raw, "none"
}

func parseJSONLine(content []byte, validUTF8 bool) (bool, bool, string) {
	if !validUTF8 || len(content) == 0 || !json.Valid(content) {
		if validUTF8 && len(content) > 0 {
			var value any
			err := json.Unmarshal(content, &value)
			return false, err != nil && strings.Contains(err.Error(), "unexpected end of JSON input"), "<invalid>"
		}
		return false, false, "<invalid>"
	}
	first := byte(0)
	for _, character := range content {
		if character != ' ' && character != '\t' && character != '\r' && character != '\n' {
			first = character
			break
		}
	}
	if first != '{' {
		return true, false, "<non_object>"
	}
	var envelope struct {
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(content, &envelope); err != nil {
		return false, false, "<invalid>"
	}
	if len(envelope.Type) == 0 {
		return true, false, "<missing>"
	}
	var text string
	if json.Unmarshal(envelope.Type, &text) == nil {
		return true, false, text
	}
	return true, false, fmt.Sprintf("%s", envelope.Type)
}
