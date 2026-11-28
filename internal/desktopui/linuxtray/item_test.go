// 中文说明：在独立会话总线上模拟真实托盘宿主，验证协议签名、菜单状态、事件分发及宿主重启恢复。
package linuxtray

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

type watcherMethods struct{ registrations chan string }

func (w *watcherMethods) RegisterStatusNotifierItem(service string) *dbus.Error {
	w.registrations <- service
	return nil
}
func newConnection(t *testing.T) *dbus.Conn {
	t.Helper()
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}
func startWatcher(t *testing.T, conn *dbus.Conn) *watcherMethods {
	t.Helper()
	api := &watcherMethods{make(chan string, 4)}
	if err := conn.Export(api, "/StatusNotifierWatcher", Watcher); err != nil {
		t.Fatal(err)
	}
	if _, err := prop.Export(conn, "/StatusNotifierWatcher", prop.Map{Watcher: {"IsStatusNotifierHostRegistered": {Value: true}}}); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.RequestName(Watcher, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("watcher 注册失败：%v %v", reply, err)
	}
	return api
}
func TestSessionBusProtocol(t *testing.T) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("通过 dbus-run-session 运行会话总线集成检查")
	}
	// 只在明确独立的测试总线运行，防止占用真实桌面的 watcher 名称。
	probe := newConnection(t)
	var exists bool
	if err := probe.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, Watcher).Store(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Skip("当前总线已有真实托盘宿主，不替换它")
	}
	host := newConnection(t)
	watcher := startWatcher(t, host)
	server := newConnection(t)
	actions := make(chan int32, 16)
	state := State{URL: "http://127.0.0.1:49152", Fallback: true}
	var stateMu sync.Mutex
	item, err := Export(server, func() State { stateMu.Lock(); defer stateMu.Unlock(); return state }, actions)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !item.Register(ctx) {
		t.Fatal("未注册托盘图标")
	}
	if got := <-watcher.registrations; got != string(ItemPath) {
		t.Fatalf("注册路径错误：%s", got)
	}
	destination := server.Names()[0]
	object := probe.Object(destination, ItemPath)
	menu := probe.Object(destination, MenuPath)
	menuProperty, err := object.GetProperty(ItemInterface + ".Menu")
	if err != nil || menuProperty.Value() != MenuPath {
		t.Fatalf("菜单路径无效：%v %v", menuProperty, err)
	}
	isMenu, err := object.GetProperty(ItemInterface + ".ItemIsMenu")
	if err != nil || isMenu.Value() != false {
		t.Fatalf("左键未提供激活行为：%v %v", isMenu, err)
	}
	var xml string
	if err := menu.CallWithContext(ctx, "org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&xml); err != nil || !strings.Contains(xml, `type="(ia{sv}av)"`) {
		t.Fatalf("DBusMenu introspection 无效：%s %v", xml, err)
	}
	var revision uint32
	var root Layout
	if err := menu.CallWithContext(ctx, MenuInterface+".GetLayout", 0, int32(0), int32(-1), []string{}).Store(&revision, &root); err != nil {
		t.Fatal(err)
	}
	if root.ID != 0 || len(root.Children) < 10 {
		t.Fatalf("菜单布局不完整：%+v", root)
	}
	var toggle int32
	for _, child := range root.Children {
		var entry Layout
		if err := dbus.Store([]any{child.Value()}, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.ID == Fallback {
			toggle = entry.Properties["toggle-state"].Value().(int32)
		}
	}
	if toggle != 1 {
		t.Fatal("自动回退勾选未传递")
	}
	if err := object.CallWithContext(ctx, ItemInterface+".Activate", 0, int32(10), int32(20)).Err; err != nil {
		t.Fatal(err)
	}
	if <-actions != Open {
		t.Fatal("左键未打开工作台")
	}
	if err := menu.CallWithContext(ctx, MenuInterface+".Event", 0, Port, "clicked", dbus.MakeVariant(int32(0)), uint32(0)).Err; err != nil {
		t.Fatal(err)
	}
	if <-actions != Port {
		t.Fatal("右键菜单事件分发错误")
	}
	if err := menu.CallWithContext(ctx, MenuInterface+".Event", 0, int32(999), "clicked", dbus.MakeVariant(int32(0)), uint32(0)).Err; err == nil {
		t.Fatal("接受了未知菜单动作")
	}
	stateMu.Lock()
	state.Fallback = false
	state.Login = true
	stateMu.Unlock()
	var changed bool
	if err := menu.CallWithContext(ctx, MenuInterface+".AboutToShow", 0, int32(0)).Store(&changed); err != nil || !changed {
		t.Fatalf("菜单状态未刷新：%v %v", changed, err)
	}
	if err := menu.CallWithContext(ctx, MenuInterface+".GetLayout", 0, int32(0), int32(0), []string{}).Store(&revision, &root); err != nil || revision < 2 || len(root.Children) != 0 {
		t.Fatalf("深度/修订未生效：%d %+v %v", revision, root, err)
	}
	host.Close()
	deadline := time.Now().Add(time.Second)
	for item.Register(ctx) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if item.Register(ctx) {
		t.Fatal("宿主退出后仍声称托盘可见")
	}
	replacement := newConnection(t)
	next := startWatcher(t, replacement)
	if !item.Register(ctx) {
		t.Fatal("新宿主未恢复图标")
	}
	if <-next.registrations != string(ItemPath) {
		t.Fatal("重注册路径错误")
	}
}

func TestDBusSignatures(t *testing.T) {
	for _, pair := range []struct {
		value any
		want  string
	}{
		{Layout{}, "(ia{sv}av)"}, {[]PropertyGroup{}, "a(ia{sv})"}, {[]MenuEvent{}, "a(isvu)"}, {archivePixmaps(), "a(iiay)"}, {tooltip{}, "(sa(iiay)ss)"},
	} {
		if got := dbus.SignatureOf(pair.value).String(); got != pair.want {
			t.Errorf("协议签名错误：%s，需要 %s", got, pair.want)
		}
	}
}

// 中文说明：更新状态、继续下载与设置入口使用真实DBusMenu条目，退出时不再接受新的更新动作。
func TestUpdateMenuAndControlDispatch(t *testing.T) {
	state := State{UpdateLabel: "更新已下载，等待安装", UpdatePaused: true, UpdateCanControl: true}
	root := layout(state)
	found := map[int32]Layout{}
	for _, child := range root.Children {
		entry := child.Value().(Layout)
		found[entry.ID] = entry
	}
	if found[Download].Properties["label"].Value() != "继续下载" || found[Updates].Properties["label"].Value() != "更新设置…" || found[108].Properties["enabled"].Value() != false {
		t.Fatal("更新菜单不完整", found)
	}
	actions := make(chan int32, 2)
	item := Item{state: func() State { return state }, actions: actions}
	if err := item.dispatch(Download); err != nil {
		t.Fatal(err)
	}
	if <-actions != Download {
		t.Fatal("继续下载动作未送达")
	}
	state.Stopping = true
	if err := item.dispatch(Download); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatal("退出时仍触发新的下载")
	}
}
