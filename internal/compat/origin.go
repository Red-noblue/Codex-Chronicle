// 中文说明：统一解析线程来源；spawn、普通 fork 和自动记忆任务保持不同语义。
package compat

import (
	"encoding/json"
	"strings"

	"codex-chronicle.local/internal/domain"
)

func threadOrigin(meta map[string]json.RawMessage) domain.ThreadOrigin {
	origin := domain.ThreadOrigin{
		ParentThreadID: rawString(meta["parent_thread_id"]), AgentPath: rawString(meta["agent_path"]),
		AgentNickname: rawString(meta["agent_nickname"]), ForkedFromID: rawString(meta["forked_from_id"]),
	}
	source := rawObject(meta["source"])
	if text := rawString(meta["source"]); text != "" {
		source = rawObject(json.RawMessage(text))
	}
	if sub := rawObject(source["subagent"]); sub != nil {
		origin.ThreadKind = "subagent"
		if spawn := rawObject(sub["thread_spawn"]); spawn != nil {
			origin.ThreadKind = "subagent"
			nestedParent := rawString(spawn["parent_thread_id"])
			origin.OriginConflict = origin.ParentThreadID != "" && nestedParent != "" && origin.ParentThreadID != nestedParent
			origin.ParentThreadID = firstNonEmpty(origin.ParentThreadID, rawString(spawn["parent_thread_id"]))
			origin.AgentPath = firstNonEmpty(origin.AgentPath, rawString(spawn["agent_path"]))
			origin.AgentNickname = firstNonEmpty(origin.AgentNickname, rawString(spawn["agent_nickname"]))
		}
	} else if rawString(source["subagent"]) != "" {
		origin.ThreadKind = "automation"
		origin.AutomationKind = rawString(source["subagent"])
	}
	if origin.ThreadKind == "" && (origin.ParentThreadID != "" || rawString(meta["thread_source"]) == "subagent") {
		origin.ThreadKind = "subagent"
	}
	return origin
}

// IsInjectedContext 只识别 Codex 的明确上下文封套，不把一般用户正文当作规则过滤。
func IsInjectedContext(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "# AGENTS.md instructions for ") || strings.HasPrefix(text, "<environment_context>")
}

// OriginTitle 在没有真实用户提问时为自动线程提供可理解的标题。
func OriginTitle(origin domain.ThreadOrigin) string {
	if origin.ThreadKind == "subagent" {
		name := firstNonEmpty(origin.AgentNickname, origin.AgentPath)
		if name != "" {
			return "子代理 · " + name
		}
		return "子代理任务"
	}
	if origin.ThreadKind == "automation" {
		if origin.AutomationKind == "memory_consolidation" {
			return "自动记忆整理"
		}
		return "自动任务"
	}
	return ""
}
