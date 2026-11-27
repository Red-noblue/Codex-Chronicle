// 中文说明：核验固定登录入口的原子更新、信任根保护、旧登录项识别和篡改拒绝；所有产物使用临时目录。
package desktop

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	updatepkg "codex-chronicle.local/internal/update"
)

func loginFixture(t *testing.T) (string, func(string, string) LaunchOptions) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data, err := PrepareDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updatepkg.NewManager(filepath.Join(data, "updates")); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return data, func(version, body string) LaunchOptions {
		t.Helper()
		root := t.TempDir()
		for _, name := range []string{"bin", "update"} {
			if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		files := map[string]string{
			filepath.Join("bin", ExecutableName("codex-chronicle-desktop")): body,
			filepath.Join("bin", ExecutableName("codex-chronicle")):         version,
			filepath.Join("update", "trusted-public-key"):                   base64.StdEncoding.EncodeToString(public) + "\n",
		}
		for name, text := range files {
			if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		manifest, err := updatepkg.BuildManifest(t.Context(), root, version, "")
		if err != nil {
			t.Fatal(err)
		}
		manifest, err = updatepkg.SignManifest(manifest, private, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := updatepkg.WriteManifest(t.Context(), root, manifest); err != nil {
			t.Fatal(err)
		}
		return LaunchOptions{DataDir: data, PackageRoot: root, ExpectedVersion: version}
	}
}

func TestPrepareLoginLauncherReplacesOnlyVerifiedBytes(t *testing.T) {
	data, bundle := loginFixture(t)
	first := bundle("1.0.0", "原来的签名启动器")
	if err := PrepareLoginLauncher(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	target := loginLauncherPath(data)
	entry, root := LoginEntry("original", data, first.PackageRoot)
	if entry != target || root != "" || pathsOverlap(filepath.Join(data, "updates"), target) {
		t.Fatalf("登录入口仍然依赖版本清理目录：%s %s", entry, root)
	}
	next := bundle("2.0.0", "新的签名启动器")
	if err := os.WriteFile(filepath.Join(next.PackageRoot, "bin", ExecutableName("codex-chronicle-desktop")), []byte("被篡改的启动器"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareLoginLauncher(t.Context(), next); err == nil {
		t.Fatal("接受了被篡改的登录启动器")
	}
	current, _ := os.ReadFile(target)
	if string(current) != "原来的签名启动器" {
		t.Fatal("失败的复制覆盖了原来的有效入口")
	}
	valid := bundle("2.0.0", "新的签名启动器")
	if err := PrepareLoginLauncher(t.Context(), valid); err != nil {
		t.Fatal(err)
	}
	current, _ = os.ReadFile(target)
	if string(current) != "新的签名启动器" {
		t.Fatal("有效的新启动器没有原子替换旧入口")
	}
	if err := os.RemoveAll(first.PackageRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("清理旧载荷影响了固定入口：%v", err)
	}
	if got := loginArguments(target, data, "unused-version"); !reflect.DeepEqual(got, []string{target, "--background", "--data-dir", data, "--launch-active"}) {
		t.Fatalf("固定入口的登录命令携带了旧版本路径：%v", got)
	}
}

func TestLoginLauncherRejectsChangedTrustAndWrongVersion(t *testing.T) {
	data, bundle := loginFixture(t)
	options := bundle("1.0.0", "可信启动器")
	wrong := options
	wrong.ExpectedVersion = "2.0.0"
	if err := PrepareLoginLauncher(t.Context(), wrong); err == nil {
		t.Fatal("接受了错误版本的入口")
	}
	if err := PrepareLoginLauncher(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	_, other := loginFixture(t)
	untrusted := other("2.0.0", "其他签名身份")
	untrusted.DataDir = data
	if err := PrepareLoginLauncher(t.Context(), untrusted); err == nil {
		t.Fatal("静默替换了已有的登录信任根")
	}
	if err := LaunchActiveFromLogin(t.Context(), "wrong-executable", data, true); err == nil {
		t.Fatal("接受了其他位置的登录入口")
	}
	if err := LaunchActiveFromLogin(t.Context(), loginLauncherPath(data), data, true); err == nil {
		t.Fatal("没有活动版本时仍然执行了入口")
	}
	if runtime.GOOS == "darwin" && strings.HasPrefix(data, "/private/var/") {
		aliasData := strings.TrimPrefix(data, "/private")
		aliasEntry := strings.TrimPrefix(loginLauncherPath(data), "/private")
		if err := LaunchActiveFromLogin(t.Context(), aliasEntry, aliasData, true); err == nil || !strings.Contains(err.Error(), "尚无可用的活动版本") {
			t.Fatalf("把 macOS /var 和 /private/var 的同一入口误判为不同安装：%v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := PrepareLoginLauncher(ctx, options); err == nil {
		t.Fatal("取消后继续复制登录入口")
	}
}

func TestLegacyLoginMatchingRequiresWholeOwnedCommand(t *testing.T) {
	data := t.TempDir()
	old := filepath.Join(data, "updates", "versions", "0.1.0-preview.19")
	render := func(executable, root string) (string, error) {
		return executable + "|" + data + "|" + root, nil
	}
	saved, _ := render(filepath.Join(old, "bin", ExecutableName("codex-chronicle-desktop")), old)
	for _, input := range []struct {
		text string
		want bool
	}{{saved, true}, {saved + "|--extra", false}, {"other|" + saved, false}} {
		got, err := matchesLoginEntry(input.text, data, render, loginLauncherPath(data), "")
		if err != nil || got != input.want {
			t.Fatalf("旧登录项匹配不正确：%v %v", got, err)
		}
	}
}
