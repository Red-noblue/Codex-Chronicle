// 中文说明：通过实际 API 验证整目录预览和批量回收覆盖分页之外的文件，同时保留敏感、非目标和预览后新增的文件。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/maintenance"
)

func TestMaintenanceDirectoryPreviewAndExecutionCoverUnloadedFiles(t *testing.T) {
	root := t.TempDir()
	write := func(relative string) {
		t.Helper()
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("directory-cleanup-fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 64; i++ {
		write(fmt.Sprintf("tmp/nested/file-%02d.bin", i))
	}
	for _, relative := range []string{"tmp/auth.json", "tmp/state.sqlite-wal", "tmp-other/untouched.tmp"} {
		write(relative)
	}
	options := &LifecycleOptions{Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(),
		JournalRoot: filepath.Join(t.TempDir(), "journal"), TrashRoot: filepath.Join(t.TempDir(), "trash"),
		ActivityOptions: &activity.Options{Interval: time.Millisecond, DisableProcessProbe: true, Conservative: false}}
	server := NewServerWithOptions(root, nil, options)
	defer server.Close()
	handler := server.Handler()
	response := apiJSON(t, handler, http.MethodGet, "/api/v1/maintenance?limit=1&directory=tmp", "")
	var report maintenance.Report
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &report) != nil || len(report.Candidates) != 1 || len(report.Directories) != 1 || report.Directories[0].EligibleCount != 64 {
		t.Fatalf("目录统计被分页截断：%s", response.Body.String())
	}
	response = apiJSON(t, handler, http.MethodGet, "/api/v1/maintenance?scope=ungrouped&limit=1", "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &report) != nil || report.UngroupedCount != 1 || report.FilteredCount != 1 || len(report.Candidates) != 1 || report.Candidates[0].RelativePath != "tmp-other/untouched.tmp" {
		t.Fatalf("独立文件入口必须精准筛选，不能重新显示目录内候选：%s", response.Body.String())
	}
	response = apiJSON(t, handler, http.MethodGet, "/api/v1/maintenance?scope=arbitrary", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("未知范围应拒绝：%d", response.Code)
	}
	response = apiJSON(t, handler, http.MethodPost, "/api/v1/maintenance/preview", `{"directories":["tmp"],"paths":["tmp/nested/file-00.bin"]}`)
	var preview struct {
		Plan      maintenance.Plan      `json:"plan"`
		Selection maintenance.Selection `json:"selection"`
		Activity  struct {
			TargetCount int                    `json:"target_count"`
			Results     []activity.AssetResult `json:"results"`
		} `json:"activity"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &preview) != nil || len(preview.Plan.Assets) != 64 || preview.Selection.FileCount != 64 || preview.Selection.RetainedCount != 2 {
		t.Fatalf("目录预览范围错误：%s", response.Body.String())
	}
	if preview.Activity.TargetCount != 64 || len(preview.Activity.Results) != 0 {
		t.Fatalf("稳定目录不应返回每个文件的重复活动证据：%+v", preview.Activity)
	}
	// 选择绑定的是预览中的文件，不会因执行时目录内新增文件而扩大删除范围。
	write("tmp/later.tmp")
	response = apiJSON(t, handler, http.MethodPost, "/api/v1/maintenance/execute", fmt.Sprintf(`{"operation_id":%q}`, preview.Plan.OperationID))
	if response.Code != http.StatusOK {
		t.Fatalf("目录批量回收失败：%s", response.Body.String())
	}
	for i := 0; i < 64; i++ {
		if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("tmp/nested/file-%02d.bin", i))); !os.IsNotExist(err) {
			t.Fatalf("分页以外的目标文件未被回收：%d %v", i, err)
		}
	}
	for _, relative := range []string{"tmp/auth.json", "tmp/state.sqlite-wal", "tmp-other/untouched.tmp", "tmp/later.tmp"} {
		if _, err := os.Stat(filepath.Join(root, relative)); err != nil {
			t.Fatalf("被排除的文件不应发生变化：%s %v", relative, err)
		}
	}
	response = apiJSON(t, handler, http.MethodGet, "/api/v1/maintenance?limit=1", "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &report) != nil || report.Directories[0].EligibleCount != 1 || report.Directories[0].FileCount != 3 {
		t.Fatalf("完成后应立即失效旧目录计数：%s", response.Body.String())
	}
	response = apiJSON(t, handler, http.MethodGet, "/api/v1/maintenance?directory=../tmp", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("越界目录筛选应被拒绝：%d", response.Code)
	}
}
