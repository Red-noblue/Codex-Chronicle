// 中文说明：只读列出本项目 ArchiveRoot 内的本地归档 manifest；不扫描 payload 正文，也不修改任何文件。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/archive"
)

const maxLocalArchiveManifestBytes int64 = 16 << 20

type localArchiveCatalogItem struct {
	Path                 string                       `json:"path"`
	Name                 string                       `json:"name"`
	OperationID          string                       `json:"operation_id"`
	ConversationID       string                       `json:"conversation_id,omitempty"`
	CreatedAt            time.Time                    `json:"created_at"`
	Encrypted            bool                         `json:"encrypted"`
	AssetCount           int                          `json:"asset_count"`
	ControlPlaneSnapshot bool                         `json:"control_plane_snapshot"`
	DeclaredPayloadBytes int64                        `json:"declared_payload_bytes"`
	ManifestBytes        int64                        `json:"manifest_bytes"`
	Staging              bool                         `json:"staging"`
	ManifestSHA256       string                       `json:"manifest_sha256"`
	Compression          *archive.CloudBackupMetadata `json:"compression,omitempty"`
}

// localArchiveCatalogHandler 只读取独立 ArchiveRoot 的 manifest，供恢复 UI 选择归档。
func (s *Server) localArchiveCatalogHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	if s == nil || s.lifecycle == nil || strings.TrimSpace(s.lifecycle.ArchiveRoot) == "" {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": false, "archive_root": "", "items": []localArchiveCatalogItem{}})
		return
	}
	root, err := filepath.Abs(s.lifecycle.ArchiveRoot)
	if err != nil {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": false, "archive_root": "", "items": []localArchiveCatalogItem{}, "errors": []string{fmt.Sprintf("解析 ArchiveRoot 失败: %v", err)}})
		return
	}
	root = filepath.Clean(root)
	if pathHasCodexComponent(root) {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": false, "archive_root": root, "items": []localArchiveCatalogItem{}, "errors": []string{"ArchiveRoot 不能位于 .codex 路径"}})
		return
	}
	info, statErr := os.Lstat(root)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": true, "archive_root": root, "items": []localArchiveCatalogItem{}})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": false, "archive_root": root, "items": []localArchiveCatalogItem{}, "errors": []string{fmt.Sprintf("读取 ArchiveRoot 失败: %v", statErr)}})
		return
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": false, "archive_root": root, "items": []localArchiveCatalogItem{}, "errors": []string{"ArchiveRoot 必须是普通目录"}})
		return
	}
	items := make([]localArchiveCatalogItem, 0)
	errors := make([]string, 0)
	if err := scanLocalArchiveCatalog(request.Context(), root, false, &items, &errors); err != nil {
		errors = append(errors, err.Error())
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].Path < items[j].Path
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	writeJSON(writer, http.StatusOK, map[string]any{"read_only": true, "available": true, "archive_root": root, "items": items, "errors": errors})
}

func scanLocalArchiveCatalog(ctx context.Context, root string, staging bool, items *[]localArchiveCatalogItem, errors *[]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("读取归档目录 %s 失败: %w", root, err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(root, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			*errors = append(*errors, fmt.Sprintf("跳过归档目录 %s: %v", path, statErr))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			*errors = append(*errors, fmt.Sprintf("跳过符号链接归档目录 %s", path))
			continue
		}
		if !info.IsDir() {
			continue
		}
		manifestPath := filepath.Join(path, "manifest.json")
		manifestInfo, manifestErr := os.Lstat(manifestPath)
		if manifestErr == nil {
			if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
				*errors = append(*errors, fmt.Sprintf("跳过不安全 manifest %s", manifestPath))
				continue
			}
			if manifestInfo.Size() > maxLocalArchiveManifestBytes {
				*errors = append(*errors, fmt.Sprintf("跳过过大的归档 manifest %s（上限 %d 字节）", manifestPath, maxLocalArchiveManifestBytes))
				continue
			}
			item, parseErr := readLocalArchiveCatalogItem(path, manifestPath, staging, manifestInfo.Size())
			if parseErr != nil {
				*errors = append(*errors, parseErr.Error())
				continue
			}
			*items = append(*items, item)
			continue
		}
		if !os.IsNotExist(manifestErr) {
			*errors = append(*errors, fmt.Sprintf("检查归档 manifest %s 失败: %v", manifestPath, manifestErr))
			continue
		}
		// 远端恢复 preview 的归档位于 .remote-restore-staging/<restore-id>/<archive-id>；
		// 只递归这个受控目录，不遍历 ArchiveRoot 中的任意用户目录。
		if !staging && entry.Name() == ".remote-restore-staging" {
			if err := scanLocalArchiveCatalog(ctx, path, true, items, errors); err != nil {
				*errors = append(*errors, err.Error())
			}
		} else if staging {
			if err := scanLocalArchiveCatalog(ctx, path, true, items, errors); err != nil {
				*errors = append(*errors, err.Error())
			}
		}
	}
	return nil
}

func readLocalArchiveCatalogItem(directory, manifestPath string, staging bool, manifestBytes int64) (localArchiveCatalogItem, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return localArchiveCatalogItem{}, fmt.Errorf("读取归档 manifest %s 失败: %w", manifestPath, err)
	}
	var manifest archive.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return localArchiveCatalogItem{}, fmt.Errorf("解析归档 manifest %s 失败: %w", manifestPath, err)
	}
	if manifest.Version != 1 || strings.TrimSpace(manifest.OperationID) == "" || len(manifest.Assets) == 0 {
		return localArchiveCatalogItem{}, fmt.Errorf("归档 manifest %s 缺少有效版本、操作 ID 或资产", manifestPath)
	}
	var declared int64
	for _, asset := range manifest.Assets {
		if asset.SizeBytes > 0 {
			declared += asset.SizeBytes
		}
	}
	if manifest.ControlPlaneSnapshot != nil && manifest.ControlPlaneSnapshot.PayloadSizeBytes > 0 {
		declared += manifest.ControlPlaneSnapshot.PayloadSizeBytes
	}
	return localArchiveCatalogItem{
		Path: directory, Name: filepath.Base(directory), OperationID: manifest.OperationID,
		ConversationID: manifest.ConversationID, CreatedAt: manifest.CreatedAt, Encrypted: manifest.Encrypted,
		AssetCount: len(manifest.Assets), ControlPlaneSnapshot: manifest.ControlPlaneSnapshot != nil,
		DeclaredPayloadBytes: declared, ManifestBytes: manifestBytes, Staging: staging,
		ManifestSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Compression: manifest.CloudBackup,
	}, nil
}
