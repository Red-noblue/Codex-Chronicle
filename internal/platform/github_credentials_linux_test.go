//go:build linux

// 中文说明：使用隔离的 secret-tool 命令替身验证 Linux Secret Service 适配器。
package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/archive"
)

func TestSystemGitHubTokenStoreUsesSecretToolWithoutTokenArgument(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"state=\"$(dirname \"$0\")/state\"\n" +
		"args=\"$(dirname \"$0\")/args\"\n" +
		"printf '%s\\n' \"$@\" > \"$args\"\n" +
		"case \"$1\" in\n" +
		"lookup)\n" +
		"  if [ ! -f \"$state\" ]; then exit 1; fi\n" +
		"  cat \"$state\"\n" +
		"  ;;\n" +
		"store)\n" +
		"  cat > \"$state\"\n" +
		"  ;;\n" +
		"clear)\n" +
		"  rm -f \"$state\"\n" +
		"  ;;\n" +
		"*) exit 2 ;;\n" +
		"esac\n"
	secretToolPath := filepath.Join(bin, "secret-tool")
	if err := os.WriteFile(secretToolPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
		t.Fatalf("unexpected Secret Service token: %q err=%v", token, err)
	}
	args, err := os.ReadFile(filepath.Join(bin, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "ghs-runtime-secret") {
		t.Fatalf("token appeared in secret-tool process arguments: %s", args)
	}
	if err := store.Delete(ctx, "personal"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "personal"); !errors.Is(err, archive.ErrGitHubCredentialNotFound) {
		t.Fatalf("expected missing Secret Service credential, got %v", err)
	}
}
