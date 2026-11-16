// 中文说明：验证一次性绑定码、会话令牌、持久化和审计边界，不接触真实 Codex 目录。
package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type authTestClock struct{ now time.Time }

func (clock *authTestClock) Now() time.Time { return clock.now }

func (clock *authTestClock) Advance(duration time.Duration) { clock.now = clock.now.Add(duration) }

func TestManagerBindsOnceAuthenticatesAndPersistsOnlyHashes(t *testing.T) {
	clock := &authTestClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	statePath := filepath.Join(t.TempDir(), "auth", "state.json")
	manager, bootstrap, err := NewManager(Config{StatePath: statePath, Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.Code == "" || bootstrap.ExpiresAt.IsZero() || bootstrap.Provided {
		t.Fatalf("unexpected generated bootstrap: %+v", bootstrap)
	}
	password := "correct horse battery staple"
	session, err := manager.Bind(BindRequest{Code: bootstrap.Code, Device: "浏览器-一", Password: password, Remote: "192.0.2.5:4000"})
	if err != nil {
		t.Fatal(err)
	}
	if session.Token == "" || session.ID == "" || session.Device != "浏览器-一" {
		t.Fatalf("unexpected session: %+v", session)
	}
	if _, err := manager.Bind(BindRequest{Code: bootstrap.Code, Device: "重复设备"}); !errors.Is(err, ErrAlreadyBound) {
		t.Fatalf("binding code should be single use after bind: %v", err)
	}
	principal, err := manager.Authenticate(session.Token)
	if err != nil || principal.SessionID != session.ID {
		t.Fatalf("session authentication failed: principal=%+v err=%v", principal, err)
	}
	login, err := manager.Login(LoginRequest{Device: "第二浏览器", Password: password, Remote: "192.0.2.5:4000"})
	if err != nil {
		t.Fatal(err)
	}
	if login.Token == "" || login.ID == session.ID {
		t.Fatalf("login did not issue a new session: %+v", login)
	}
	if err := manager.RevokeToken(session.Token, "192.0.2.5:4000"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Authenticate(session.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoked token was accepted: %v", err)
	}

	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, bootstrap.Code) || strings.Contains(text, session.Token) || strings.Contains(text, password) {
		t.Fatalf("credential leaked into persisted auth state: %s", text)
	}
	var persisted persistedState
	if err := json.Unmarshal(content, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.PasswordHash == "" || persisted.PasswordSalt == "" || persisted.Sessions[0].TokenHash == "" {
		t.Fatalf("persisted state did not contain expected hashes: %+v", persisted)
	}

	restarted, _, err := NewManager(Config{StatePath: statePath, Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	restartedPrincipal, err := restarted.Authenticate(login.Token)
	if err != nil || restartedPrincipal.SessionID != login.ID {
		t.Fatalf("persisted session did not survive restart: principal=%+v err=%v", restartedPrincipal, err)
	}
	status := restarted.Status()
	if !status.Bound || !status.PasswordConfigured || status.ActiveSessionCount != 1 {
		t.Fatalf("unexpected auth status: %+v", status)
	}
	audit := restarted.Audit(20)
	if len(audit) < 3 || audit[len(audit)-1].Action != "logout" {
		t.Fatalf("audit did not preserve lifecycle: %+v", audit)
	}
}

func TestManagerExpiryRateLimitAndResetBinding(t *testing.T) {
	clock := &authTestClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	manager, bootstrap, err := NewManager(Config{Clock: clock.Now, BindingTTL: time.Minute, SessionTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxBindingAttempts; index++ {
		if _, err := manager.Bind(BindRequest{Code: "wrong-code", Device: "test"}); !errors.Is(err, ErrInvalidBindingCode) {
			t.Fatalf("invalid binding attempt %d returned %v", index, err)
		}
	}
	if _, err := manager.Bind(BindRequest{Code: "wrong-code", Device: "test"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("sixth binding attempt should be rate limited: %v", err)
	}
	clock.Advance(time.Minute)
	if _, err := manager.Bind(BindRequest{Code: bootstrap.Code, Device: "expired"}); !errors.Is(err, ErrInvalidBindingCode) {
		t.Fatalf("expired binding code was accepted: %v", err)
	}
	if _, err := manager.ReissueBinding(); err != nil {
		t.Fatal(err)
	}
	newBootstrap, err := manager.ResetBinding()
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Bind(BindRequest{Code: newBootstrap.Code, Device: "test-device"})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, err := manager.Authenticate(session.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired session was accepted: %v", err)
	}
}

func TestManagerRejectsSymlinkState(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target.json")
	link := filepath.Join(base, "state.json")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前平台不支持符号链接: %v", err)
	}
	if _, _, err := NewManager(Config{StatePath: link}); err == nil {
		t.Fatal("认证状态不应跟随符号链接")
	}
}
