// 中文说明：解析工作区首次启动的默认源和持久化位置；优先沿用已保存的选择，并隔离不同源的实验实例。
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codex-chronicle.local/internal/workspace"
)

func resolveWorkspaceStartup(ctx context.Context, root, archive, config string) (string, string, error) {
	root, archive, config = strings.TrimSpace(root), strings.TrimSpace(archive), strings.TrimSpace(config)
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", "", err
		}
		root = abs
	}
	if config == "" {
		if archive != "" && archive != ":memory:" {
			config = archive + ".workspaces.json"
		} else {
			directory, err := os.UserConfigDir()
			if err != nil {
				return "", "", fmt.Errorf("无法定位用户配置目录，请显式设置 --workspace-config：%w", err)
			}
			config = filepath.Join(directory, "Codex Chronicle", "workspaces.json")
			if root != "" {
				digest := sha256.Sum256([]byte(root))
				config = filepath.Join(directory, "Codex Chronicle", "workspaces", fmt.Sprintf("%x.json", digest[:8]))
			}
		}
	}
	file, err := workspace.NewFileStore(config)
	if err != nil {
		return "", "", err
	}
	registry, err := file.Load(ctx)
	if err != nil {
		return "", "", err
	}
	if registry.CurrentID != "" {
		for _, definition := range registry.Workspaces {
			if definition.ID == registry.CurrentID {
				if pathIsWithin(definition.Root, file.Path()) {
					return "", "", fmt.Errorf("工作区注册表必须位于扫描根之外")
				}
				return definition.Root, file.Path(), nil
			}
		}
	}
	if root == "" {
		candidates, err := workspace.DiscoverCodexHomes(ctx)
		if err != nil {
			return "", "", err
		}
		for _, candidate := range candidates {
			if candidate.Status == "available" {
				root = candidate.Root
				break
			}
		}
	}
	if root != "" && pathIsWithin(root, file.Path()) {
		return "", "", fmt.Errorf("工作区注册表必须位于扫描根之外")
	}
	return root, file.Path(), nil
}
