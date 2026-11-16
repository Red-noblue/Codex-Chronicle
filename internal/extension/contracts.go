// 中文说明：冻结可扩展模块的最小接口，兼容层、搜索、归档和平台能力彼此隔离。
package extension

import (
	"context"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

// SearchProvider 是可替换的全文/按需搜索后端契约。
type SearchProvider interface {
	Search(ctx context.Context, workspaceRoot, query string, limit int) ([]domain.SearchHit, error)
}

// ArchiveProvider 是本地、GitHub、WebDAV 或 S3 provider 的最小归档契约。
type ArchiveProvider interface {
	Create(ctx context.Context, plan lifecycle.OperationPlan) (ArchiveResult, error)
	Verify(ctx context.Context, archiveID string) error
	Delete(ctx context.Context, archiveID string) error
}

// ArchiveResult 是 provider 不依赖具体存储类型的结果摘要。
type ArchiveResult struct {
	ArchiveID string `json:"archive_id"`
	Location  string `json:"location"`
	Verified  bool   `json:"verified"`
}

// PlatformProvider 抽象回收站、锁、路径和能力探针。
type PlatformProvider interface {
	DetectTrash(ctx context.Context) (TrashCapability, error)
	MoveToTrash(ctx context.Context, operationID, root, relativePath string) (TrashReceipt, error)
	Restore(ctx context.Context, receipt TrashReceipt, root string) error
}

// TrashCapability 描述当前平台回收站是否可安全使用。
type TrashCapability struct {
	OS        string `json:"os"`
	Path      string `json:"path"`
	Supported bool   `json:"supported"`
	Verified  bool   `json:"verified"`
	Reason    string `json:"reason"`
}

// TrashReceipt 是跨平台回收站操作的最小恢复凭据。
type TrashReceipt struct {
	OperationID  string `json:"operation_id"`
	RelativePath string `json:"relative_path"`
	OriginalPath string `json:"original_path"`
	TrashPath    string `json:"trash_path"`
}

// CompatibilityAdapter 是 Codex 格式适配器的稳定边界。
type CompatibilityAdapter interface {
	Descriptor() compat.Descriptor
	Inspect(ctx context.Context, root string) (domain.InventoryReport, error)
	BuildPlan(ctx context.Context, root string, conversation domain.ConversationSummary, actions []lifecycle.Action) (lifecycle.OperationPlan, error)
}

// ControlPlaneMutation 是第三方版本化 adapter 的统一写入契约；未知版本不得伪造实现。
type ControlPlaneMutation = compat.ControlPlaneMutation

// PluginRequest/PluginResponse 为未来 JSON-RPC 插件进程保留版本化消息形状。
type PluginRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      string         `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type PluginResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      string       `json:"id"`
	Result  any          `json:"result,omitempty"`
	Error   *PluginError `json:"error,omitempty"`
}

type PluginError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}
