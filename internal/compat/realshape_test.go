// 中文说明：用项目内脱敏真实形状 fixture 回归只读快照、行定位和 fail-closed 边界。
package compat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const realShapeFixtureThreadID = "00000000-0000-4000-8000-000000000001"

func TestInspectRealShapeFixtureGenerated(t *testing.T) {
	script := realShapeFixtureScript(t)
	root := createRealShapeFixtureForTest(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := InspectRealShapeFixture(ctx, root, realShapeFixtureThreadID)
	if err != nil {
		t.Fatalf("读取脱敏真实形状 fixture 失败: %v", err)
	}
	if !snapshot.ReadOnly || !snapshot.Complete {
		t.Fatalf("fixture 快照必须完整且只读: %+v", snapshot)
	}
	if snapshot.RolloutPath == "" || snapshot.RolloutSHA256 == "" || len(snapshot.SessionIndexLines) != 1 || snapshot.SessionIndexLines[0] != 1 {
		t.Fatalf("fixture rollout/index 证据不完整: %+v", snapshot)
	}
	if len(snapshot.Databases) != len(realShapeDatabases) || len(snapshot.Rows) == 0 {
		t.Fatalf("fixture 数据库或关系行快照不完整: databases=%d rows=%d", len(snapshot.Databases), len(snapshot.Rows))
	}
	if !hasRealShapeRow(snapshot.Rows, "state_5.sqlite", "threads", "id") || !hasRealShapeRow(snapshot.Rows, "state_5.sqlite", "thread_spawn_edges", "parent_thread_id") {
		t.Fatalf("fixture 未定位关键 thread 行: %+v", snapshot.Rows)
	}
}

func TestInspectRealShapeFixtureFailsClosed(t *testing.T) {
	script := realShapeFixtureScript(t)
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "标记篡改",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, realShapeFixtureMarker)
				var marker RealShapeFixtureManifest
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(content, &marker); err != nil {
					t.Fatal(err)
				}
				marker.Kind = "tampered"
				content, err = json.Marshal(marker)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, content, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "缺失 rollout",
			mutate: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "sessions/2026/01/01/rollout-2026-01-01T00-00-00-00000000-0000-4000-8000-000000000001.jsonl")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "缺失 index 关联",
			mutate: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, realShapeSessionIndex), []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "rollout 路径穿越",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "state_5.sqlite")
				database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=rw")
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				if _, err := database.Exec(`UPDATE threads SET rollout_path='../outside.jsonl' WHERE id=?`, realShapeFixtureThreadID); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := createRealShapeFixtureForTest(t, script)
			test.mutate(t, root)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := InspectRealShapeFixture(ctx, root, realShapeFixtureThreadID); err == nil {
				t.Fatalf("篡改后的 fixture 被错误接受")
			}
		})
	}
}

func TestInspectRealShapeCopyMapsAbsoluteRolloutPathFromManifest(t *testing.T) {
	root := createRealShapeFixtureForTest(t, realShapeFixtureScript(t))
	sourceRoot := filepath.Join(t.TempDir(), ".codex")
	database, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(sourceRoot, "sessions/2026/01/01/rollout-2026-01-01T00-00-00-00000000-0000-4000-8000-000000000001.jsonl")
	if _, err := database.Exec(`UPDATE threads SET rollout_path=? WHERE id=?`, rollout, realShapeFixtureThreadID); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(RealShapeFixtureManifest{
		Kind: RealShapeCopyKind, SchemaSHA256: RealShapeFixtureSchemaSHA256,
		SQLiteSemanticsSHA256: RealShapeSQLiteSemanticsSHA256, SourceRoot: sourceRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, realShapeCopyMarker), append(marker, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := InspectRealShapeCopy(context.Background(), root, realShapeFixtureThreadID)
	if err != nil {
		t.Fatalf("稳定副本绝对 rollout_path 映射失败: %v", err)
	}
	want := filepath.ToSlash(filepath.Join("sessions/2026/01/01", "rollout-2026-01-01T00-00-00-00000000-0000-4000-8000-000000000001.jsonl"))
	if snapshot.RolloutPath != want {
		t.Fatalf("绝对 rollout_path 未映射为副本相对路径: got=%q want=%q", snapshot.RolloutPath, want)
	}
}

func TestResolveRolloutPathRejectsUnmappedAbsolutePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copy")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := ResolveRolloutPath(root, filepath.Join(t.TempDir(), "outside.jsonl")); ok {
		t.Fatal("未提供稳定副本源根时不应接受越出扫描根的绝对路径")
	}
}

func realShapeFixtureScript(t *testing.T) string {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("CODEX_CHRONICLE_REALSHAPE_FIXTURE")); configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return configured
		}
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Clean(filepath.Join(workingDirectory, "..", "..", "docs", "项目验证", "codex_realshape_fixture.py"))
	if _, err := os.Stat(script); errors.Is(err, os.ErrNotExist) {
		t.Skipf("未找到脱敏真实形状 fixture 生成器: %s", script)
	}
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func createRealShapeFixtureForTest(t *testing.T, script string) string {
	t.Helper()
	python := "python3"
	if _, err := exec.LookPath(python); err != nil {
		python = "python"
		if _, err := exec.LookPath(python); err != nil {
			t.Skip("未找到 Python 解释器，跳过脱敏真实形状 fixture 集成回归")
		}
	}
	root := filepath.Join(t.TempDir(), "fixture")
	command := exec.Command(python, script, "--root", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("生成脱敏真实形状 fixture 失败: %v\n%s", err, output)
	}
	return root
}

func hasRealShapeRow(rows []RealShapeRowSnapshot, database, table, column string) bool {
	for _, row := range rows {
		if row.Database == database && row.Table == table && row.Column == column && row.RowCount > 0 && row.RowsComplete && len(row.RowKeys) > 0 {
			return true
		}
	}
	return false
}
