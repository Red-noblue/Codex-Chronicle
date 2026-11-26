// 中文说明：验证实际启动前快照、外置工作区数据库恢复、中断提交和损坏快照的保护。
package desktop

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	updatepkg "codex-chronicle.local/internal/update"
	workspacepkg "codex-chronicle.local/internal/workspace"
)

func restartFixture(t *testing.T) (LaunchOptions, *updatepkg.Manager, restartJournal) {
	t.Helper()
	data, err := PrepareDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := func(version string) string {
		t.Helper()
		root, err := PrepareDirectory(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"bin", "update"} {
			if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "bin", "codex-chronicle"), []byte(version), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "update", "trusted-public-key"), []byte(base64.StdEncoding.EncodeToString(public)), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest, err := updatepkg.BuildManifest(t.Context(), root, version, "")
		if err != nil {
			t.Fatal(err)
		}
		manifest, err = updatepkg.SignManifest(manifest, private, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := updatepkg.WriteManifest(t.Context(), root, manifest); err != nil {
			t.Fatal(err)
		}
		return root
	}
	options := LaunchOptions{DataDir: data, PackageRoot: bundle("1.0.0")}
	m, err := updateManager(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterInstallation(t.Context(), options.PackageRoot, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	plan, err := m.Prepare(t.Context(), bundle("2.0.0"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "archive.db"), []byte("升级前数据库"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "launcher.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp(m.Root(), "restart-profile-")
	if err != nil {
		t.Fatal(err)
	}
	j := restartJournal{Version: "2.0.0", Previous: "1.0.0", Work: work, Phase: "preparing"}
	return options, m, j
}

func TestRestartSnapshotChecksWholeProfileSpaceBeforeCopy(t *testing.T) {
	options, _, j := restartFixture(t)
	checked := false
	err := snapshotRestartProfileWithSpaceCheck(options.DataDir, &j, func(path string, bytes int64, phase string) error {
		checked = true
		if path != j.Work || bytes < int64(len("升级前数据库")+len("{}")) {
			t.Fatal("没有估算全部快照体积")
		}
		return &updatepkg.InsufficientSpaceError{Phase: phase, RequiredBytes: uint64(bytes)}
	})
	var space *updatepkg.InsufficientSpaceError
	if !checked || !errors.As(err, &space) || len(j.Files) != 0 {
		t.Fatalf("空间不足仍复制快照：%v", err)
	}
	files, err := os.ReadDir(j.Work)
	if err != nil || len(files) != 0 {
		t.Fatal("空间不足留下部分快照")
	}
	data, _ := os.ReadFile(filepath.Join(options.DataDir, "archive.db"))
	if string(data) != "升级前数据库" {
		t.Fatal("预检改动了项目数据库")
	}
}

func TestRestartRecoveryRestoresProfileAndExternalWorkspace(t *testing.T) {
	options, m, j := restartFixture(t)
	external, err := PrepareDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(external, "workspace.db")
	if err := os.WriteFile(db, []byte("外置数据库原内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := workspacepkg.NewFileStore(filepath.Join(options.DataDir, "workspaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg := workspacepkg.Registry{Version: 1}
	if _, err := workspacepkg.Add(t.Context(), &reg, workspacepkg.Definition{Root: t.TempDir(), ArchiveDB: db}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	settingSidecar := filepath.Join(options.DataDir, "launcher.json-wal")
	if err := os.WriteFile(settingSidecar, []byte("不属于数据库旁路文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshotRestartProfile(options.DataDir, &j); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{db, filepath.Join(options.DataDir, "archive.db")} {
		if err := os.WriteFile(path, []byte("失败迁移后的内容"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 候选改写过注册表也须从更新前注册表定位恢复目标。
	if err := registry.Save(t.Context(), workspacepkg.Registry{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := recoverRestartJournal(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{db: "外置数据库原内容", filepath.Join(options.DataDir, "archive.db"): "升级前数据库"} {
		actual, err := os.ReadFile(path)
		if err != nil || string(actual) != expected {
			t.Fatalf("快照未恢复：%s %s %v", path, actual, err)
		}
	}
	status, _ := m.Status(t.Context())
	if b, err := os.ReadFile(settingSidecar); err != nil || string(b) != "不属于数据库旁路文件" {
		t.Fatal("恢复删除了不属于数据库的文件")
	}
	if status.State.ActiveVersion != "1.0.0" || status.State.RejectedVersion != "2.0.0" {
		t.Fatalf("未恢复旧版本：%+v", status.State)
	}
	if _, err := os.Stat(filepath.Join(options.DataDir, "updates", restartJournalName)); !os.IsNotExist(err) {
		t.Fatal("恢复完成后遗留事务")
	}
}

func TestRestartRecoveryNeverRestoresCommittedOrDamagedSnapshot(t *testing.T) {
	for _, phase := range []string{"committing", "damaged"} {
		t.Run(phase, func(t *testing.T) {
			options, m, j := restartFixture(t)
			if err := snapshotRestartProfile(options.DataDir, &j); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(options.DataDir, "archive.db")
			if err := os.WriteFile(path, []byte("提交后新增数据"), 0o600); err != nil {
				t.Fatal(err)
			}
			if phase == "committing" {
				j.Phase = phase
			} else {
				if err := os.WriteFile(filepath.Join(j.Work, "0"), []byte("已损坏"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveRestartJournal(options.DataDir, j); err != nil {
				t.Fatal(err)
			}
			err := recoverRestartJournal(t.Context(), options)
			if phase == "damaged" && err == nil || phase == "committing" && err != nil {
				t.Fatal(err)
			}
			actual, _ := os.ReadFile(path)
			if string(actual) != "提交后新增数据" {
				t.Fatal("旧快照覆盖了用户新数据")
			}
			status, _ := m.Status(t.Context())
			if status.State.ActiveVersion != "2.0.0" {
				t.Fatal("提交后或快照损坏仍撤销了版本")
			}
			if phase == "damaged" {
				b, err := os.ReadFile(filepath.Join(options.DataDir, "updates", restartJournalName))
				if err != nil {
					t.Fatal(err)
				}
				var saved restartJournal
				if json.Unmarshal(b, &saved) != nil || saved.Phase != "trial" {
					t.Fatal("损坏快照恢复丢失了事务")
				}
			}
		})
	}
}
