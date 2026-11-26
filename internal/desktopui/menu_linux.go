// 中文说明：Linux 使用会话 D-Bus 托盘与 Zenity 原生设置；无图标宿主时显示管理窗口，兼容 Wayland/X11且不增加 TCP 端口。
package desktopui

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-chronicle.local/internal/desktop"
	"codex-chronicle.local/internal/desktopui/linuxtray"
	"github.com/godbus/dbus/v5"
)

var linuxLifecycle struct {
	sync.Mutex
	cancel context.CancelFunc
}

type linuxUI struct {
	context  context.Context
	cancel   context.CancelFunc
	actions  chan int32
	mu       sync.Mutex
	stopping bool
	dialog   bool
	host     atomic.Bool
}

type linuxManagement struct{ ui *linuxUI }

func (m *linuxManagement) ShowIfNeeded() *dbus.Error {
	if !m.ui.host.Load() {
		select {
		case m.ui.actions <- linuxtray.Manage:
		default:
		}
	}
	return nil
}

func linuxBusName(dataDir string) string {
	return fmt.Sprintf("local.codex_chronicle.Desktop.profile_%x", sha256.Sum256([]byte(dataDir)))
}

func ShowError(message string) {
	if !linuxDisplay() {
		log.Print(message)
		return
	}
	if _, err := linuxDialog(context.Background(), "--error", "--title=Codex Chronicle", "--text="+message); err != nil {
		log.Printf("%s；提示窗口不可用：%v", message, err)
	}
}
func ShowStartupError(message, executable, dataDir, packageRoot string) {
	configure("", executable, dataDir, packageRoot)
	if !linuxDisplay() {
		log.Print(message)
		return
	}
	_, err := linuxDialog(context.Background(), "--question", "--title=Codex Chronicle 暂时无法打开", "--text="+message+"\n\n是否打开端口设置？保存后重新打开应用即可。", "--ok-label=端口设置…", "--cancel-label=关闭")
	if err == nil {
		linuxSettings(context.Background())
	}
}
func Settings(executable, dataDir, packageRoot string) {
	configure("", executable, dataDir, packageRoot)
	linuxSettings(context.Background())
}
func Stop() {
	linuxLifecycle.Lock()
	cancel := linuxLifecycle.cancel
	linuxLifecycle.Unlock()
	if cancel != nil {
		cancel()
	}
}
func Run(url, executable, dataDir, packageRoot string) {
	guard, err := desktop.AcquireTray(dataDir)
	if err != nil {
		if !errors.Is(err, desktop.ErrAlreadyRunning) {
			ShowError(err.Error())
		} else if conn, err := dbus.ConnectSessionBus(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			defer conn.Close()
			_ = conn.Object(linuxBusName(dataDir), "/Desktop").CallWithContext(ctx, "local.codex_chronicle.Desktop.ShowIfNeeded", 0).Err
		}
		return
	}
	defer guard.Close()
	configure(url, executable, dataDir, packageRoot)
	if !linuxDisplay() {
		log.Printf("没有图形桌面；工作台：%s；可使用启动器 --status / --quit 管理后台", url)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	linuxLifecycle.Lock()
	linuxLifecycle.cancel = cancel
	linuxLifecycle.Unlock()
	defer func() { linuxLifecycle.Lock(); linuxLifecycle.cancel = nil; linuxLifecycle.Unlock() }()
	ui := &linuxUI{context: ctx, cancel: cancel, actions: make(chan int32, 16)}
	conn, err := dbus.ConnectSessionBus()
	var item *linuxtray.Item
	if err == nil {
		defer conn.Close()
		item, err = linuxtray.Export(conn, ui.state, ui.actions)
		if err == nil {
			if exportErr := conn.Export(&linuxManagement{ui}, "/Desktop", "local.codex_chronicle.Desktop"); exportErr == nil {
				_, _ = conn.RequestName(linuxBusName(dataDir), dbus.NameFlagDoNotQueue)
			}
		}
	}
	if err != nil {
		log.Printf("系统托盘不可用，使用独立管理窗口：%v", err)
	}
	host := false
	register := func() bool {
		if item == nil {
			return false
		}
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return item.Register(probe)
	}
	host = register()
	ui.host.Store(host)
	if !host {
		ui.showManager()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next := register()
			if host && !next {
				ui.showManager()
			}
			host = next
			ui.host.Store(host)
			if item != nil {
				item.Refresh()
			}
		case action := <-ui.actions:
			ui.handle(action)
			if item != nil {
				item.Refresh()
			}
		}
	}
}
func linuxDisplay() bool { return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" }
func linuxDialog(ctx context.Context, arguments ...string) (string, error) {
	if !linuxDisplay() {
		return "", fmt.Errorf("没有图形桌面，无法显示设置窗口")
	}
	path, err := exec.LookPath("zenity")
	if err != nil {
		return "", fmt.Errorf("请通过发行版包管理器安装 zenity；工作台仍可通过浏览器访问，--quit 可安全退出")
	}
	command := exec.CommandContext(ctx, path, append([]string{"--no-markup"}, arguments...)...)
	output, err := command.Output()
	return strings.TrimSuffix(string(output), "\n"), err
}
func linuxCanceled(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && (exit.ExitCode() == 1 || exit.ExitCode() == 5)
}
func linuxSettings(ctx context.Context) {
	for {
		settings, _, loadErr := controller.settings()
		status := "开启"
		if !settings.AutoFallback {
			status = "关闭"
		}
		text := "网页与 API 共用一个本机端口，仅监听 127.0.0.1。\n保存后下次启动生效，当前任务继续运行。"
		if controller.url != "" {
			text += "\n当前地址：" + controller.url
		}
		if loadErr != nil {
			text += "\n原配置读取失败：" + loadErr.Error()
		}
		result, err := linuxDialog(ctx, "--forms", "--title=端口设置 · Codex Chronicle", "--width=460", "--text="+text, "--separator=\x1f", "--add-entry=首选端口（当前："+strconv.Itoa(settings.Port)+"，留空保留）", "--add-combo=端口占用时自动回退（当前："+status+"）", "--combo-values=开启（推荐）|关闭", "--ok-label=保存", "--cancel-label=取消")
		if err != nil {
			if !linuxCanceled(err) && ctx.Err() == nil {
				ShowError(err.Error())
			}
			return
		}
		fields := strings.Split(result, "\x1f")
		if len(fields) != 2 {
			ShowError("无法读取端口设置，请重新输入")
			continue
		}
		port := strings.TrimSpace(fields[0])
		if port == "" {
			port = strconv.Itoa(settings.Port)
		}
		fallback := settings.AutoFallback
		if fields[1] == "开启（推荐）" {
			fallback = true
		} else if fields[1] == "关闭" {
			fallback = false
		}
		if err := controller.savePort(port, fallback); err != nil {
			ShowError(err.Error())
			continue
		}
		return
	}
}
func (ui *linuxUI) state() linuxtray.State {
	settings, login, err := controller.settings()
	ui.mu.Lock()
	stopping := ui.stopping
	ui.mu.Unlock()
	state := linuxtray.State{URL: controller.url, Login: login, Fallback: settings.AutoFallback, Stopping: stopping}
	if err != nil {
		state.SettingsError = err.Error()
	}
	return state
}
func (ui *linuxUI) showManager() {
	ui.mu.Lock()
	if ui.dialog {
		ui.mu.Unlock()
		return
	}
	ui.dialog = true
	ui.mu.Unlock()
	go func() {
		state := ui.state()
		login, fallback := "关闭", "关闭"
		if state.Login {
			login = "开启"
		}
		if state.Fallback {
			fallback = "开启"
		}
		text := "工作台运行中：" + state.URL + "\nGNOME 需 AppIndicator 扩展才能显示图标。\n关闭此窗口后后台继续运行，可从应用菜单再次打开。"
		choices := []string{"--list", "--title=Codex Chronicle · 后台管理", "--text=" + text, "--width=520", "--height=480", "--column=操作编号", "--column=操作", "--hide-column=1", "--print-column=1", "--ok-label=执行", "--cancel-label=关闭窗口",
			"1", "打开工作台", "2", "查看并复制工作台地址", "3", "登录时启动：" + login, "4", "端口设置…", "5", "端口占用时自动回退：" + fallback, "6", "打开日志文件夹", "7", "安全退出 Codex Chronicle"}
		selected, err := linuxDialog(ui.context, choices...)
		ui.mu.Lock()
		ui.dialog = false
		ui.mu.Unlock()
		if err != nil {
			if !linuxCanceled(err) && ui.context.Err() == nil {
				log.Printf("管理窗口不可用：%v；地址：%s；可运行启动器 --quit", err, state.URL)
			}
			return
		}
		id, err := strconv.ParseInt(strings.TrimSpace(selected), 10, 32)
		if err != nil {
			return
		}
		select {
		case ui.actions <- int32(id):
		case <-ui.context.Done():
		}
	}()
}
func (ui *linuxUI) handle(id int32) {
	if id == linuxtray.Manage {
		ui.showManager()
		return
	}
	if id == linuxtray.Quit {
		ui.mu.Lock()
		if ui.stopping {
			ui.mu.Unlock()
			return
		}
		ui.stopping = true
		ui.mu.Unlock()
		go func() {
			if err := controller.quit(); err != nil {
				ui.mu.Lock()
				ui.stopping = false
				ui.mu.Unlock()
				ShowError(err.Error())
			} else {
				ui.cancel()
			}
		}()
		return
	}
	// 原生弹窗独立运行，避免用户停留在设置窗口时阻塞托盘宿主探测与安全退出。
	ui.mu.Lock()
	if ui.dialog {
		ui.mu.Unlock()
		return
	}
	ui.dialog = true
	ui.mu.Unlock()
	go func() {
		defer func() { ui.mu.Lock(); ui.dialog = false; ui.mu.Unlock() }()
		var err error
		switch id {
		case linuxtray.Open:
			err = controller.open()
		case linuxtray.Copy:
			err = linuxCopy(ui.context, controller.url)
		case linuxtray.Login:
			err = controller.toggleAutostart()
		case linuxtray.Port:
			linuxSettings(ui.context)
		case linuxtray.Fallback:
			err = controller.toggleFallback()
		case linuxtray.Logs:
			err = controller.logs()
		}
		if err != nil && !linuxCanceled(err) && ui.context.Err() == nil {
			ShowError(err.Error())
		}
	}()
}

func linuxCopy(ctx context.Context, value string) error {
	var arguments []string
	program := "xclip"
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		program = "wl-copy"
	} else {
		arguments = []string{"-selection", "clipboard"}
	}
	if path, err := exec.LookPath(program); err == nil {
		command := exec.CommandContext(ctx, path, arguments...)
		command.Stdin = strings.NewReader(value)
		if err := command.Run(); err == nil {
			return nil
		}
	}
	_, err := linuxDialog(ctx, "--entry", "--title=工作台地址", "--text=当前实际地址，可选中复制：", "--entry-text="+value, "--ok-label=完成", "--cancel-label=关闭")
	return err
}
