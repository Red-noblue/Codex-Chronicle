// 中文说明：安装版轻量入口，负责启动、复用、检查和安全退出后台；前端继续使用系统浏览器。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"codex-chronicle.local/internal/desktop"
	"codex-chronicle.local/internal/desktopui"
)

var version = "0.0.0-dev"

func main() {
	dataDir := flag.String("data-dir", "", "用户数据目录；默认使用系统当前用户应用数据目录")
	packageRoot := flag.String("package-root", "", "安装载荷目录；默认从可执行文件定位")
	address := flag.String("address", "127.0.0.1:8787", "本机监听地址；端口冲突时安装版会选用空闲端口")
	scanRoot := flag.String("scan-root", "", "首次只读扫描目录；省略时自动发现用户 Codex 目录")
	noBrowser := flag.Bool("no-browser", false, "启动后只输出地址，不打开浏览器或菜单栏")
	status := flag.Bool("status", false, "核对并显示已有后台实例，不启动新进程")
	quit := flag.Bool("quit", false, "等待任务完成并退出已有后台；不会强制终止")
	verify := flag.Bool("verify", false, "使用空白隔离目录验证此安装包的启动、版本和安全退出")
	startupCheck := flag.Bool("startup-check", false, "安装器专用：启动暂停接收操作的候选版本")
	activate := flag.Bool("activate", false, "安装器专用：核对版本后开放已通过检查的后台")
	installApp := flag.String("install-app", "", "安装器专用：完整 macOS 应用来源")
	installTarget := flag.String("install-target", "", "安装器专用：完整 macOS 应用目标")
	printVersion := flag.Bool("version", false, "显示启动器版本")
	flag.Parse()
	explicitDataDir := *dataDir
	fatal := func(err any) {
		if !*noBrowser && !*status && !*quit && !*verify && !*startupCheck && !*activate && !*printVersion {
			desktopui.ShowError(fmt.Sprint(err))
		}
		log.Fatal(err)
	}
	if *printVersion {
		fmt.Println(version)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	if *packageRoot == "" {
		*packageRoot = desktop.PackageRoot(executable)
	}
	if *dataDir == "" {
		*dataDir, err = desktop.DefaultDataDir()
		if err != nil {
			fatal(err)
		}
	}
	if *verify {
		if err := verifyPackage(*packageRoot, explicitDataDir); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("安装包验证通过：%s\n", version)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if *installApp != "" {
		if err := desktop.InstallMacApp(ctx, *installApp, *installTarget, *dataDir, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *quit {
		if err := desktop.Quit(ctx, *dataDir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *activate {
		instance, err := desktop.Running(ctx, *dataDir)
		if err != nil {
			log.Fatal(err)
		}
		if instance.Version != version {
			log.Fatal("候选版本不一致，未开放操作")
		}
		if err := instance.Activate(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	var instance desktop.Instance
	if *status {
		instance, err = desktop.Running(ctx, *dataDir)
	} else {
		instance, err = desktop.Launch(ctx, desktop.LaunchOptions{PackageRoot: *packageRoot, DataDir: *dataDir, Address: *address, ScanRoot: *scanRoot, StartupCheck: *startupCheck, ExpectedVersion: version})
	}
	if err != nil {
		fatal(err)
	}
	if *startupCheck {
		if err := instance.CheckHealth(ctx); err != nil {
			log.Fatal(err)
		}
	}
	instance.Token = ""
	_ = json.NewEncoder(os.Stdout).Encode(instance)
	if !*status && !*noBrowser {
		if err := desktop.OpenBrowser(instance.URL); err != nil {
			log.Printf("请在浏览器中打开 %s：%v", instance.URL, err)
		}
		if runtime.GOOS == "darwin" {
			go watchBackend(*dataDir, instance.ID)
		}
		desktopui.Run(instance.URL, executable, *dataDir)
	}
}

// watchBackend 在安装器从其他进程退出或替换后台后关闭旧菜单栏入口，避免重复图标和过期地址。
func watchBackend(dataDir, instanceID string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		current, err := desktop.Running(ctx, dataDir)
		cancel()
		if err != nil || current.ID != instanceID {
			desktopui.Stop()
			return
		}
	}
}

func verifyPackage(root, profile string) error {
	scratch, err := os.MkdirTemp("", "codex-chronicle-package-check-")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(scratch)
		}
	}()
	source := filepath.Join(scratch, "empty-source")
	if err := os.Mkdir(source, 0o700); err != nil {
		return err
	}
	data := filepath.Join(scratch, "profile")
	if profile != "" {
		profile, err = desktop.PrepareDirectory(profile)
		if err != nil {
			return err
		}
		lease, err := desktop.Acquire(filepath.Join(profile, "runtime"))
		if err != nil {
			return fmt.Errorf("验证现有数据库前必须先安全退出该数据目录的后台：%w", err)
		}
		copyErr := copyProfileDatabase(profile, data)
		closeErr := lease.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cleanup = false
	instance, err := desktop.Launch(ctx, desktop.LaunchOptions{PackageRoot: root, DataDir: data, ScanRoot: source, Address: "127.0.0.1:0"})
	if err != nil {
		quitCtx, quitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer quitCancel()
		cleanup = desktop.Quit(quitCtx, data) == nil
		return fmt.Errorf("%w；检查目录：%s", err, scratch)
	}
	healthErr := instance.CheckHealth(ctx)
	err = desktop.Quit(ctx, data)
	cleanup = err == nil
	if err != nil {
		return fmt.Errorf("%w；检查目录：%s", err, scratch)
	}
	if instance.Version != version {
		return fmt.Errorf("版本不一致：启动器 %s，后台 %s", version, instance.Version)
	}
	if healthErr != nil {
		return healthErr
	}
	return nil
}

// copyProfileDatabase 只在持有已停止实例的锁后复制项目数据库；候选迁移发生在临时副本，绝不读取源 .codex。
func copyProfileDatabase(profile, destination string) error {
	source := filepath.Join(profile, "archive.db")
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("项目数据库不是普通文件")
	}
	if wal, err := os.Stat(source + "-wal"); err == nil && wal.Size() != 0 {
		return fmt.Errorf("项目数据库仍有未合并 WAL，拒绝复制，请先完成安全退出")
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(filepath.Join(destination, "archive.db"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
