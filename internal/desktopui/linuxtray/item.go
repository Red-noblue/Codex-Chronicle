// 中文说明：通过 StatusNotifierItem 与 DBusMenu 提供 Linux 原生状态图标，桌面宿主负责绘制菜单，不创建网络监听端口。
package linuxtray

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	ItemInterface                 = "org.kde.StatusNotifierItem"
	MenuInterface                 = "com.canonical.dbusmenu"
	Watcher                       = "org.kde.StatusNotifierWatcher"
	ItemPath      dbus.ObjectPath = "/StatusNotifierItem"
	MenuPath      dbus.ObjectPath = "/MenuBar"
	Open          int32           = 1
	Copy          int32           = 2
	Login         int32           = 3
	Port          int32           = 4
	Fallback      int32           = 5
	Logs          int32           = 6
	Quit          int32           = 7
	Manage        int32           = 8
	Updates       int32           = 9
	Download      int32           = 10
)

type State struct {
	URL                            string
	Login, Fallback, Stopping      bool
	SettingsError                  string
	UpdateLabel                    string
	UpdatePaused, UpdateCanControl bool
}
type Layout struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}
type PropertyGroup struct {
	ID         int32
	Properties map[string]dbus.Variant
}
type MenuEvent struct {
	ID        int32
	Event     string
	Data      dbus.Variant
	Timestamp uint32
}
type Item struct {
	conn        *dbus.Conn
	state       func() State
	actions     chan<- int32
	mu          sync.Mutex
	revision    uint32
	previous    State
	initialized bool
	owner       string
}
type itemMethods struct{ item *Item }
type menuMethods struct{ item *Item }
type pixmap struct {
	Width, Height int32
	Data          []byte
}
type tooltip struct {
	Icon               string
	Pixmaps            []pixmap
	Title, Description string
}

func Export(conn *dbus.Conn, state func() State, actions chan<- int32) (*Item, error) {
	item := &Item{conn: conn, state: state, actions: actions, revision: 1}
	api := &itemMethods{item}
	menu := &menuMethods{item}
	properties := map[string]*prop.Prop{}
	values := map[string]any{
		"Category": "ApplicationStatus", "Id": "codex-chronicle", "Title": "Codex Chronicle",
		"Status": "Active", "WindowId": uint32(0), "IconName": "folder-documents",
		"IconPixmap": archivePixmaps(), "OverlayIconName": "", "OverlayIconPixmap": []pixmap{},
		"AttentionIconName": "", "AttentionIconPixmap": []pixmap{}, "AttentionMovieName": "",
		"ToolTip":    tooltip{"folder-documents", archivePixmaps(), "Codex Chronicle", state().URL},
		"ItemIsMenu": false, "Menu": MenuPath,
	}
	for name, value := range values {
		properties[name] = &prop.Prop{Value: value, Emit: prop.EmitConst}
	}
	if _, err := prop.Export(conn, ItemPath, prop.Map{ItemInterface: properties}); err != nil {
		return nil, err
	}
	if err := conn.Export(api, ItemPath, ItemInterface); err != nil {
		return nil, err
	}
	if err := conn.Export(menu, MenuPath, MenuInterface); err != nil {
		return nil, err
	}
	menuProperties := map[string]*prop.Prop{
		"Version": {Value: uint32(3)}, "TextDirection": {Value: "ltr"}, "Status": {Value: "normal"}, "IconThemePath": {Value: []string{}},
	}
	if _, err := prop.Export(conn, MenuPath, prop.Map{MenuInterface: menuProperties}); err != nil {
		return nil, err
	}
	for _, node := range []struct {
		path  dbus.ObjectPath
		iface string
		api   any
		props map[string]*prop.Prop
	}{
		{ItemPath, ItemInterface, api, properties}, {MenuPath, MenuInterface, menu, menuProperties},
	} {
		iface := introspect.Interface{Name: node.iface, Methods: introspect.Methods(node.api)}
		for name, value := range node.props {
			iface.Properties = append(iface.Properties, introspect.Property{Name: name, Type: dbus.SignatureOf(value.Value).String(), Access: "read"})
		}
		if node.iface == MenuInterface {
			iface.Signals = []introspect.Signal{{Name: "LayoutUpdated", Args: []introspect.Arg{{Type: "u"}, {Type: "i"}}}}
		}
		if err := conn.Export(introspect.NewIntrospectable(&introspect.Node{Interfaces: []introspect.Interface{iface, prop.IntrospectData}}), node.path, "org.freedesktop.DBus.Introspectable"); err != nil {
			return nil, err
		}
	}
	return item, nil
}

