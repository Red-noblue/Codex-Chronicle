// 中文说明：验证跨进程实例锁、控制凭证、身份核对及源数据边界，防止错误复用或退出其他服务。
package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstanceLockSubprocess(t *testing.T) {
	if dir := os.Getenv("CHRONICLE_TEST_LOCK_DIR"); dir != "" {
		lease, err := Acquire(dir)
		if lease != nil {
			_ = lease.Close()
		}
		if errors.Is(err, ErrAlreadyRunning) {
			return
		}
		t.Fatalf("第二进程未被阻断：%v", err)
	}
	dir := t.TempDir()
	lease, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestInstanceLockSubprocess$")
	command.Env = append(os.Environ(), "CHRONICLE_TEST_LOCK_DIR="+dir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("锁未隔离：%v %s", err, output)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Acquire(dir)
	if err != nil {
		t.Fatalf("退出后未释放锁：%v", err)
	}
	_ = next.Close()
}

func TestAuthenticatedControlAndIdentity(t *testing.T) {
	lease, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	stopped := make(chan struct{}, 1)
	server := httptest.NewServer(lease.Handler(http.NotFoundHandler(), func() { stopped <- struct{}{} }, func() bool { return false }))
	defer server.Close()
	if err := lease.Publish(strings.TrimPrefix(server.URL, "http://"), "1.2.3-preview.1"); err != nil {
		t.Fatal(err)
	}
	instance, err := LoadInstance(lease.dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	actual, err := instance.Status(ctx)
	if err != nil || actual.Version != "1.2.3-preview.1" || actual.Token != "" {
		t.Fatalf("身份检查异常：%+v %v", actual, err)
	}
	wrong := instance
	wrong.Token = strings.Repeat("0", 64)
	if err := wrong.RequestQuit(ctx); err == nil {
		t.Fatal("错误凭证仍能退出后台")
	}
	if err := instance.RequestQuit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("有效退出请求未送达")
	}
	wrong = instance
	wrong.ID = "stale"
	if _, err := wrong.Status(ctx); err == nil {
		t.Fatal("错误身份被复用")
	}
}

func TestStartupCheckBlocksOperationsUntilAuthenticatedActivation(t *testing.T) {
	lease, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	lease.SetStartupPaused(true)
	server := httptest.NewServer(lease.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), func() {}, func() bool { return false }))
	defer server.Close()
	if err := lease.Publish(strings.TrimPrefix(server.URL, "http://"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(server.URL+"/api/v1/operations/execute", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatalf("未验收的候选版本接受了操作：%d", response.StatusCode)
	}
	if err := lease.Instance.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	response, err = http.Post(server.URL+"/api/v1/operations/execute", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("确认后没有开放：%d", response.StatusCode)
	}
}

func TestDataDirectoryCannotUseCodexOrSymlink(t *testing.T) {
	root := t.TempDir()
	if _, err := PrepareDirectory(filepath.Join(root, ".codex", "runtime")); err == nil {
		t.Fatal("运行文件落入 .codex")
	}
	if _, err := os.Stat(filepath.Join(root, ".codex")); !os.IsNotExist(err) {
		t.Fatal("检查创建了源目录")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err == nil {
		if _, err := PrepareDirectory(link); err == nil {
			t.Fatal("接受了符号链接数据目录")
		}
	}
}

func TestDaemonArgumentsAreIndependentAndReadOnlyByDefault(t *testing.T) {
	data := filepath.Join(t.TempDir(), "profile")
	args, err := DaemonArguments(LaunchOptions{PackageRoot: "/application/payload", DataDir: data, Address: "127.0.0.1:0"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\n")
	if strings.Contains(joined, "real-conversation-write") || strings.Contains(joined, "install-root") {
		t.Fatalf("默认扩大写权限或允许改写安装包：%v", args)
	}
	for _, expected := range []string{"archive.db", "workspaces.json", "github-settings.json", "lifecycle", "--desktop-state-dir"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("缺少独立持久化路径：%s", expected)
		}
	}
	if _, err := DaemonArguments(LaunchOptions{Address: "0.0.0.0:8787"}, Config{}); err == nil {
		t.Fatal("启动器允许远程控制接口")
	}
}
