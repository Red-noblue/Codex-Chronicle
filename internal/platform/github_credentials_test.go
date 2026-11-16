// 中文说明：验证系统凭证适配器的输入校验，不访问本机真实钥匙串或其他凭证库。
package platform

import (
	"context"
	"testing"
)

func TestSystemGitHubTokenStoreValidatesInputsWithoutTouchingCredentialStore(t *testing.T) {
	store, err := NewSystemGitHubTokenStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), ""); err == nil {
		t.Fatal("expected empty account rejection")
	}
	if err := store.Save(context.Background(), "", "token"); err == nil {
		t.Fatal("expected empty account rejection")
	}
	if err := store.Save(context.Background(), "account", "token\nwith-newline"); err == nil {
		t.Fatal("expected token newline rejection")
	}
	if err := store.Delete(context.Background(), ""); err == nil {
		t.Fatal("expected empty account rejection")
	}
	var nilStore *SystemGitHubTokenStore
	if _, err := nilStore.Load(context.Background(), "account"); err == nil {
		t.Fatal("expected nil store rejection")
	}
}

func TestNewSystemGitHubTokenStoreRejectsUnsafeServiceName(t *testing.T) {
	if _, err := NewSystemGitHubTokenStore("service\nname"); err == nil {
		t.Fatal("expected service name newline rejection")
	}
}
