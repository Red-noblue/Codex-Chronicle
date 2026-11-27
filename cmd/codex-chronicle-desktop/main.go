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
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"codex-chronicle.local/internal/desktop"
	"codex-chronicle.local/internal/desktopui"
	updatepkg "codex-chronicle.local/internal/update"
)

var version = "0.0.0-dev"

// updateFeedURL 由发布构建注入固定更新 feed；开发构建为空，不开启在线更新。
var updateFeedURL = ""

func main() {
	dataDir := flag.String("data-dir", "", "用户数据目录；默认使用系统当前用户应用数据目录")
	packageRoot := flag.String("package-root", "", "安装载荷目录；默认从可执行文件定位")
	address := flag.String("address", "127.0.0.1:8787", "网页与 API 的统一本机地址；省略时自动避让冲突，显式指定时严格使用该地址")
	scanRoot := flag.String("scan-root", "", "首次只读扫描目录；省略时自动发现用户 Codex 目录")
	noBrowser := flag.Bool("no-browser", false, "启动后只输出地址，不打开浏览器或菜单栏")
	background := flag.Bool("background", false, "登录自启动：显示菜单栏或托盘，不主动打开网页")
	launchActive := flag.Bool("launch-active", false, "登录启动专用：从固定入口验签并启动当前活动版本")
	startupSettings := flag.Bool("startup-settings", false, "直接打开端口设置，无需后台成功启动")
	exportDiagnostics := flag.String("export-diagnostics", "", "离线导出脱敏更新诊断 ZIP 到新文件；无需启动后台，不覆盖现有文件")
	status := flag.Bool("status", false, "核对并显示已有后台实例，不启动新进程")
	quit := flag.Bool("quit", false, "等待任务完成并退出已有后台；不会强制终止")
	verify := flag.Bool("verify", false, "使用空白隔离目录验证此安装包的启动、版本和安全退出")
	startupCheck := flag.Bool("startup-check", false, "安装器专用：启动暂停接收操作的候选版本")
	restartInstance := flag.String("restart-instance", "", "更新器专用：安全退出指定实例并重新打开应用")
	restartVersion := flag.String("restart-version", "", "更新器专用：本次重新启动的已确认目标版本")
	activate := flag.Bool("activate", false, "安装器专用：核对版本后开放已通过检查的后台")
	installApp := flag.String("install-app", "", "安装器专用：完整 macOS 应用来源")
	installTarget := flag.String("install-target", "", "安装器专用：完整 macOS 应用目标")
	printVersion := flag.Bool("version", false, "显示启动器版本")
	flag.Parse()
	autoPort := true
	flag.Visit(func(option *flag.Flag) {
		if option.Name == "address" {
			autoPort = false
		}
	})
	explicitDataDir := *dataDir
	var executable string
	fatal := func(err any) {
		if !*noBrowser && !*status && !*quit && !*verify && !*startupCheck && !*activate && !*printVersion && *exportDiagnostics == "" {
			if executable != "" && *dataDir != "" {
				desktopui.ShowStartupError(fmt.Sprint(err), executable, *dataDir, *packageRoot)
			} else {
				desktopui.ShowError(fmt.Sprint(err))
			}
		}
		log.Fatal(err)
	}
	if *printVersion {
		fmt.Println(version)
		return
	}
	var err error
	executable, err = os.Executable()
	if err != nil {
		fatal(err)
	}
	// 在初始化用户目录前拦住磁盘镜像内的直接启动；诊断、安装器和只读验证仍可运行。
	if !*status && !*quit && !*verify && !*startupCheck && !*activate && !*startupSettings && *exportDiagnostics == "" && *installApp == "" && *restartInstance == "" {
		required, err := desktop.DragInstallRequired(executable)
		if err != nil {
			fatal(err)
		}
		if required {
			if *noBrowser || *background {
				log.Fatal("请先将 Codex Chronicle 拖入应用程序文件夹，再从已安装的位置打开")
			}
			desktopui.ShowInstallRequired()
			return
		}
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
	*dataDir, err = filepath.Abs(*dataDir)
	if err != nil {
		fatal(err)
	}
	*packageRoot, err = filepath.Abs(*packageRoot)
	if err != nil {
		fatal(err)
	}
	if *exportDiagnostics != "" {
		id, err := updatepkg.WriteDiagnosticsFile(context.Background(), filepath.Join(*dataDir, "updates"), version, *exportDiagnostics)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("诊断包已保存：%s\n诊断编号：%s\n仅在本地生成，请检查后主动分享给开发者。\n", *exportDiagnostics, id)
		return
	}
	if *startupSettings {
		desktopui.Settings(executable, *dataDir, *packageRoot)
		return
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
	if *launchActive {
		if err := desktop.LaunchActiveFromLogin(ctx, executable, *dataDir, *noBrowser); err != nil {
			fatal(err)
		}
		return
	}
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
		if err := desktop.WaitTrayExit(ctx, *dataDir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *restartInstance != "" {
		// 独立协作进程等待数据任务正常结束，不受原 HTTP 请求或旧后台退出影响。
		if err := desktop.RestartAfterUpdate(context.Background(), desktop.LaunchOptions{PackageRoot: *packageRoot, DataDir: *dataDir, ExpectedVersion: version, UpdateFeedURL: updateFeedURL, NoBrowser: *noBrowser, Background: *background}, *restartInstance, *restartVersion); err != nil {
			fatal(err)
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
		active, err := desktop.Running(ctx, *dataDir)
		if err != nil {
			log.Fatal(err)
		}
		if err := active.CheckHealth(ctx); err != nil {
			log.Fatal(err)
		}
		if err := desktop.ConfirmUpdateLaunch(ctx, desktop.LaunchOptions{PackageRoot: *packageRoot, DataDir: *dataDir, UpdateFeedURL: updateFeedURL}, active); err != nil {
			log.Fatal(err)
		}
		return
	}
	if !*status && !*startupCheck {
		launcher, root, err := desktop.UpdatedLauncher(ctx, desktop.LaunchOptions{PackageRoot: *packageRoot, DataDir: *dataDir, ExpectedVersion: version, UpdateFeedURL: updateFeedURL})
		if err != nil {
			fatal(err)
		}
		if launcher != "" {
			args := append(append([]string{}, os.Args[1:]...), "--package-root", root)
			command := exec.Command(launcher, args...)
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := command.Run(); err != nil {
				fatal(err)
			}
			return
		}
	}
	var instance desktop.Instance
	if *status {
		instance, err = desktop.Running(ctx, *dataDir)
	} else {
		instance, err = desktop.Launch(ctx, desktop.LaunchOptions{PackageRoot: *packageRoot, DataDir: *dataDir, Address: *address, AutoPort: autoPort, ScanRoot: *scanRoot, StartupCheck: *startupCheck, NoBrowser: *noBrowser, ExpectedVersion: version, UpdateFeedURL: updateFeedURL})
	}
	if err != nil {
		fatal(err)
	}
	if *startupCheck {
		if err := instance.CheckHealth(ctx); err != nil {
			log.Fatal(err)
		}
		if err := desktop.PrepareLoginLauncher(ctx, desktop.LaunchOptions{PackageRoot: *packageRoot, DataDir: *dataDir, ExpectedVersion: version}); err != nil {
			log.Fatal(err)
		}
	}
	instance.Token = ""
	_ = json.NewEncoder(os.Stdout).Encode(instance)
	if !*status && !*noBrowser {
		if !*background {
			if err := desktop.OpenBrowser(instance.URL); err != nil {
				log.Printf("请在浏览器中打开 %s：%v", instance.URL, err)
			}
		}
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" || runtime.GOOS == "linux" {
			go watchBackend(*dataDir, instance.ID)
		}
		desktopui.Run(instance.URL, executable, *dataDir, *packageRoot)
	}
}

// watchBackend 在安装器从其他进程退出或替换后台后关闭旧菜单栏入口，避免重复图标和过期地址。
func watchBackend(dataDir, instanceID string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	failures := 0
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		current, err := desktop.Running(ctx, dataDir)
		cancel()
		if err != nil {
			failures++
			if failures < 3 {
				continue
			}
		} else {
			failures = 0
			if current.ID == instanceID {
				continue
			}
		}
		desktopui.Stop()
		return
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
	instance, err := desktop.Launch(ctx, desktop.LaunchOptions{PackageRoot: root, DataDir: data, ScanRoot: source, Address: "127.0.0.1:0", ExpectedVersion: version})
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
