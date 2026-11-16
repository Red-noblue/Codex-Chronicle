// 中文说明：原生菜单栏只承载打开工作台和退出动作，退出请求通过启动器的认证控制通道完成。
#import <AppKit/AppKit.h>

@interface ChronicleMenu : NSObject <NSApplicationDelegate>
@property(nonatomic, strong) NSStatusItem *statusItem;
@property(nonatomic, copy) NSString *url;
@property(nonatomic, copy) NSString *executable;
@property(nonatomic, copy) NSString *dataDir;
@property(nonatomic, assign) BOOL stopping;
@property(nonatomic, strong) NSMenuItem *quitItem;
@end

@implementation ChronicleMenu
- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    self.statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
    self.statusItem.button.image = [NSImage imageWithSystemSymbolName:@"archivebox" accessibilityDescription:@"Codex Chronicle"];
    self.statusItem.button.toolTip = @"Codex Chronicle";
    NSMenu *menu = [[NSMenu alloc] init];
    NSMenuItem *open = [[NSMenuItem alloc] initWithTitle:@"打开工作台" action:@selector(openWorkspace:) keyEquivalent:@""];
    open.target = self;
    [menu addItem:open];
    [menu addItem:[NSMenuItem separatorItem]];
    self.quitItem = [[NSMenuItem alloc] initWithTitle:@"退出后台" action:@selector(quitBackend:) keyEquivalent:@""];
    self.quitItem.target = self;
    [menu addItem:self.quitItem];
    self.statusItem.menu = menu;
}
- (void)openWorkspace:(id)sender {
    [[NSWorkspace sharedWorkspace] openURL:[NSURL URLWithString:self.url]];
}
- (BOOL)applicationShouldHandleReopen:(NSApplication *)sender hasVisibleWindows:(BOOL)flag {
    [self openWorkspace:nil];
    return NO;
}
- (void)quitBackend:(id)sender {
    if (self.stopping) return;
    self.stopping = YES;
    self.quitItem.title = @"正在等待任务完成…";
    NSTask *task = [[NSTask alloc] init];
    task.executableURL = [NSURL fileURLWithPath:self.executable];
    task.arguments = @[@"--quit", @"--data-dir", self.dataDir];
    task.standardOutput = [NSFileHandle fileHandleWithNullDevice];
    task.standardError = [NSFileHandle fileHandleWithNullDevice];
    task.terminationHandler = ^(NSTask *finished) {
        dispatch_async(dispatch_get_main_queue(), ^{
            if (finished.terminationStatus == 0) {
                [NSApp terminate:nil];
            } else {
                self.stopping = NO;
                self.quitItem.title = @"退出后台";
                NSAlert *alert = [[NSAlert alloc] init];
                alert.messageText = @"后台尚未退出";
                alert.informativeText = @"任务可能仍在进行，未强制终止。请等待完成后重试。";
                [alert addButtonWithTitle:@"知道了"];
                [alert runModal];
            }
        });
    };
    NSError *error = nil;
    if (![task launchAndReturnError:&error]) {
        self.stopping = NO;
        self.quitItem.title = @"退出后台";
    }
}
@end

void chronicleMenu(const char *url, const char *executable, const char *dataDir) {
    @autoreleasepool {
        NSApplication *application = [NSApplication sharedApplication];
        [application setActivationPolicy:NSApplicationActivationPolicyAccessory];
        ChronicleMenu *delegate = [[ChronicleMenu alloc] init];
        delegate.url = [NSString stringWithUTF8String:url];
        delegate.executable = [NSString stringWithUTF8String:executable];
        delegate.dataDir = [NSString stringWithUTF8String:dataDir];
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
        NSAlert *alert = [[NSAlert alloc] init];
        alert.messageText = @"Codex Chronicle 暂时无法打开";
        alert.informativeText = [NSString stringWithUTF8String:message];
        [alert addButtonWithTitle:@"知道了"];
        [alert runModal];
    }
}
