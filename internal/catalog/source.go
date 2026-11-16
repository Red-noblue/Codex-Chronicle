// 中文说明：提供 JSONL 文件扫描和受限字节范围读取，所有源文件访问均为只读。
package catalog

import (
	"context"
	"fmt"
	"io"
	"os"

	"codex-chronicle.local/internal/domain"
)

const DefaultMaxReadBytes int64 = 8 << 20

// ScanPath 以只读方式打开 JSONL 文件并交给 framing 扫描器。
func ScanPath(path string, handler FrameHandler) (summary domain.JSONLScanSummary, err error) {
	input, err := os.Open(path)
	if err != nil {
		return summary, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return summary, err
	}
	return ScanReader(io.LimitReader(input, info.Size()), handler)
}

// ScanPathContent 以只读方式扫描文件，并把每行内容交给受限回调。
func ScanPathContent(path string, handler ContentFrameHandler) (summary domain.JSONLScanSummary, err error) {
	input, err := os.Open(path)
	if err != nil {
		return summary, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return summary, err
	}
	return ScanReaderContent(io.LimitReader(input, info.Size()), handler)
}

// ReadRange 读取 [start, start+length) 的源文件字节，最多读取 maxBytes。
// 返回值可能短于 length，此时表示已到达文件末尾。
func ReadRange(ctx context.Context, path string, start, length, maxBytes int64) ([]byte, error) {
	if start < 0 || length < 0 {
		return nil, fmt.Errorf("字节范围不能为负数")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxReadBytes
	}
	if length > maxBytes {
		return nil, fmt.Errorf("字节范围超过读取上限 %d", maxBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if start >= info.Size() {
		return []byte{}, nil
	}
	if available := info.Size() - start; length > available {
		length = available
	}
	if _, err := input.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("定位源文件范围失败: %w", err)
	}
	buffer := make([]byte, length)
	read, readErr := io.ReadFull(input, buffer)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("读取源文件范围失败: %w", readErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return buffer[:read], nil
}
