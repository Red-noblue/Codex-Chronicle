// 中文说明：核验安装事务拒绝未知记录，并在隔离应用的多个真实替换边界强杀进程后恢复；实机样本通过环境变量显式指定。
package desktop

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallRecoveryRejectsUnknownJournalWithoutChangingFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS 安装事务使用目录 fsync")
	}
	data, err := PrepareDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := PrepareDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(parent, ".chronicle-install-test")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(data, "launcher.json")
	if err := os.WriteFile(sentinel, []byte("原始设置"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := &installJournal{Format: 1, Target: filepath.Join(filepath.Dir(work), "target.app"), Work: work, DataDir: data, Phase: "unknown", Version: "1.0.0", CandidateHash: strings.Repeat("a", 64)}
	if err := saveInstallJournal(journal); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(data, installMarker))
	if _, err := recoverInstallation(t.Context(), data); err == nil || !strings.Contains(err.Error(), "未知安装阶段") {
		t.Fatal("未知安装阶段被自动放行")
	}
	after, _ := os.ReadFile(filepath.Join(data, installMarker))
	settings, _ := os.ReadFile(sentinel)
	if string(before) != string(after) || string(settings) != "原始设置" {
		t.Fatal("未知阶段更改了恢复材料或设置")
	}
}

func TestInstallRecoveryStopsBeforeRestoringDamagedSnapshot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS 安装恢复边界")
	}
	data, err := PrepareDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := PrepareDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work, target := filepath.Join(parent, ".chronicle-install-test"), filepath.Join(parent, "target.app")
	for _, path := range []string{filepath.Join(work, "profile"), target} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{filepath.Join(data, "launcher.json"): "原设置", filepath.Join(work, "profile", "launcher.json"): "已损坏快照", filepath.Join(target, "sentinel"): "原应用"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	journal := &installJournal{Format: 1, Target: target, Work: work, DataDir: data, Phase: "checking", Version: "1.0.0", CandidateHash: strings.Repeat("a", 64), Snapshot: true, ProfileDigests: map[string]string{"launcher.json": strings.Repeat("b", 64)}}
	if err := saveInstallJournal(journal); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(data, installMarker))
	if _, err := recoverInstallation(t.Context(), data); err == nil || !strings.Contains(err.Error(), "快照") {
		t.Fatalf("损坏快照没有在恢复前阻断：%v", err)
	}
	after, _ := os.ReadFile(filepath.Join(data, installMarker))
	settings, _ := os.ReadFile(filepath.Join(data, "launcher.json"))
	app, _ := os.ReadFile(filepath.Join(target, "sentinel"))
	if string(before) != string(after) || string(settings) != "原设置" || string(app) != "原应用" {
		t.Fatal("损坏快照更改了原应用、原设置或恢复材料")
	}
}

func TestMacInstallInterruptedStages(t *testing.T) {
	app := os.Getenv("CHRONICLE_INSTALL_TEST_APP")
	if runtime.GOOS != "darwin" || app == "" {
		t.Skip("需要显式指定本轮已构建的隔离 macOS 应用")
	}
	if data := os.Getenv("CHRONICLE_INSTALL_TEST_DATA"); data != "" {
		phase := os.Getenv("CHRONICLE_INSTALL_TEST_STAGE")
		err := installMacApp(t.Context(), app, os.Getenv("CHRONICLE_INSTALL_TEST_TARGET"), data, io.Discard, func(current string) {
			if current != phase {
				return
			}
			config, _ := json.Marshal(Config{ScanRoot: os.Getenv("CHRONICLE_INSTALL_TEST_SOURCE"), ArchiveRoot: filepath.Join(data, "新设置")})
			if err := os.WriteFile(filepath.Join(data, "launcher.json"), config, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(data, "test-paused"), []byte(current), 0o600); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Hour)
		})
		t.Fatalf("故障子进程未停在检查点：%v", err)
	}
	app, _ = filepath.Abs(app)
	for _, phase := range []string{"snapshot_saved", "previous_moved", "candidate_installed", "commit_recorded", "activated", "check_stopped"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			data, source, target := filepath.Join(root, "profile"), filepath.Join(root, "source"), filepath.Join(root, "apps", "Codex Chronicle.app")
			for _, dir := range []string{data, source} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			sentinel := filepath.Join(source, "sentinel.jsonl")
			if err := os.WriteFile(sentinel, []byte("源样本不变\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			config, _ := json.Marshal(Config{ScanRoot: source})
			if err := os.WriteFile(filepath.Join(data, "launcher.json"), config, 0o600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := Quit(ctx, data); err != nil {
					t.Errorf("退出隔离后台失败：%v", err)
				}
			}()
			if err := InstallMacApp(t.Context(), app, target, data, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := Running(t.Context(), data); err == nil {
				t.Fatal("安装完成后仍保留安装器启动的后台")
			}
			lease, err := Acquire(filepath.Join(data, "runtime"))
			if err != nil {
				t.Fatalf("安装结束后后台锁未释放：%v", err)
			}
			_ = lease.Close()
			command := exec.Command(os.Args[0], "-test.run=^TestMacInstallInterruptedStages$")
			command.Env = append(os.Environ(), "CHRONICLE_INSTALL_TEST_APP="+app, "CHRONICLE_INSTALL_TEST_DATA="+data, "CHRONICLE_INSTALL_TEST_TARGET="+target, "CHRONICLE_INSTALL_TEST_STAGE="+phase, "CHRONICLE_INSTALL_TEST_SOURCE="+source)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = command.Process.Kill() }()
			deadline := time.Now().Add(45 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(data, "test-paused")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("安装未抵达强杀检查点")
				}
				time.Sleep(25 * time.Millisecond)
			}
			options := LaunchOptions{DataDir: data, PackageRoot: PackageRoot(filepath.Join(target, appEntry)), Address: "127.0.0.1:0"}
			if _, err := Launch(t.Context(), options); err == nil {
				t.Fatal("安装器持锁时普通启动未被阻断")
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = command.Wait()
			instance, err := Launch(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			if instance.StartupPaused {
				t.Fatal("恢复后后台仍暂停")
			}
			restored, err := LoadConfig(data)
			if err != nil || restored.ScanRoot != source {
				t.Fatalf("恢复设置失败：%+v %v", restored, err)
			}
			committed := phase == "commit_recorded" || phase == "activated" || phase == "check_stopped"
			if (restored.ArchiveRoot != "") != committed {
				t.Fatalf("恢复越过提交边界：%+v", restored)
			}
			if _, err := os.Stat(filepath.Join(data, installMarker)); !os.IsNotExist(err) {
				t.Fatalf("恢复后事务未清理：%v", err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "源样本不变\n" {
				t.Fatal("安装恢复改动了源目录")
			}
		})
	}
}
