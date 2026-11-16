// 中文说明：按 Codex 文件用途区分完整对话、全局输入历史和普通 JSONL；辅助目录只保留物理资产，不推导会话。
package compat

import (
	"path"
	"strings"

	"codex-chronicle.local/internal/domain"
)

// IsCodexSupportPath 只匹配扫描根下的辅助目录，不按任意层级的 fixtures 名称排除用户对话。
func IsCodexSupportPath(relative string) bool {
	normalized := strings.ToLower(path.Clean(strings.ReplaceAll(relative, "\\", "/")))
	first, _, _ := strings.Cut(normalized, "/")
	switch first {
	case ".tmp", "tmp", "temp", ".cache", "cache", "caches", "log", "logs", "plugins", "skills", "vendor_imports", "shell_snapshots":
		return true
	}
	return false
}

// JSONLFileKind 必须有 rollout 元数据或预期会话路径才视为对话；仅有 .jsonl 扩展名、usage 或 session_id 不够。
// 官方依据：openai/codex rust-v0.167.2 的 message-history/src/lib.rs 与 rollout/src/{lib,recorder,session_index}.rs。
func JSONLFileKind(relative string, summary domain.JSONLScanSummary) domain.AssetKind {
	normalized := strings.ToLower(path.Clean(strings.ReplaceAll(relative, "\\", "/")))
	if IsCodexSupportPath(normalized) {
		return domain.AssetKindGenericJSONL
	}
	base := path.Base(normalized)
	switch base {
	case "history.jsonl":
		return domain.AssetKindInputHistory
	case "session_index.jsonl", "session-index.jsonl":
		return domain.AssetKindSessionIndex
	}
	first, _, _ := strings.Cut(normalized, "/")
	if summary.TopLevelTypes["session_meta"] > 0 || first == "sessions" || first == "archived_sessions" || base == "rollout.jsonl" || strings.HasPrefix(base, "rollout-") {
		return domain.AssetKindJSONL
	}
	return domain.AssetKindGenericJSONL
}
