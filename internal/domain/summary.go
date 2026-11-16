// 中文说明：定义对话摘要的派生结果契约；摘要不属于源 .codex，也不作为长期知识索引保存。
package domain

import "time"

// SummaryEvidence 是摘要使用的证据类型和边界说明，不保存额外正文副本。
type SummaryEvidence struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// ConversationSummaryResult 是一次摘要生成结果，来源和限制必须随结果返回。
type ConversationSummaryResult struct {
	ConversationID string            `json:"conversation_id"`
	ProviderID     string            `json:"provider_id"`
	Text           string            `json:"text"`
	GeneratedAt    time.Time         `json:"generated_at"`
	Confidence     string            `json:"confidence"`
	Evidence       []SummaryEvidence `json:"evidence,omitempty"`
	Limitations    []string          `json:"limitations,omitempty"`
	Redacted       bool              `json:"redacted"`
	RedactionCount int               `json:"redaction_count,omitempty"`
}
