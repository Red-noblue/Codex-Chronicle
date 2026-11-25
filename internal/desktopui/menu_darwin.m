// 中文说明：原生菜单栏左键打开网页、右键提供分组状态菜单与端口设置；所有退出均等待后台任务完成。
#import <AppKit/AppKit.h>

extern char *chronicleAction(char *action, char *value, int enabled);

static NSDictionary *action(NSString *name, NSString *value, BOOL enabled) {
    char *result = chronicleAction((char *)name.UTF8String, (char *)(value ?: @"").UTF8String, enabled);
    NSData *data = [[NSString stringWithUTF8String:result] dataUsingEncoding:NSUTF8StringEncoding];
    free(result);
    return [NSJSONSerialization JSONObjectWithData:data options:0 error:nil] ?: @{};
}

static void showMessage(NSString *title, NSString *message) {
    NSAlert *alert = [[NSAlert alloc] init];
    alert.messageText = title;
    alert.informativeText = message;
    [alert addButtonWithTitle:@"知道了"];
    [alert runModal];
}

static void settingsDialog(void) {
    NSDictionary *state = action(@"state", nil, NO);
    NSAlert *alert = [[NSAlert alloc] init];
    alert.messageText = @"端口设置";
    alert.icon = [NSImage imageWithSystemSymbolName:@"network" accessibilityDescription:@"本机连接"];
    alert.informativeText = @"网页与 API 共用一个本机端口。保存后，下次启动生效；当前任务继续运行。";
    [alert addButtonWithTitle:@"保存"];
    [alert addButtonWithTitle:@"取消"];
    NSView *view = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 340, 142)];
    NSTextField *label = [NSTextField labelWithString:@"首选端口"];
    label.frame = NSMakeRect(0, 110, 90, 22);
    [view addSubview:label];
    NSTextField *port = [[NSTextField alloc] initWithFrame:NSMakeRect(100, 110, 130, 24)];
    port.stringValue = [state[@"port"] stringValue] ?: @"8787";
    port.accessibilityLabel = @"首选端口，1 至 65535";
    [view addSubview:port];
    NSButton *fallback = [NSButton checkboxWithTitle:@"端口占用时自动回退（推荐）" target:nil action:nil];
    fallback.frame = NSMakeRect(0, 75, 340, 24);
    fallback.state = [state[@"auto_fallback"] boolValue] ? NSControlStateValueOn : NSControlStateValueOff;
    [view addSubview:fallback];
    NSString *url = state[@"url"];
    NSString *detail = url.length ? [@"当前地址：" stringByAppendingString:url] : @"仅监听本机 127.0.0.1；推荐端口 8787。";
    NSTextField *hint = [NSTextField wrappingLabelWithString:detail];
    hint.frame = NSMakeRect(0, 30, 340, 35);
    hint.textColor = NSColor.secondaryLabelColor;
    hint.font = [NSFont systemFontOfSize:11];
    [view addSubview:hint];
    NSTextField *errorLabel = [NSTextField labelWithString:state[@"error"] ?: @""];
    errorLabel.frame = NSMakeRect(0, 0, 340, 25);
    errorLabel.textColor = NSColor.systemRedColor;
    errorLabel.font = [NSFont systemFontOfSize:11];
    [view addSubview:errorLabel];
    alert.accessoryView = view;
    [NSApp activateIgnoringOtherApps:YES];
    [alert.window setInitialFirstResponder:port];
    while ([alert runModal] == NSAlertFirstButtonReturn) {
        NSDictionary *result = action(@"port", port.stringValue, fallback.state == NSControlStateValueOn);
        if (!result[@"error"]) break;
        errorLabel.stringValue = result[@"error"];
    }
}

@interface ChronicleMenu : NSObject <NSApplicationDelegate>
@property(nonatomic, strong) NSStatusItem *statusItem;
@property(nonatomic, copy) NSString *url;
@property(nonatomic, assign) BOOL stopping;
@end

