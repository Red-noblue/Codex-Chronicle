// 中文说明：验证子代理的多版本来源、自动任务区分和注入规则的标题隔离。
package compat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpawnTitleSkipsInjectedRules(t *testing.T) {
	data := `{"type":"session_meta","payload":{"id":"child","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","agent_path":"/root/review","agent_nickname":"Fermat"}}}}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"# AGENTS.md instructions for /project\n<INSTRUCTIONS>rules</INSTRUCTIONS>"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"<environment_context>cwd</environment_context>"}]}}
`
	p, _, err := AnalyzeRollout(strings.NewReader(data), "child.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if p.ParentThreadID != "parent" || p.SourceID != "child" || p.ThreadKind != "subagent" || p.Title != "子代理 · Fermat" || p.UserMessages != 0 || p.FirstUserText != "" {
		t.Fatalf("unexpected projection: %+v", p)
	}
	path := filepath.Join(t.TempDir(), "child.jsonl")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	items, _, _, err := ReadRolloutEventsPath(context.Background(), path, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Kind != "injected_context" || items[1].Kind != "injected_context" {
		t.Fatalf("injected rules not separated: %+v", items)
	}
}

func TestOriginDistinguishesForkAndMemory(t *testing.T) {
	for _, tc := range []struct{ input, kind, parent string }{
		{`{"source":{"subagent":"memory_consolidation"}}`, "automation", ""},
		{`{"forked_from_id":"parent","session_id":"parent","source":"cli"}`, "", ""},
		{`{"parent_thread_id":"parent","thread_source":"subagent","agent_path":"/root/a"}`, "subagent", "parent"},
		{`{"source":"{\"subagent\":{\"thread_spawn\":{\"parent_thread_id\":\"parent\"}}}"}`, "subagent", "parent"},
	} {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.input), &meta); err != nil {
			t.Fatal(err)
		}
		o := threadOrigin(meta)
		if o.ThreadKind != tc.kind || o.ParentThreadID != tc.parent {
			t.Fatalf("%s: %+v", tc.input, o)
		}
	}
	if IsInjectedContext("请解释 AGENTS.md instructions 的用法") {
		t.Fatal("normal user text hidden")
	}
}
