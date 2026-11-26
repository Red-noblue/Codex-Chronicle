// 中文说明：验证搜索的文字匹配、原始定位与活动文件的结果发布边界。
package application

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchJSONLReturnsByteRangeAndConversation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"search-thread\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"第一行\\n第二行\"}]}}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := SearchJSONL(context.Background(), root, "第一行\n第二行", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Line != 2 || hits[0].StartOffset <= 0 || hits[0].EndOffset <= hits[0].StartOffset {
		t.Fatalf("unexpected search hits: %+v", hits)
	}
	if hits[0].ConversationID == "" || hits[0].Snippet == "" {
		t.Fatalf("search hit lacks evidence: %+v", hits[0])
	}
}

func TestSearchJSONLFilesUsesDiscoveredPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "first.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"first\"}}\n{\"type\":\"message\",\"text\":\"目标关键词\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.jsonl"), []byte("{\"text\":\"目标关键词\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := SearchJSONLFiles(context.Background(), root, []string{"first.jsonl"}, "目标关键词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].RelativePath != "first.jsonl" || hits[0].ConversationID == "" {
		t.Fatalf("按已发现路径搜索结果错误: %+v", hits)
	}
}

func TestSearchJSONLFilesSkipsPendingTailAndReportsLiveBoundary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "active.jsonl")
	content := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"active-thread\"}}\n" +
		"{\"type\":\"message\",\"text\":\"已完成关键词\"}\n" +
		"{\"type\":\"message\",\"text\":\"未完成关键词")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	hits, err := SearchJSONLFiles(context.Background(), root, []string{"active.jsonl"}, "关键词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Line != 2 || hits[0].SourceStatus != "active_writing" || hits[0].ActivityStatus != "pending_jsonl_line" || hits[0].LiveConsistency != "active_pending_line" || !hits[0].ReadOnly {
		t.Fatalf("pending tail leaked into search or live evidence missing: %+v", hits)
	}
}

func TestSearchReportsProgressBeforePublishingVerifiedFileHits(t *testing.T) {
	const name = "rollout-stream.jsonl"
	const header = "{\"type\":\"session_meta\",\"payload\":{\"id\":\"streaming-search\"}}\n"
	const hit = "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"搜索进度样本\"}}\n"
	const filler = "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"填充文字\"}}\n"
	for _, nameCase := range []string{"稳定", "扫描中截断"} {
		t.Run(nameCase, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, name)
			if err := os.WriteFile(path, []byte(header+hit+strings.Repeat(filler, 2500)), 0600); err != nil {
				t.Fatal(err)
			}
			var records int64
			published := 0
			hits, err := SearchJSONLFilesObserved(t.Context(), root, []string{name}, "搜索进度样本", 4, false, nil, func(progress SearchProgress) error {
				if progress.FilesDone == 0 {
					if progress.RecordsDone <= records || len(progress.Hits) != 0 {
						t.Fatalf("校验前只应报告新增进度：%+v", progress)
					}
					if records == 0 && nameCase == "扫描中截断" {
						if err := os.Truncate(path, int64(len(header))); err != nil {
							t.Fatal(err)
						}
					}
					records = progress.RecordsDone
				} else {
					published += len(progress.Hits)
				}
				return nil
			})
			if records == 0 {
				t.Fatal("未报告文件内进度")
			}
			if nameCase == "扫描中截断" {
				if err == nil || len(hits) != 0 || published != 0 {
					t.Fatalf("截断后不应发布原先的命中：hits=%v published=%d err=%v", hits, published, err)
				}
			} else if err != nil || records < 2000 || len(hits) != 1 || hits[0].Line != 2 || published != 1 {
				t.Fatalf("应在文件通过校验后发布命中：records=%d hits=%v published=%d err=%v", records, hits, published, err)
			}
		})
	}
}
