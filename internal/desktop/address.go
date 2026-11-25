// 中文说明：为安装版保存上次成功绑定的回环地址；地址仅作绑定偏好，不能代替实例身份认证。
package desktop

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

const defaultAddress = "127.0.0.1:8787"

func preferredAddress(dataDir string) string {
	path := filepath.Join(dataDir, "runtime", "last-address.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024 {
		return defaultAddress
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultAddress
	}
	var saved struct {
		Address string `json:"address"`
	}
	if json.Unmarshal(data, &saved) != nil {
		return defaultAddress
	}
	host, port, err := net.SplitHostPort(saved.Address)
	number, numberErr := strconv.Atoi(port)
	if err != nil || numberErr != nil || host != "127.0.0.1" || number < 1 || number > 65535 {
		return defaultAddress
	}
	return saved.Address
}

// RememberAddress 仅在后台已绑定端口并发布实例后调用，退出不会清除此偏好。
func (l *Lease) RememberAddress() error {
	parsed, err := url.Parse(l.Instance.URL)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Address string `json:"address"`
	}{parsed.Host})
	if err != nil {
		return err
	}
	path := filepath.Join(l.dir, "last-address.json")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("地址偏好必须为普通文件")
	}
	file, err := os.CreateTemp(l.dir, ".last-address-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
