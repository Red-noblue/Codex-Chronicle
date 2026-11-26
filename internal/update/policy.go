// 中文说明：在线更新在切换及实际重启前重新核验发布策略，支持版本撤回和暂停，并绑定下载时的包身份。
package update

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrOnlineReleaseWithdrawn = errors.New("该更新已撤回或不再由当前渠道提供")
var ErrOnlineUpdatesPaused = errors.New("发布方已暂停在线更新")
var ErrOnlinePolicyUnavailable = errors.New("暂时无法确认最新更新策略")

type WithdrawnVersion struct {
	Version string `json:"version"`
	Reason  string `json:"reason,omitempty"`
}

type OnlineSource struct {
	FeedURL string        `json:"feed_url"`
	Release OnlineRelease `json:"release"`
}

func onlineSourcesEqual(a, b *OnlineSource) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.FeedURL == b.FeedURL && sameOnlineRelease(a.Release, b.Release)
}

func withdrawnReason(feed OnlineFeed, version string) string {
	for _, item := range feed.Withdrawn {
		if item.Version == version {
			if item.Reason != "" {
				return item.Reason
			}
			return "发布方已撤回该版本"
		}
	}
	return ""
}

// AuthorizeOnlineSource 只对在线计划要求新鲜索引；人工选择的外部离线包继续按本地签名策略验证。
func (m *Manager) AuthorizeOnlineSource(ctx context.Context, source *OnlineSource) error {
	if source == nil {
		return nil
	}
	m.mu.Lock()
	config := m.online
	m.mu.Unlock()
	return authorizeOnlineSource(ctx, config, source)
}

func authorizeOnlineSource(ctx context.Context, config OnlineConfig, source *OnlineSource) error {
	if source == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	config.FeedURL = source.FeedURL
	feed, err := readOnlineFeed(ctx, config, "")
	if err != nil {
		return fmt.Errorf("%w：%w", ErrOnlinePolicyUnavailable, err)
	}
	if reason := withdrawnReason(feed, source.Release.Version); reason != "" {
		return fmt.Errorf("%w：%s", ErrOnlineReleaseWithdrawn, reason)
	}
	if feed.UpdatesPaused {
		return fmt.Errorf("%w：%s", ErrOnlineUpdatesPaused, feed.PauseReason)
	}
	for _, release := range feed.Releases {
		if sameOnlineRelease(release, source.Release) {
			return nil
		}
	}
	return ErrOnlineReleaseWithdrawn
}