@implementation ChronicleMenu
- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    self.statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
    self.statusItem.button.image = [NSImage imageWithSystemSymbolName:@"archivebox" accessibilityDescription:@"Codex Chronicle"];
    self.statusItem.button.toolTip = [@"Codex Chronicle · 运行中\n" stringByAppendingString:self.url];
    self.statusItem.button.target = self;
    self.statusItem.button.action = @selector(clicked:);
    [self.statusItem.button sendActionOn:NSEventMaskLeftMouseUp | NSEventMaskRightMouseUp];
}
- (NSMenuItem *)item:(NSString *)title action:(SEL)selector menu:(NSMenu *)menu {
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:title action:selector keyEquivalent:@""];
    item.target = self;
    [menu addItem:item];
    return item;
}
- (void)clicked:(id)sender {
    NSEvent *event = NSApp.currentEvent;
    if (event.type != NSEventTypeRightMouseUp && !(event.modifierFlags & NSEventModifierFlagControl)) {
        [self openWorkspace:nil];
        return;
    }
    NSDictionary *state = action(@"state", nil, NO);
    NSMenu *menu = [[NSMenu alloc] init];
    NSMenuItem *heading = [self item:@"Codex Chronicle" action:nil menu:menu];
    heading.attributedTitle = [[NSAttributedString alloc] initWithString:heading.title attributes:@{NSFontAttributeName:[NSFont boldSystemFontOfSize:13]}];
    [self item:self.stopping ? @"正在等待任务完成…" : @"后台运行中" action:nil menu:menu];
    NSMenuItem *address = [self item:self.url action:nil menu:menu];
    address.attributedTitle = [[NSAttributedString alloc] initWithString:self.url attributes:@{NSFontAttributeName:[NSFont monospacedSystemFontOfSize:11 weight:NSFontWeightRegular]}];
    [menu addItem:NSMenuItem.separatorItem];
    NSMenuItem *open = [self item:@"打开工作台" action:@selector(openWorkspace:) menu:menu];
    open.image = [NSImage imageWithSystemSymbolName:@"arrow.up.right.square" accessibilityDescription:nil];
    [self item:@"复制工作台地址" action:@selector(copyAddress:) menu:menu];
    [menu addItem:NSMenuItem.separatorItem];
    NSMenuItem *login = [self item:@"登录时启动" action:@selector(toggleLogin:) menu:menu];
    login.state = [state[@"autostart"] boolValue] ? NSControlStateValueOn : NSControlStateValueOff;
    [self item:@"端口设置…" action:@selector(portSettings:) menu:menu];
    NSMenuItem *fallback = [self item:@"端口占用时自动回退" action:@selector(toggleFallback:) menu:menu];
    fallback.state = [state[@"auto_fallback"] boolValue] ? NSControlStateValueOn : NSControlStateValueOff;
    [self item:@"端口设置下次启动生效" action:nil menu:menu];
    if (state[@"error"]) [self item:@"设置读取失败，请打开端口设置修复" action:nil menu:menu];
    [menu addItem:NSMenuItem.separatorItem];
    [self item:@"打开日志文件夹" action:@selector(openLogs:) menu:menu];
    [self item:self.stopping ? @"正在安全退出…" : @"退出 Codex Chronicle" action:self.stopping ? nil : @selector(quitBackend:) menu:menu];
    // 临时交给 NSStatusItem 定位和展开，避免普通弹出菜单将顶部信息滚动到屏幕之外。
    self.statusItem.menu = menu;
    [self.statusItem.button performClick:nil];
    self.statusItem.menu = nil;
}
- (void)openWorkspace:(id)sender {
    if (!self.stopping) [[NSWorkspace sharedWorkspace] openURL:[NSURL URLWithString:self.url]];
}
- (void)copyAddress:(id)sender {
    NSPasteboard *pasteboard = NSPasteboard.generalPasteboard;
    [pasteboard clearContents];
    [pasteboard setString:self.url forType:NSPasteboardTypeString];
}
- (BOOL)applicationShouldHandleReopen:(NSApplication *)sender hasVisibleWindows:(BOOL)flag {
    [self openWorkspace:nil];
    return NO;
}
- (void)performSetting:(NSString *)name {
    NSDictionary *result = action(name, nil, NO);
    if (result[@"error"]) showMessage(@"设置未能保存", result[@"error"]);
}
- (void)toggleLogin:(id)sender { [self performSetting:@"autostart"]; }
- (void)toggleFallback:(id)sender { [self performSetting:@"fallback"]; }
- (void)portSettings:(id)sender { settingsDialog(); }
- (void)openLogs:(id)sender { [self performSetting:@"logs"]; }
- (void)quitBackend:(id)sender {
    if (self.stopping) return;
    self.stopping = YES;
    self.statusItem.button.toolTip = @"Codex Chronicle · 正在等待任务完成…";
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
        NSDictionary *result = action(@"quit", nil, NO);
        dispatch_async(dispatch_get_main_queue(), ^{
            if (!result[@"error"]) [NSApp terminate:nil];
            else {
                self.stopping = NO;
                self.statusItem.button.toolTip = [@"Codex Chronicle\n" stringByAppendingString:self.url];
                showMessage(@"后台尚未退出", result[@"error"]);
            }
        });
    });
}
@end

void chronicleMenu(const char *url) {
    @autoreleasepool {
        NSApplication *application = NSApplication.sharedApplication;
        [application setActivationPolicy:NSApplicationActivationPolicyAccessory];
        ChronicleMenu *delegate = [[ChronicleMenu alloc] init];
        delegate.url = [NSString stringWithUTF8String:url];
        application.delegate = delegate;
        [application run];
    }
}
void chronicleStopMenu(void) {
    dispatch_async(dispatch_get_main_queue(), ^{ [NSApp terminate:nil]; });
}
void chronicleShowError(const char *message) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        showMessage(@"Codex Chronicle 暂时无法打开", [NSString stringWithUTF8String:message]);
    }
}
void chronicleSettings(void) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        settingsDialog();
    }
}
void chronicleStartupError(const char *message) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        NSAlert *alert = [[NSAlert alloc] init];
        alert.messageText = @"Codex Chronicle 暂时无法打开";
        alert.informativeText = [NSString stringWithUTF8String:message];
        [alert addButtonWithTitle:@"关闭"];
        [alert addButtonWithTitle:@"端口设置…"];
        if ([alert runModal] == NSAlertSecondButtonReturn) settingsDialog();
    }
}
