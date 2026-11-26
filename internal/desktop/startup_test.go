// 中文说明：验证用户端口策略优先级、非法值拒绝、原设置保留和菜单栏单实例锁。
package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestStartupSettingsAndAddress(t *testing.T) {
	dir := t.TempDir()
	address, fallback, err := StartupAddress(dir)
	if err != nil || address != defaultAddress || !fallback {
		t.Fatalf("默认启动策略错误：%s %v %v", address, fallback, err)
	}
	for _, settings := range []StartupSettings{{Port: 9000, AutoFallback: false}, {Port: 9001, AutoFallback: true}} {
		if err := SaveStartupSettings(dir, settings); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadStartupSettings(dir)
		if err != nil || loaded != settings {
			t.Fatalf("保存后读取不一致：%+v %v", loaded, err)
		}
		address, fallback, err := StartupAddress(dir)
		if err != nil || address != "127.0.0.1:"+strconv.Itoa(settings.Port) || fallback != settings.AutoFallback {
			t.Fatalf("主动配置的启动策略错误：%s %v %v", address, fallback, err)
		}
	}
	before, _ := os.ReadFile(filepath.Join(dir, "startup.json"))
	for _, port := range []int{-1, 0, 65536} {
		if err := SaveStartupSettings(dir, StartupSettings{Port: port}); err == nil {
			t.Fatalf("接受了非法端口 %d", port)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "startup.json"))
	if string(before) != string(after) {
		t.Fatal("非法设置破坏了原文件")
	}
}

func TestStartupSettingsRejectDamagedFileAndSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "startup.json"), []byte(`{"port":"broken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StartupAddress(dir); err == nil {
		t.Fatal("损坏的配置被忽略，可能导致使用意外端口")
	}
	if err := SaveStartupSettings(filepath.Join(dir, ".codex"), StartupSettings{Port: 8787}); err == nil {
		t.Fatal("允许将设置写入源目录")
	}
	if err := SaveStartupSettings(dir, StartupSettings{Port: 8787, AutoFallback: true}); err != nil {
		t.Fatalf("无法修复损坏设置：%v", err)
	}
}

func TestTraySingleInstance(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireTray(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if other, err := AcquireTray(dir); !errors.Is(err, ErrAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("未拒绝重复图标：%v", err)
	}
	first.Close()
	next, err := AcquireTray(dir)
	if err != nil {
		t.Fatalf("图标退出后未释放锁：%v", err)
	}
	next.Close()
}

func TestWaitTrayExitDoesNotReturnWhileTrayIsRunning(t *testing.T) {
	dir := t.TempDir()
	file, err := AcquireTray(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := WaitTrayExit(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("图标仍运行时错误地宣告退出完成：%v", err)
	}
	file.Close()
	if err := WaitTrayExit(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
}
