//go:build darwin

// 中文说明：默认以命令替身验证 macOS 凭证库；显式启用的原生测试仅创建并清理合成探针。
package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/archive"
)

func mockMacOSSecurity(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"state=\"$(dirname \"$0\")/state\"\n" +
		"args=\"$(dirname \"$0\")/args\"\n" +
		"printf '%s\\n' \"$@\" >> \"$args\"\n" +
		"case \"$1\" in\n" +
		"find-generic-password)\n" +
		"  if [ ! -f \"$state\" ]; then exit 44; fi\n" +
		"  cat \"$state\"\n" +
		"  ;;\n" +
		"-i)\n" +
		"  IFS= read -r input\n" +
		"  case \"$input\" in 'add-generic-password -U -s \"Codex Chronicle Test\" -a \"personal\" -X '*) ;; *) exit 2 ;; esac\n" +
		"  case \"$CHRONICLE_TEST_KEYCHAIN_MODE\" in\n" +
		"    drop) exit 0 ;;\n" +
		"    corrupt) printf 'incorrect' > \"$state\"; exit 0 ;;\n" +
		"    fail) exit 1 ;;\n" +
		"  esac\n" +
		"  printf '%s' \"${input##* -X }\" | /usr/bin/xxd -r -p > \"$state\"\n" +
		"  ;;\n" +
		"delete-generic-password)\n" +
		"  rm -f \"$state\"\n" +
		"  ;;\n" +
		"*) exit 2 ;;\n" +
		"esac\n"
	securityPath := filepath.Join(bin, "security")
	if err := os.WriteFile(securityPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

func TestSystemGitHubTokenStoreUsesSecurityWithoutTokenArgument(t *testing.T) {
	bin := mockMacOSSecurity(t)
	store, err := NewSystemGitHubTokenStore("Codex Chronicle Test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, "personal", "ghs-runtime-secret"); err != nil {
		t.Fatal(err)
	}
	token, err := store.Load(ctx, "personal")
	if err != nil || token != "ghs-runtime-secret" {
		t.Fatalf("unexpected keychain token: %q err=%v", token, err)
	}
	args, err := os.ReadFile(filepath.Join(bin, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "ghs-runtime-secret") {
		t.Fatalf("token appeared in security process arguments: %s", args)
	}
	if err := store.Delete(ctx, "personal"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "personal"); !errors.Is(err, archive.ErrGitHubCredentialNotFound) {
		t.Fatalf("expected missing keychain credential, got %v", err)
	}
}

func TestMacOSCredentialSaveDoesNotTrustCommandSuccess(t *testing.T) {
	for _, mode := range []string{"drop", "corrupt", "fail"} {
		t.Run(mode, func(t *testing.T) {
			mockMacOSSecurity(t)
			t.Setenv("CHRONICLE_TEST_KEYCHAIN_MODE", mode)
			store, err := NewSystemGitHubTokenStore("Codex Chronicle Test")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(t.Context(), "personal", "synthetic-secret"); err == nil {
				t.Fatal("未保存或读回不一致的凭证不能报告保存成功")
			}
		})
	}
}

// 仅在显式启用时验证真实系统命令，探针使用唯一服务名和合成密码，并在结束时清理。
func TestMacOSCredentialNativeRoundTrip(t *testing.T) {
	if os.Getenv("CHRONICLE_TEST_NATIVE_KEYCHAIN") != "1" {
		t.Skip("设置 CHRONICLE_TEST_NATIVE_KEYCHAIN=1 后验证原生钥匙串")
	}
	store, err := NewSystemGitHubTokenStore(fmt.Sprintf("Codex Chronicle Probe %d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	const account = `isolated "probe" \ account`
	t.Cleanup(func() {
		if err := store.Delete(context.Background(), account); err != nil {
			t.Error("清理本次钥匙串探针失败")
		}
	})
	for _, token := range []string{"synthetic_keychain_probe", `synthetic "quotes" \ $() ; probe`} {
		if err := store.Save(t.Context(), account, token); err != nil {
			t.Fatal(err)
		}
		loaded, err := store.Load(t.Context(), account)
		if err != nil || loaded != token {
			t.Fatal("原生钥匙串未能正确保存和读回合成凭证")
		}
	}
}
