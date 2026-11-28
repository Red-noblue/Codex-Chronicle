// 中文说明：菜单栏与托盘通过已有实例私有控制通道读取更新状态和暂停下载，凭证不经过网页或代理。
package desktop

import (
	updatepkg "codex-chronicle.local/internal/update"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (i Instance) UpdateStatus(ctx context.Context) (updatepkg.Status, error) {
	response, err := i.request(ctx, http.MethodGet, "/update/status")
	if err != nil {
		return updatepkg.Status{}, err
	}
	defer response.Body.Close()
	var result struct {
		Status updatepkg.Status `json:"status"`
		Error  string           `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return result.Status, err
	}
	if response.StatusCode != 200 {
		return result.Status, fmt.Errorf("无法读取更新状态：%s", result.Error)
	}
	return result.Status, nil
}
func (i Instance) ControlUpdateDownload(ctx context.Context, action string) error {
	body, _ := json.Marshal(map[string]string{"action": action})
	response, err := i.requestBody(ctx, http.MethodPost, "/update/control", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusAccepted {
		return nil
	}
	var result struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&result)
	return fmt.Errorf("下载操作未完成：%s", result.Error)
}
