// 中文说明：在 daemon 上可选托管已构建的 Svelte 工作台；API 路径继续交给原 REST handler。
package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// WithWebRoot 将普通静态目录包装到 API handler 外层，并为前端路由回退到 index.html。
// root 必须是独立的普通目录；空路径表示不启用托管。
func WithWebRoot(next http.Handler, root string) (http.Handler, error) {
	if next == nil {
		return nil, errors.New("Web handler 需要底层 API handler")
	}
	if strings.TrimSpace(root) == "" {
		return next, nil
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("Web 根目录必须是普通目录")
	}
	indexPath := filepath.Join(abs, "index.html")
	indexInfo, err := os.Lstat(indexPath)
	if err != nil {
		return nil, err
	}
	if indexInfo.Mode()&os.ModeSymlink != 0 || !indexInfo.Mode().IsRegular() {
		return nil, errors.New("Web 根目录的 index.html 必须是普通文件")
	}
	files := http.FileServer(http.Dir(abs))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") || request.URL.Path == "/healthz" {
			next.ServeHTTP(writer, request)
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			next.ServeHTTP(writer, request)
			return
		}
		relative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(request.URL.Path)), "/")
		candidate := filepath.Join(abs, filepath.FromSlash(relative))
		candidateInfo, statErr := os.Stat(candidate)
		if statErr == nil && candidateInfo.Mode().IsRegular() {
			files.ServeHTTP(writer, request)
			return
		}
		http.ServeFile(writer, request, indexPath)
	}), nil
}
