// 中文说明：定义 Codex Chronicle 的只读资产、状态和 JSONL framing 数据契约。
package domain

import "time"

// AssetStatus 表示资产当前是否满足继续分析或写入预检的状态。
type AssetStatus string

const (
	StatusStable                   AssetStatus = "stable"
	StatusActiveWriting            AssetStatus = "active_writing"
	StatusGrowing                  AssetStatus = "growing"
	StatusPendingJSONLLine         AssetStatus = "pending_jsonl_line"
	StatusOpenByProcess            AssetStatus = "open_by_process"
	StatusWriterLockObserved       AssetStatus = "writer_lock_observed"
	StatusStaleLock                AssetStatus = "stale_lock"
	StatusSQLiteWALPresent         AssetStatus = "sqlite_wal_present"
	StatusSourceChangedDuringProbe AssetStatus = "source_changed_during_probe"
	StatusBlockedForMutation       AssetStatus = "blocked_for_mutation"
	StatusUnknown                  AssetStatus = "unknown"
	StatusConflict                 AssetStatus = "conflict"
	StatusUnsupported              AssetStatus = "unsupported"
)

// ProcessHandle 是只读活动探测发现的打开文件进程摘要，不保存命令行参数或正文。
type ProcessHandle struct {
	PID     int    `json:"pid"`
	Command string `json:"command,omitempty"`
}

// ActivityEvidence 记录一次短时间窗口内的文件、进程、锁和 SQLite 观测证据。
// 探测器无法确认的能力必须显式记录，不能把缺失探测误报为安全。
type ActivityEvidence struct {
	ProbedAt                 time.Time             `json:"probed_at"`
	IntervalMS               int                   `json:"interval_ms"`
	WindowIndex              int                   `json:"window_index,omitempty"`
	WindowCount              int                   `json:"window_count,omitempty"`
	BeforeSizeBytes          int64                 `json:"before_size_bytes"`
	AfterSizeBytes           int64                 `json:"after_size_bytes"`
	BeforeModTime            time.Time             `json:"before_mod_time,omitempty"`
	AfterModTime             time.Time             `json:"after_mod_time,omitempty"`
	ChangedDuringProbe       bool                  `json:"changed_during_probe"`
	SourceChangedDuringProbe bool                  `json:"source_changed_during_probe,omitempty"`
	SourceIdentityBefore     string                `json:"source_identity_before,omitempty"`
	SourceIdentityAfter      string                `json:"source_identity_after,omitempty"`
	SourceHashBefore         string                `json:"source_hash_before,omitempty"`
	SourceHashAfter          string                `json:"source_hash_after,omitempty"`
	SourceHashAlgorithm      string                `json:"source_hash_algorithm,omitempty"`
	SourceHashCompared       bool                  `json:"source_hash_compared,omitempty"`
	JSONLPendingLine         bool                  `json:"jsonl_pending_line,omitempty"`
	SQLiteWALPresent         bool                  `json:"sqlite_wal_present,omitempty"`
	SQLiteSHMPresent         bool                  `json:"sqlite_shm_present,omitempty"`
	SQLiteAuxiliary          []string              `json:"sqlite_auxiliary,omitempty"`
	OpenByProcess            []ProcessHandle       `json:"open_by_process,omitempty"`
	MatchingLockIDs          []string              `json:"matching_lock_ids,omitempty"`
	LockAgeSeconds           int64                 `json:"lock_age_seconds,omitempty"`
	LockProbeAvailable       bool                  `json:"lock_probe_available"`
	LockProbeReason          string                `json:"lock_probe_reason,omitempty"`
	SQLiteBusy               bool                  `json:"sqlite_busy,omitempty"`
	ProcessProbeAvailable    bool                  `json:"process_probe_available"`
	ProcessProbeReason       string                `json:"process_probe_reason,omitempty"`
	SQLiteProbeReason        string                `json:"sqlite_probe_reason,omitempty"`
	ObservedStates           []AssetStatus         `json:"observed_states,omitempty"`
	MutationBlocked          bool                  `json:"mutation_blocked"`
	Readable                 bool                  `json:"readable"`
	LifecycleBlocked         bool                  `json:"lifecycle_blocked"`
	History                  []ActivityObservation `json:"history,omitempty"`
	Reason                   string                `json:"reason,omitempty"`
}

