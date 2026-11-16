// 中文说明：供发行版执行器调用的原生回收站桥接程序；使用 JSON 标准输入输出，不依赖文档或实验目录。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"codex-chronicle.local/internal/platform"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var request struct {
		Action      string                `json:"action"`
		OperationID string                `json:"operation_id"`
		Root        string                `json:"root"`
		Relative    string                `json:"relative"`
		Receipt     platform.TrashReceipt `json:"receipt"`
	}
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&request); err != nil {
		return err
	}
	provider, err := platform.NewNativeSystemTrash()
	if err != nil {
		return err
	}
	var result any
	switch request.Action {
	case "locate":
		var path string
		path, err = platform.NativeTrashDirectory(request.Root)
		result = map[string]string{"trash_path": path}
	case "move":
		result, err = provider.Move(context.Background(), request.OperationID, request.Root, request.Relative)
	case "restore":
		err = provider.Restore(context.Background(), request.Receipt, request.Root)
		result = map[string]bool{"restored": err == nil}
	default:
		return fmt.Errorf("不支持的回收站动作：%s", request.Action)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
