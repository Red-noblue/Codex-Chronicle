// 中文说明：定义可分页展示的 rollout 事件投影，正文预览和原始字节范围彼此分离。
package domain

import "time"

// ConversationEvent 是 UI 阅读器使用的单条事件窗口，不代表可独立删除的资产。
type ConversationEvent struct {
	ID            string               `json:"id"`
	Role          string               `json:"role,omitempty"`
	Kind          string               `json:"kind"`
	TopLevelType  string               `json:"top_level_type"`
	PayloadType   string               `json:"payload_type,omitempty"`
	Text          string               `json:"text"`
	Truncated     bool                 `json:"truncated"`
	Line          uint64               `json:"line"`
	StartOffset   int64                `json:"start_offset"`
	EndOffset     int64                `json:"end_offset"`
	Timestamp     time.Time            `json:"timestamp,omitempty"`
	RelativePath  string               `json:"relative_path,omitempty"`
	Anchor        string               `json:"anchor,omitempty"`
	SourceVersion string               `json:"source_version,omitempty"`
	TurnID        string               `json:"turn_id,omitempty"`
	TurnInferred  bool                 `json:"turn_inferred,omitempty"`
	ToolName      string               `json:"tool_name,omitempty"`
	CallID        string               `json:"call_id,omitempty"`
	ItemID        string               `json:"item_id,omitempty"`
	Status        string               `json:"status,omitempty"`
	ExitCode      *int                 `json:"exit_code,omitempty"`
	Tool          *ToolPresentation    `json:"tool,omitempty"`
	Changes       []RecordedFileChange `json:"changes,omitempty"`
}

// EventReference 仅指向已验证历史中的原始记录，不读取记录内提及的项目文件。
type EventReference struct {
	Path  string `json:"path"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Line  uint64 `json:"line"`
}

type ToolPresentation struct {
	Kind             string          `json:"kind"`
	Title            string          `json:"title"`
	Target           string          `json:"target,omitempty"`
	WorkingDirectory string          `json:"cwd,omitempty"`
	Output           string          `json:"output,omitempty"`
	OutputTruncated  bool            `json:"output_truncated,omitempty"`
	Result           *EventReference `json:"result,omitempty"`
}

// RecordedFileChange 是 rollout 中记录的操作，不保证等于文件当前状态。
type RecordedFileChange struct {
	Path      string `json:"path"`
	MovePath  string `json:"move_path,omitempty"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Diff      string `json:"diff,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}