// ActivityObservation 是一个连续探测窗口中的轻量历史记录，不包含正文内容。
type ActivityObservation struct {
	ProbedAt           time.Time     `json:"probed_at"`
	Status             AssetStatus   `json:"status"`
	ObservedStates     []AssetStatus `json:"observed_states,omitempty"`
	Changed            bool          `json:"changed"`
	SourceHashCompared bool          `json:"source_hash_compared,omitempty"`
	Reason             string        `json:"reason,omitempty"`
}

// AssetKind 表示物理资产的粗粒度类型；详细 schema 由兼容层另行声明。
type AssetKind string

const (
	AssetKindJSONL        AssetKind = "rollout_jsonl"
	AssetKindGenericJSONL AssetKind = "jsonl"
	AssetKindInputHistory AssetKind = "input_history_jsonl"
	AssetKindSessionIndex AssetKind = "session_index_jsonl"
	AssetKindSQLite       AssetKind = "sqlite"
	AssetKindUnknown      AssetKind = "unknown"
)

// DiagnosticSeverity 表示诊断消息对后续操作的阻断程度。
type DiagnosticSeverity string

const (
	SeverityInfo    DiagnosticSeverity = "info"
	SeverityWarning DiagnosticSeverity = "warning"
	SeverityError   DiagnosticSeverity = "error"
	SeverityBlock   DiagnosticSeverity = "block"
)

// Diagnostic 是可展示给用户的结构化证据，不保存正文内容。
type Diagnostic struct {
	Code     string             `json:"code"`
	Severity DiagnosticSeverity `json:"severity"`
	Message  string             `json:"message"`
	Line     uint64             `json:"line,omitempty"`
	Evidence string             `json:"evidence,omitempty"`
	ReadOnly bool               `json:"read_only"`
}

// TokenUsage 保存 rollout 中能够安全识别的累计 token 统计。
type TokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// ThreadOrigin 保存明确的线程来源；session_id 本身不构成父子关系。
type ThreadOrigin struct {
	OriginConflict bool   `json:"origin_conflict,omitempty"`
	AutomationKind string `json:"automation_kind,omitempty"`
	ThreadKind     string `json:"thread_kind,omitempty"`
	ParentThreadID string `json:"parent_thread_id,omitempty"`
	AgentPath      string `json:"agent_path,omitempty"`
	AgentNickname  string `json:"agent_nickname,omitempty"`
	ForkedFromID   string `json:"forked_from_id,omitempty"`
}

// RolloutSummary 是单个 JSONL rollout 的轻量投影；完整正文仍通过字节范围读取。
type RolloutSummary struct {
	ThreadOrigin
	HistoryEdited     bool                 `json:"history_edited,omitempty"`
	ConversationID    string               `json:"conversation_id"`
	SourceID          string               `json:"source_id,omitempty"`
	SessionID         string               `json:"session_id,omitempty"`
	ThreadID          string               `json:"thread_id,omitempty"`
	Title             string               `json:"title,omitempty"`
	FirstUserText     string               `json:"first_user_text,omitempty"`
	CWD               string               `json:"cwd,omitempty"`
	Model             string               `json:"model,omitempty"`
	ModelProvider     string               `json:"model_provider,omitempty"`
	CLIVersion        string               `json:"cli_version,omitempty"`
	StartedAt         time.Time            `json:"started_at,omitempty"`
	UpdatedAt         time.Time            `json:"updated_at,omitempty"`
	EventCount        int                  `json:"event_count"`
	UserMessages      int                  `json:"user_messages"`
	AssistantMessages int                  `json:"assistant_messages"`
	TurnCount         int                  `json:"turn_count"`
	TokenUsage        TokenUsage           `json:"token_usage"`
	Status            AssetStatus          `json:"status"`
	Diagnostics       []Diagnostic         `json:"diagnostics,omitempty"`
	Relationships     []SQLiteRelationship `json:"relationships,omitempty"`
	Orphan            bool                 `json:"orphan"`
}

