//go:build !darwin && !windows && !linux

// 中文说明：其他平台暂不提供菜单栏登录启动配置，保留现有浏览器启动方式。
package desktop

import "fmt"

func AutostartEnabled(executable, dataDir, packageRoot string) (bool, error) {
	return false, nil
}

func SetAutostart(executable, dataDir, packageRoot string, enabled bool) error {
	return fmt.Errorf("此平台尚未提供登录启动设置")
}
