// 中文说明：保存本项目中间产物管理的启用状态；配置与清理执行权限分离，不触发移动或删除。
package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const cleanupManagementFile = "management.json"

type ManagementSettings struct {
	Version int  `json:"version"`
	Enabled bool `json:"enabled"`
}

// LoadManagementSettings 不创建登记目录；旧 fixture 可由调用方提供默认启用状态。
func (registry *Registry) LoadManagementSettings(defaultEnabled bool) (ManagementSettings, error) {
	settings := ManagementSettings{Version: 1, Enabled: defaultEnabled}
	path := filepath.Join(registry.Root, cleanupManagementFile)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if !info.Mode().IsRegular() {
		return settings, fmt.Errorf("中间产物管理配置必须是普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, fmt.Errorf("读取中间产物管理配置失败：%w", err)
	}
	if settings.Version != 1 {
		return settings, fmt.Errorf("不支持的中间产物管理配置版本：%d", settings.Version)
	}
	return settings, nil
}

func (registry *Registry) SaveManagementSettings(ctx context.Context, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCleanupWriteRoot(registry.Root); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(registry.Root, cleanupManagementFile), ManagementSettings{Version: 1, Enabled: enabled}, false)
}
