// 中文说明：定义摘要 provider 边界，使本地摘要、云端模型和用户扩展实现彼此隔离。
package summary

import (
	"context"
	"fmt"
	"strings"

	"codex-chronicle.local/internal/domain"
)

// Request 是 provider 的最小输入；事件正文由应用层按需读取后传入。
type Request struct {
	Conversation domain.ConversationSummary
	Events       []domain.ConversationEvent
	MaxChars     int
}

// Provider 生成一个对话的派生摘要，不应写入源文件或控制面数据库。
type Provider interface {
	ID() string
	Summarize(context.Context, Request) (domain.ConversationSummaryResult, error)
}

// Registry 保存显式启用的摘要 provider。
type Registry struct {
	providers map[string]Provider
}

// NewRegistry 创建摘要 provider 注册表。
func NewRegistry(providers ...Provider) Registry {
	registry := Registry{providers: make(map[string]Provider, len(providers))}
	for _, provider := range providers {
		if provider == nil || strings.TrimSpace(provider.ID()) == "" {
			continue
		}
		registry.providers[provider.ID()] = provider
	}
	return registry
}

// DefaultRegistry 只启用不依赖外部服务的本地确定性摘要。
func DefaultRegistry() Registry {
	return NewRegistry(NewLocalProvider())
}

// Resolve 按 ID 查找 provider；空 ID 使用本地 provider。
func (r Registry) Resolve(id string) (Provider, error) {
	if strings.TrimSpace(id) == "" {
		id = LocalProviderID
	}
	provider, ok := r.providers[id]
	if !ok {
		return nil, fmt.Errorf("摘要 provider 未启用: %s", id)
	}
	return provider, nil
}

// IDs 返回已启用 provider 的稳定排序列表。
func (r Registry) IDs() []string {
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}