// Register 识别宿主重启，重新向新的 watcher 注册；没有宿主时让调用方保留独立管理入口。
func (i *Item) Register(ctx context.Context) bool {
	var owner string
	err := i.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, Watcher).Store(&owner)
	if err != nil {
		i.owner = ""
		return false
	}
	if i.owner != owner {
		if err := i.conn.Object(Watcher, "/StatusNotifierWatcher").CallWithContext(ctx, Watcher+".RegisterStatusNotifierItem", 0, string(ItemPath)).Err; err != nil {
			return false
		}
		i.owner = owner
	}
	var value dbus.Variant
	err = i.conn.Object(Watcher, "/StatusNotifierWatcher").CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, Watcher, "IsStatusNotifierHostRegistered").Store(&value)
	if err != nil {
		return false
	}
	registered, _ := value.Value().(bool)
	return registered
}

func (i *Item) dispatch(id int32) *dbus.Error {
	if id < Open || id > Download {
		return dbus.NewError("com.canonical.dbusmenu.Error.InvalidMenuItem", []any{"未知菜单项"})
	}
	if i.state().Stopping && (id == Open || id == Quit || id == Download || id == Updates) {
		return nil
	}
	select {
	case i.actions <- id:
		return nil
	default:
		return dbus.NewError("org.freedesktop.DBus.Error.LimitsExceeded", []any{"正在处理菜单操作，请稍后重试"})
	}
}
func (m *itemMethods) Activate(x, y int32) *dbus.Error                    { return m.item.dispatch(Open) }
func (m *itemMethods) SecondaryActivate(x, y int32) *dbus.Error           { return m.item.dispatch(Open) }
func (m *itemMethods) ContextMenu(x, y int32) *dbus.Error                 { return m.item.dispatch(Manage) }
func (m *itemMethods) Scroll(delta int32, orientation string) *dbus.Error { return nil }