// ConversationSummary 是 UI 中展示的逻辑对话；它可以关联多个物理 rollout。
type ConversationSummary struct {
	ThreadOrigin
	ParentConversationID string                  `json:"parent_conversation_id,omitempty"`
	ChildConversationIDs []string                `json:"child_conversation_ids,omitempty"`
	DescendantCount      int                     `json:"descendant_count,omitempty"`
	ParentMissing        bool                    `json:"parent_missing,omitempty"`
	FamilyConflict       bool                    `json:"family_conflict,omitempty"`
	ID                   string                  `json:"id"`
	SourceID             string                  `json:"source_id,omitempty"`
	Title                string                  `json:"title,omitempty"`
	FirstUserText        string                  `json:"first_user_text,omitempty"`
	CWD                  string                  `json:"cwd,omitempty"`
	Model                string                  `json:"model,omitempty"`
	ModelProvider        string                  `json:"model_provider,omitempty"`
	CLIVersion           string                  `json:"cli_version,omitempty"`
	StartedAt            time.Time               `json:"started_at,omitempty"`
	UpdatedAt            time.Time               `json:"updated_at,omitempty"`
	RolloutPaths         []string                `json:"rollout_paths"`
	RelatedAssetPaths    []string                `json:"related_asset_paths,omitempty"`
	EventCount           int                     `json:"event_count"`
	UserMessages         int                     `json:"user_messages"`
	AssistantMessages    int                     `json:"assistant_messages"`
	TurnCount            int                     `json:"turn_count"`
	TokenUsage           TokenUsage              `json:"token_usage"`
	Status               AssetStatus             `json:"status"`
	Diagnostics          []Diagnostic            `json:"diagnostics,omitempty"`
	Relationships        []SQLiteRelationship    `json:"relationships,omitempty"`
	Orphan               bool                    `json:"orphan"`
	AssetGraph           *ConversationAssetGraph `json:"asset_graph,omitempty"`
	AssetSetHash         string                  `json:"asset_set_hash,omitempty"`
}

// SearchHit 定位一个原始 JSONL 行，供 UI 按需读取完整内容。
type SearchMatchOrigin string

const (
	SearchMatchIndex  SearchMatchOrigin = "index"
	SearchMatchSource SearchMatchOrigin = "source"
)

type SearchHit struct {
	ConversationID  string            `json:"conversation_id,omitempty"`
	RelativePath    string            `json:"relative_path"`
	Line            uint64            `json:"line"`
	StartOffset     int64             `json:"start"`
	EndOffset       int64             `json:"end"`
	Snippet         string            `json:"snippet"`
	TopLevelType    string            `json:"top_type"`
	MatchOrigin     SearchMatchOrigin `json:"match_origin,omitempty"`
	SourceStatus    AssetStatus       `json:"source_status,omitempty"`
	ActivityStatus  AssetStatus       `json:"activity_status,omitempty"`
	LiveConsistency string            `json:"live_consistency,omitempty"`
	ReadOnly        bool              `json:"read_only"`
}

// SQLiteTableSummary 是 SQLite 表结构的只读摘要，不包含正文行。
type SQLiteTableSummary struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

// SQLiteSchemaObjectSummary 是不含 SQL 正文的索引、触发器或视图结构摘要。
type SQLiteSchemaObjectSummary struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Table     string `json:"table,omitempty"`
	SQLSHA256 string `json:"sql_sha256,omitempty"`
}

