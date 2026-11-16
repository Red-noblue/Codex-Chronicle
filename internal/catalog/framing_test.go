// 中文说明：验证 JSONL framing 的长行、换行、非法记录和字节范围边界。
package catalog

import (
	"bytes"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestScanReaderPreservesFramesAndOffsets(t *testing.T) {
	input := []byte("{\"type\":\"session_meta\"}\n{\"type\":\"response_item\"}\r\n{\"type\":\"broken\"\n{\"type\":\"final\"}")
	var frames []domain.JSONLFrame

	summary, err := ScanReader(bytes.NewReader(input), func(frame domain.JSONLFrame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatalf("ScanReader returned error: %v", err)
	}
	if summary.LineCount != 4 || summary.InvalidJSONLines != 1 || summary.PendingLines != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if len(frames) != 4 {
		t.Fatalf("unexpected frame count: %d", len(frames))
	}
	if frames[0].StartOffset != 0 || frames[0].EndOffset != 24 || frames[0].ContentEndOffset != 23 {
		t.Fatalf("unexpected first range: %+v", frames[0])
	}
	if frames[1].LineEnding != "crlf" || frames[1].TopLevelType != "response_item" {
		t.Fatalf("unexpected second frame: %+v", frames[1])
	}
	if frames[2].ValidJSON || frames[2].TopLevelType != "<invalid>" {
		t.Fatalf("unexpected invalid frame: %+v", frames[2])
	}
	if !frames[3].ValidJSON || frames[3].LineEnding != "none" {
		t.Fatalf("unexpected final frame: %+v", frames[3])
	}
}

func TestScanReaderMarksUnterminatedInvalidLinePending(t *testing.T) {
	var frames []domain.JSONLFrame
	input := strings.NewReader("{\"type\":\"complete\"}\n{\"type\":\"")
	summary, err := ScanReader(input, func(frame domain.JSONLFrame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatalf("ScanReader returned error: %v", err)
	}
	if summary.PendingLines != 1 || len(frames) != 2 || !frames[1].Pending {
		t.Fatalf("expected pending final line: summary=%+v frames=%+v", summary, frames)
	}
}

func TestScanReaderSeparatesCompleteInvalidFinalLineFromPending(t *testing.T) {
	var frames []domain.JSONLFrame
	input := strings.NewReader("{not-json")
	summary, err := ScanReader(input, func(frame domain.JSONLFrame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.InvalidJSONLines != 1 || summary.PendingLines != 0 || len(frames) != 1 || frames[0].Pending {
		t.Fatalf("complete invalid line was misclassified: summary=%+v frames=%+v", summary, frames)
	}
}

func TestScanReaderHandlesLongLine(t *testing.T) {
	longValue := strings.Repeat("x", 256*1024)
	input := strings.NewReader("{\"type\":\"large\",\"value\":\"" + longValue + "\"}\n")
	summary, err := ScanReader(input, nil)
	if err != nil {
		t.Fatalf("ScanReader returned error: %v", err)
	}
	if summary.LineCount != 1 || summary.InvalidJSONLines != 0 || summary.MaxLineBytes < 256*1024 {
		t.Fatalf("unexpected long line summary: %+v", summary)
	}
}

func TestScanReaderClassifiesTopLevelTypesWithoutPayloadProjection(t *testing.T) {
	input := strings.NewReader("[]\n{\"payload\":{\"large\":true}}\n{\"type\":17}\n")
	var types []string
	if _, err := ScanReader(input, func(frame domain.JSONLFrame) error {
		types = append(types, frame.TopLevelType)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"<non_object>", "<missing>", "17"}
	if len(types) != len(want) {
		t.Fatalf("unexpected type count: got=%v want=%v", types, want)
	}
	for index := range want {
		if types[index] != want[index] {
			t.Fatalf("unexpected type at %d: got=%q want=%q", index, types[index], want[index])
		}
	}
}