func props(label string, enabled bool) map[string]dbus.Variant {
	return map[string]dbus.Variant{"label": dbus.MakeVariant(label), "enabled": dbus.MakeVariant(enabled), "visible": dbus.MakeVariant(true)}
}
func layout(state State) Layout {
	root := Layout{ID: 0, Properties: map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, Children: []dbus.Variant{}}
	add := func(id int32, label string, enabled bool, toggle *bool) {
		entry := Layout{ID: id, Properties: props(label, enabled), Children: []dbus.Variant{}}
		if toggle != nil {
			entry.Properties["toggle-type"] = dbus.MakeVariant("checkmark")
			var checked int32
			if *toggle {
				checked = 1
			}
			entry.Properties["toggle-state"] = dbus.MakeVariant(checked)
		}
		root.Children = append(root.Children, dbus.MakeVariant(entry))
	}
	separator := func(id int32) {
		root.Children = append(root.Children, dbus.MakeVariant(Layout{id, map[string]dbus.Variant{"type": dbus.MakeVariant("separator")}, []dbus.Variant{}}))
	}
	add(100, "Codex Chronicle", false, nil)
	status := "后台运行中"
	if state.Stopping {
		status = "正在等待任务完成…"
	}
	add(101, status, false, nil)
	add(102, state.URL, false, nil)
	separator(103)
	add(Open, "打开工作台", !state.Stopping, nil)
	add(Copy, "复制工作台地址", true, nil)
	separator(104)
	add(Login, "登录时启动", true, &state.Login)
	add(Port, "端口设置…", true, nil)
	add(Fallback, "端口占用时自动回退", true, &state.Fallback)
	add(105, "端口设置下次启动生效", false, nil)
	if state.SettingsError != "" {
		add(106, "设置读取失败，请打开端口设置修复", false, nil)
	}
	separator(107)
	if state.UpdateLabel != "" {
		add(108, state.UpdateLabel, false, nil)
	}
	if state.UpdateCanControl {
		title := "暂停更新下载"
		if state.UpdatePaused {
			title = "继续下载"
		}
		add(Download, title, !state.Stopping, nil)
	}
	add(Updates, "更新设置…", !state.Stopping, nil)
	separator(109)
	add(Logs, "打开日志文件夹", true, nil)
	add(Quit, "退出 Codex Chronicle", !state.Stopping, nil)
	return root
}
func (i *Item) refresh() (State, uint32, bool) {
	state := i.state()
	i.mu.Lock()
	changed := i.initialized && !reflect.DeepEqual(i.previous, state)
	if changed {
		i.revision++
	}
	i.previous = state
	i.initialized = true
	revision := i.revision
	i.mu.Unlock()
	if changed {
		_ = i.conn.Emit(MenuPath, MenuInterface+".LayoutUpdated", revision, int32(0))
	}
	return state, revision, changed
}
func (i *Item) Refresh() { i.refresh() }
func filterProperties(properties map[string]dbus.Variant, names []string) map[string]dbus.Variant {
	if len(names) == 0 {
		return properties
	}
	selected := map[string]dbus.Variant{}
	for _, name := range names {
		if value, ok := properties[name]; ok {
			selected[name] = value
		}
	}
	return selected
}
func (m *menuMethods) GetLayout(parent, depth int32, names []string) (uint32, Layout, *dbus.Error) {
	state, revision, _ := m.item.refresh()
	root := layout(state)
	if parent != 0 {
		for _, child := range root.Children {
			entry := child.Value().(Layout)
			if entry.ID == parent {
				entry.Properties = filterProperties(entry.Properties, names)
				return revision, entry, nil
			}
		}
		return 0, Layout{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidMenuItem", []any{fmt.Sprint(parent)})
	}
	root.Properties = filterProperties(root.Properties, names)
	if depth == 0 {
		root.Children = []dbus.Variant{}
	} else {
		for index, child := range root.Children {
			entry := child.Value().(Layout)
			entry.Properties = filterProperties(entry.Properties, names)
			root.Children[index] = dbus.MakeVariant(entry)
		}
	}
	return revision, root, nil
}
func (m *menuMethods) GetGroupProperties(ids []int32, names []string) ([]PropertyGroup, *dbus.Error) {
	state, _, _ := m.item.refresh()
	root := layout(state)
	result := []PropertyGroup{}
	selected := func(id int32) bool {
		if len(ids) == 0 {
			return true
		}
		for _, v := range ids {
			if v == id {
				return true
			}
		}
		return false
	}
	if selected(0) {
		result = append(result, PropertyGroup{0, filterProperties(root.Properties, names)})
	}
	for _, child := range root.Children {
		entry := child.Value().(Layout)
		if selected(entry.ID) {
			result = append(result, PropertyGroup{entry.ID, filterProperties(entry.Properties, names)})
		}
	}
	return result, nil
}
func (m *menuMethods) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	groups, _ := m.GetGroupProperties([]int32{id}, []string{name})
	if len(groups) > 0 {
		if v, ok := groups[0].Properties[name]; ok {
			return v, nil
		}
	}
	return dbus.Variant{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidProperty", []any{name})
}
func (m *menuMethods) Event(id int32, event string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if event != "clicked" {
		return nil
	}
	return m.item.dispatch(id)
}
func (m *menuMethods) EventGroup(events []MenuEvent) ([]int32, *dbus.Error) {
	failed := []int32{}
	for _, event := range events {
		if err := m.Event(event.ID, event.Event, event.Data, event.Timestamp); err != nil {
			failed = append(failed, event.ID)
		}
	}
	return failed, nil
}
func (m *menuMethods) AboutToShow(id int32) (bool, *dbus.Error) {
	_, _, changed := m.item.refresh()
	return changed, nil
}
func (m *menuMethods) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	_, _, changed := m.item.refresh()
	updates := []int32{}
	if changed {
		updates = append(updates, ids...)
	}
	return updates, []int32{}, nil
}
func archivePixmaps() []pixmap {
	data := make([]byte, 32*32*4)
	for y := 5; y < 27; y++ {
		for x := 4; x < 28; x++ {
			if y <= 9 || y >= 11 && (x <= 6 || x >= 25 || y >= 24) || y >= 14 && y <= 16 && x >= 12 && x <= 19 {
				offset := (y*32 + x) * 4
				copy(data[offset:offset+4], []byte{255, 45, 120, 235})
			}
		}
	}
	return []pixmap{{32, 32, data}}
}