// ThreadRecord 是兼容层从 SQLite threads 表提取的元数据投影。
type ThreadRecord struct {
	ThreadOrigin
	ID               string    `json:"id"`
	RolloutPath      string    `json:"rollout_path,omitempty"`
	Title            string    `json:"title,omitempty"`
	FirstUserMessage string    `json:"first_user_message,omitempty"`
	CWD              string    `json:"cwd,omitempty"`
	Model            string    `json:"model,omitempty"`
	ModelProvider    string    `json:"model_provider,omitempty"`
	CLIVersion       string    `json:"cli_version,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	TokensUsed       int64     `json:"tokens_used,omitempty"`
	Archived         bool      `json:"archived"`
}

// SQLiteRelationship 描述一个线程在 SQLite 中的关联证据，不保存关联正文。
type SQLiteRelationship struct {
	Table            string               `json:"table"`
	Column           string               `json:"column"`
	ThreadID         string               `json:"thread_id"`
	RelatedThreadID  string               `json:"related_thread_id,omitempty"`
	RelatedThreadIDs []string             `json:"related_thread_ids,omitempty"`
	Relation         string               `json:"relation"`
	RowCount         int64                `json:"row_count"`
	RowKeys          []SQLiteRowReference `json:"row_keys,omitempty"`
	RowsComplete     bool                 `json:"rows_complete"`
}

// SQLiteRowReference 是只读数据库行的稳定定位摘要；不保存行正文或敏感列内容。
type SQLiteRowReference struct {
	Key     string `json:"key"`
	KeyKind string `json:"key_kind"`
}

// GraphNodeKind 是 canonical conversation asset graph 的节点类型。
type GraphNodeKind string

const (
	GraphNodeConversation   GraphNodeKind = "conversation"
	GraphNodeThread         GraphNodeKind = "thread"
	GraphNodeRollout        GraphNodeKind = "rollout"
	GraphNodeFile           GraphNodeKind = "file"
	GraphNodeSQLiteRelation GraphNodeKind = "sqlite_relation"
	GraphNodeSQLiteRow      GraphNodeKind = "sqlite_row"
	GraphNodeIndexEntry     GraphNodeKind = "index_entry"
)

// AssetGraphNode 是 UI、删除预览和归档计划共享的物理/逻辑资产节点。
type AssetGraphNode struct {
	ID           string        `json:"id"`
	Kind         GraphNodeKind `json:"kind"`
	Label        string        `json:"label,omitempty"`
	RelativePath string        `json:"relative_path,omitempty"`
	AssetKind    AssetKind     `json:"asset_kind,omitempty"`
	ThreadID     string        `json:"thread_id,omitempty"`
	Table        string        `json:"table,omitempty"`
	Column       string        `json:"column,omitempty"`
	RowKey       string        `json:"row_key,omitempty"`
	RowKeyKind   string        `json:"row_key_kind,omitempty"`
	Line         uint64        `json:"line,omitempty"`
	Status       AssetStatus   `json:"status"`
	SizeBytes    int64         `json:"size_bytes,omitempty"`
	ModTime      time.Time     `json:"mod_time,omitempty"`
	Source       string        `json:"source,omitempty"`
}

// AssetGraphEdge 是带证据和置信度的资产关系；推断关系不能默认为可写。
type AssetGraphEdge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Relation   string `json:"relation"`
	Evidence   string `json:"evidence"`
	Confidence string `json:"confidence"`
}

// ConversationAssetGraph 是一个逻辑对话的 canonical 资产图。
type ConversationAssetGraph struct {
	Version        int              `json:"version"`
	ConversationID string           `json:"conversation_id"`
	Completeness   string           `json:"completeness"`
	Nodes          []AssetGraphNode `json:"nodes"`
	Edges          []AssetGraphEdge `json:"edges"`
	Diagnostics    []Diagnostic     `json:"diagnostics,omitempty"`
	Hash           string           `json:"hash"`
}

// SQLiteSummary 是 SQLite 控制面和 Codex 数据库的安全结构摘要。
type SQLiteSummary struct {
	UserVersion       int                         `json:"user_version"`
	Tables            []SQLiteTableSummary        `json:"tables"`
	SchemaObjects     []SQLiteSchemaObjectSummary `json:"schema_objects,omitempty"`
	SchemaFingerprint string                      `json:"schema_fingerprint"`
	ReadOnly          bool                        `json:"read_only"`
	Diagnostic        *Diagnostic                 `json:"diagnostic,omitempty"`
	Threads           []ThreadRecord              `json:"threads,omitempty"`
	Relationships     []SQLiteRelationship        `json:"relationships,omitempty"`
}

// Asset 是一个可定位的物理文件资产，不代表完整 conversation。
type Asset struct {
	RelativePath string            `json:"relative_path"`
	Kind         AssetKind         `json:"kind"`
	SizeBytes    int64             `json:"size_bytes"`
	ModTime      time.Time         `json:"mod_time"`
	Status       AssetStatus       `json:"status"`
	Error        string            `json:"error,omitempty"`
	Activity     *ActivityEvidence `json:"activity,omitempty"`
}

// JSONLFrame 是一条 JSONL 记录的字节范围和解析结果，不保存正文。
type JSONLFrame struct {
	LineNumber       uint64 `json:"line"`
	StartOffset      int64  `json:"start"`
	EndOffset        int64  `json:"end"`
	ContentEndOffset int64  `json:"content_end"`
	LineEnding       string `json:"line_ending"`
	ValidUTF8        bool   `json:"valid_utf8"`
	ValidJSON        bool   `json:"valid_json"`
	Pending          bool   `json:"pending"`
	TopLevelType     string `json:"top_type"`
	SHA256           string `json:"sha256"`
}

// JSONLScanSummary 是单个 JSONL 文件的聚合结果。
type JSONLScanSummary struct {
	LineCount        uint64            `json:"line_count"`
	InvalidJSONLines uint64            `json:"invalid_json_lines"`
	InvalidUTF8Lines uint64            `json:"invalid_utf8_lines"`
	PendingLines     uint64            `json:"pending_lines"`
	MaxLineBytes     int64             `json:"max_line_bytes"`
	TopLevelTypes    map[string]uint64 `json:"top_level_types"`
	ContentSHA256    string            `json:"content_sha256,omitempty"`
}

// SessionIndexEntry 是 session index 中可与 thread/session 关联的最小记录。
type SessionIndexEntry struct {
	ID   string `json:"id"`
	Line uint64 `json:"line"`
}

// SessionIndexSummary 是 session index 的只读结构投影。
type SessionIndexSummary struct {
	LineCount        uint64              `json:"line_count"`
	InvalidJSONLines uint64              `json:"invalid_json_lines"`
	Entries          []SessionIndexEntry `json:"entries"`
}

// FileInventory 是一个物理文件及其只读解析结果。
type FileInventory struct {
	Asset
	ChangedDuringScan bool                 `json:"changed_during_scan"`
	JSONL             *JSONLScanSummary    `json:"jsonl,omitempty"`
	Rollout           *RolloutSummary      `json:"rollout,omitempty"`
	SessionIndex      *SessionIndexSummary `json:"session_index,omitempty"`
	SQLite            *SQLiteSummary       `json:"sqlite,omitempty"`
}

// InventoryReport 是一次只读扫描的结果。
type InventoryReport struct {
	SchemaVersion int                   `json:"schema_version"`
	Root          string                `json:"root"`
	ReadOnly      bool                  `json:"read_only"`
	GeneratedAt   time.Time             `json:"generated_at"`
	Files         []FileInventory       `json:"files"`
	Conversations []ConversationSummary `json:"conversations,omitempty"`
	Errors        []string              `json:"errors,omitempty"`
}

// TaskStatus 表示后台扫描任务生命周期。
type TaskStatus string

const (
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)
