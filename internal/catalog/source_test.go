// 中文说明：验证只读字节范围读取的边界、上限、EOF 和取消行为。
package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScanPathAndReadRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	content := []byte("前缀\n{\"type\":\"response_item\"}\n结尾")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := ScanPath(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.LineCount != 3 || summary.InvalidJSONLines != 2 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	start := int64(len([]byte("前缀\n")))
	chunk, err := ReadRange(context.Background(), path, start, int64(len([]byte("{\"type\":\"response_item\"}"))), 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "{\"type\":\"response_item\"}" {
		t.Fatalf("unexpected chunk: %q", chunk)
	}
	short, err := ReadRange(context.Background(), path, int64(len(content)-3), 20, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(short) != "尾" {
		t.Fatalf("unexpected short read: %q", short)
	}
}

func TestReadRangeRejectsInvalidOrOversizedRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		start  int64
		length int64
		max    int64
	}{
		{name: "negative start", start: -1, length: 1, max: 10},
		{name: "negative length", start: 0, length: -1, max: 10},
		{name: "over max", start: 0, length: 11, max: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ReadRange(context.Background(), path, test.start, test.length, test.max); err == nil {
				t.Fatal("expected range error")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadRange(ctx, path, 0, 1, 10); err == nil {
		t.Fatal("expected canceled read")
	}
}
