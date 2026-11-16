// 中文说明：管理远程网页访问的一次性绑定码、会话令牌和最小访问审计；不读取或写入真实 Codex 数据。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	stateVersion       = 1
	defaultBindingTTL  = 10 * time.Minute
	defaultSessionTTL  = 30 * 24 * time.Hour
	maxAuditEntries    = 256
	maxBindingAttempts = 5
	passwordIterations = 120000
)

var (
	ErrAlreadyBound       = errors.New("远程访问已完成绑定")
	ErrInvalidBindingCode = errors.New("一次性绑定码无效或已过期")
	ErrRateLimited        = errors.New("认证尝试过于频繁，请稍后再试")
	ErrPasswordNotSet     = errors.New("尚未设置本地账户密码")
	ErrInvalidPassword    = errors.New("本地账户密码错误")
	ErrInvalidSession     = errors.New("会话令牌无效或已过期")
	ErrSessionNotFound    = errors.New("会话不存在")
	ErrInvalidDevice      = errors.New("设备名称无效")
	ErrBindingStillValid  = errors.New("当前绑定码仍在有效期内")
)

// Config 控制认证状态持久化和可注入的时间/随机源。
type Config struct {
	StatePath          string
	BindingTTL         time.Duration
	SessionTTL         time.Duration
	InitialBindingCode string
	Clock              func() time.Time
	Random             io.Reader
}

// BootstrapInfo 是启动时给本地操作者显示的一次性绑定码信息。
// Code 只在本次创建或重置时返回，不写入状态文件。
type BootstrapInfo struct {
	Code      string
	ExpiresAt time.Time
	Provided  bool
}

// BindRequest 是远程设备首次绑定请求；密码可选，空密码表示仅使用会话令牌。
type BindRequest struct {
	Code     string
	Device   string
	Password string
	Remote   string
}

// LoginRequest 是本地账户密码登录请求。
type LoginRequest struct {
	Device   string
	Password string
	Remote   string
}

// Session 是返回给 UI 的会话信息；Token 只在创建时返回。
type Session struct {
	ID        string    `json:"id"`
	Token     string    `json:"token,omitempty"`
	Device    string    `json:"device"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Principal 是已认证请求的最小身份投影。
type Principal struct {
	SessionID string    `json:"session_id"`
	Device    string    `json:"device"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SessionInfo 是本地管理页可见的设备会话摘要，不包含令牌。
type SessionInfo struct {
	ID        string    `json:"id"`
	Device    string    `json:"device"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
}

// Status 不包含绑定码、密码摘要或会话令牌。
type Status struct {
	Enabled            bool       `json:"enabled"`
	Bound              bool       `json:"bound"`
	PasswordConfigured bool       `json:"password_configured"`
	BindingExpiresAt   *time.Time `json:"binding_expires_at,omitempty"`
	ActiveSessionCount int        `json:"active_session_count"`
}

// AuditEvent 是不含凭证的最小访问审计记录。
type AuditEvent struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Result string    `json:"result"`
	Device string    `json:"device,omitempty"`
	Remote string    `json:"remote,omitempty"`
}

type persistedState struct {
	Version          int                `json:"version"`
	Bound            bool               `json:"bound"`
	BindingHash      string             `json:"binding_hash,omitempty"`
	BindingExpiresAt time.Time          `json:"binding_expires_at,omitempty"`
	PasswordSalt     string             `json:"password_salt,omitempty"`
	PasswordHash     string             `json:"password_hash,omitempty"`
	Sessions         []persistedSession `json:"sessions,omitempty"`
	Audit            []AuditEvent       `json:"audit,omitempty"`
}

type persistedSession struct {
	ID        string    `json:"id"`
	TokenHash string    `json:"token_hash"`
	Device    string    `json:"device"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
}

// Manager 是线程安全的本地认证控制面；StatePath 必须位于 .codex 外部。
type Manager struct {
	mu                  sync.Mutex
	config              Config
	state               persistedState
	volatile            bool
	initialCodeConsumed bool
	bindAttempts        int
	bindWindow          time.Time
}

// NewManager 加载或创建认证状态。未绑定时每次启动生成一个新的绑定码，旧码立即失效。
func NewManager(config Config) (*Manager, BootstrapInfo, error) {
	if config.BindingTTL <= 0 {
		config.BindingTTL = defaultBindingTTL
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = defaultSessionTTL
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	manager := &Manager{config: config, volatile: strings.TrimSpace(config.StatePath) == ""}
	if !manager.volatile {
		if err := manager.load(); err != nil {
			return nil, BootstrapInfo{}, err
		}
	}
	if manager.state.Version == 0 {
		manager.state.Version = stateVersion
		manager.state.Sessions = make([]persistedSession, 0)
		manager.state.Audit = make([]AuditEvent, 0)
	}
	if manager.state.Version != stateVersion {
		return nil, BootstrapInfo{}, fmt.Errorf("认证状态版本不受支持: %d", manager.state.Version)
	}
	manager.pruneExpiredLocked()
	if manager.state.Bound {
		if err := manager.saveLocked(); err != nil {
			return nil, BootstrapInfo{}, err
		}
		return manager, BootstrapInfo{}, nil
	}
	code, provided, err := manager.newBindingCode()
	if err != nil {
		return nil, BootstrapInfo{}, err
	}
	manager.state.BindingHash = digest(normalizeCode(code))
	manager.state.BindingExpiresAt = manager.now().Add(config.BindingTTL)
	if err := manager.saveLocked(); err != nil {
		return nil, BootstrapInfo{}, err
	}
	return manager, BootstrapInfo{Code: code, ExpiresAt: manager.state.BindingExpiresAt, Provided: provided}, nil
}

func (m *Manager) now() time.Time { return m.config.Clock().UTC() }

func (m *Manager) newBindingCode() (string, bool, error) {
	if provided := strings.TrimSpace(m.config.InitialBindingCode); provided != "" && !m.initialCodeConsumed {
		if len([]byte(provided)) < 16 {
			return "", false, fmt.Errorf("初始绑定码至少需要 16 字节")
		}
		m.initialCodeConsumed = true
		return normalizeCode(provided), true, nil
	}
	bytes := make([]byte, 20)
	if _, err := io.ReadFull(m.config.Random, bytes); err != nil {
		return "", false, fmt.Errorf("生成一次性绑定码失败: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(bytes)
	return groupCode(encoded), false, nil
}

// Bind 消费一次性绑定码并签发首个会话；成功后绑定码立即失效。
func (m *Manager) Bind(request BindRequest) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Bound {
		return Session{}, ErrAlreadyBound
	}
	if !m.allowAttemptLocked() {
		return Session{}, ErrRateLimited
	}
	provided := digest(normalizeCode(request.Code))
	if !secureEqual(provided, m.state.BindingHash) || !m.now().Before(m.state.BindingExpiresAt) {
		m.recordAuditLocked("bind", "failure", request.Device, request.Remote)
		_ = m.saveLocked()
		return Session{}, ErrInvalidBindingCode
	}
	device, err := validateDevice(request.Device)
	if err != nil {
		return Session{}, err
	}
	passwordSalt, passwordHash, err := passwordRecord(request.Password, m.config.Random)
	if err != nil {
		return Session{}, err
	}
	m.state.Bound = true
	m.state.BindingHash = ""
	m.state.BindingExpiresAt = time.Time{}
	m.state.PasswordSalt = passwordSalt
	m.state.PasswordHash = passwordHash
	session, persisted, err := m.newSessionLocked(device)
	if err != nil {
		return Session{}, err
	}
	m.state.Sessions = append(m.state.Sessions, persisted)
	m.recordAuditLocked("bind", "success", device, request.Remote)
	if err := m.saveLocked(); err != nil {
		return Session{}, err
	}
	return session, nil
}

// Login 使用绑定时设置的密码签发新会话。
func (m *Manager) Login(request LoginRequest) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Bound {
		return Session{}, ErrInvalidSession
	}
	if !m.allowAttemptLocked() {
		return Session{}, ErrRateLimited
	}
	device, err := validateDevice(request.Device)
	if err != nil {
		return Session{}, err
	}
	if m.state.PasswordHash == "" || !verifyPassword(request.Password, m.state.PasswordSalt, m.state.PasswordHash) {
		m.recordAuditLocked("login", "failure", device, request.Remote)
		_ = m.saveLocked()
		if m.state.PasswordHash == "" {
			return Session{}, ErrPasswordNotSet
		}
		return Session{}, ErrInvalidPassword
	}
	session, persisted, err := m.newSessionLocked(device)
	if err != nil {
		return Session{}, err
	}
	m.state.Sessions = append(m.state.Sessions, persisted)
	m.recordAuditLocked("login", "success", device, request.Remote)
	if err := m.saveLocked(); err != nil {
		return Session{}, err
	}
	return session, nil
}

// Authenticate 校验 Bearer 令牌，只返回非敏感身份信息。
func (m *Manager) Authenticate(token string) (Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	digestValue := digest(strings.TrimSpace(token))
	for index := range m.state.Sessions {
		session := &m.state.Sessions[index]
		if session.RevokedAt.IsZero() && now.Before(session.ExpiresAt) && secureEqual(digestValue, session.TokenHash) {
			return Principal{SessionID: session.ID, Device: session.Device, CreatedAt: session.CreatedAt, ExpiresAt: session.ExpiresAt}, nil
		}
	}
	return Principal{}, ErrInvalidSession
}

// RevokeToken 撤销当前会话，不需要把令牌写入审计或磁盘。
func (m *Manager) RevokeToken(token, remote string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	digestValue := digest(strings.TrimSpace(token))
	for index := range m.state.Sessions {
		session := &m.state.Sessions[index]
		if !session.RevokedAt.IsZero() || !secureEqual(digestValue, session.TokenHash) {
			continue
		}
		session.RevokedAt = m.now()
		m.recordAuditLocked("logout", "success", session.Device, remote)
		return m.saveLocked()
	}
	return ErrSessionNotFound
}

// RevokeSession 由本地管理页按会话 ID 撤销设备。
func (m *Manager) RevokeSession(sessionID, remote string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for index := range m.state.Sessions {
		session := &m.state.Sessions[index]
		if session.ID != strings.TrimSpace(sessionID) || !session.RevokedAt.IsZero() {
			continue
		}
		session.RevokedAt = m.now()
		m.recordAuditLocked("revoke", "success", session.Device, remote)
		return m.saveLocked()
	}
	return ErrSessionNotFound
}

// ResetBinding 仅供本地管理调用；它会撤销全部会话并创建新绑定码。
func (m *Manager) ResetBinding() (BootstrapInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	code, provided, err := m.newBindingCode()
	if err != nil {
		return BootstrapInfo{}, err
	}
	m.state.Bound = false
	m.state.PasswordSalt = ""
	m.state.PasswordHash = ""
	m.state.BindingHash = digest(normalizeCode(code))
	m.state.BindingExpiresAt = m.now().Add(m.config.BindingTTL)
	m.state.Sessions = make([]persistedSession, 0)
	m.recordAuditLocked("reset_binding", "success", "", "local")
	if err := m.saveLocked(); err != nil {
		return BootstrapInfo{}, err
	}
	return BootstrapInfo{Code: code, ExpiresAt: m.state.BindingExpiresAt, Provided: provided}, nil
}

// ReissueBinding 只在尚未绑定且旧绑定码已过期时重新生成绑定码，避免首次启动错过期限后只能手动删除状态文件。
func (m *Manager) ReissueBinding() (BootstrapInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Bound {
		return BootstrapInfo{}, ErrAlreadyBound
	}
	if m.now().Before(m.state.BindingExpiresAt) {
		return BootstrapInfo{}, ErrBindingStillValid
	}
	code, provided, err := m.newBindingCode()
	if err != nil {
		return BootstrapInfo{}, err
	}
	m.state.BindingHash = digest(normalizeCode(code))
	m.state.BindingExpiresAt = m.now().Add(m.config.BindingTTL)
	m.recordAuditLocked("reissue_binding", "success", "", "local")
	if err := m.saveLocked(); err != nil {
		return BootstrapInfo{}, err
	}
	return BootstrapInfo{Code: code, ExpiresAt: m.state.BindingExpiresAt, Provided: provided}, nil
}

// Status 返回当前认证状态，过期会话不会出现在计数中。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked()
	status := Status{Enabled: true, Bound: m.state.Bound, PasswordConfigured: m.state.PasswordHash != ""}
	if !m.state.Bound {
		expiresAt := m.state.BindingExpiresAt
		status.BindingExpiresAt = &expiresAt
	}
	for _, session := range m.state.Sessions {
		if session.RevokedAt.IsZero() && m.now().Before(session.ExpiresAt) {
			status.ActiveSessionCount++
		}
	}
	return status
}

// Audit 返回最近的最小审计记录，不返回任何凭证。
func (m *Manager) Audit(limit int) []AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > maxAuditEntries {
		limit = maxAuditEntries
	}
	start := len(m.state.Audit) - limit
	if start < 0 {
		start = 0
	}
	result := append([]AuditEvent(nil), m.state.Audit[start:]...)
	return result
}

// Sessions 返回设备会话摘要，供本地管理页撤销指定设备。
func (m *Manager) Sessions() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked()
	result := make([]SessionInfo, 0, len(m.state.Sessions))
	for _, session := range m.state.Sessions {
		result = append(result, SessionInfo{ID: session.ID, Device: session.Device, CreatedAt: session.CreatedAt, ExpiresAt: session.ExpiresAt, RevokedAt: session.RevokedAt})
	}
	return result
}

func (m *Manager) allowAttemptLocked() bool {
	now := m.now()
	if m.bindWindow.IsZero() || now.Sub(m.bindWindow) >= time.Minute {
		m.bindWindow = now
		m.bindAttempts = 0
	}
	if m.bindAttempts >= maxBindingAttempts {
		return false
	}
	m.bindAttempts++
	return true
}

func (m *Manager) newSessionLocked(device string) (Session, persistedSession, error) {
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(m.config.Random, bytes); err != nil {
		return Session{}, persistedSession{}, fmt.Errorf("生成会话令牌失败: %w", err)
	}
	token := hex.EncodeToString(bytes)
	idBytes := make([]byte, 12)
	if _, err := io.ReadFull(m.config.Random, idBytes); err != nil {
		return Session{}, persistedSession{}, fmt.Errorf("生成会话 ID 失败: %w", err)
	}
	created := m.now()
	expires := created.Add(m.config.SessionTTL)
	persisted := persistedSession{ID: "ses_" + hex.EncodeToString(idBytes), TokenHash: digest(token), Device: device, CreatedAt: created, ExpiresAt: expires}
	return Session{ID: persisted.ID, Token: token, Device: device, CreatedAt: created, ExpiresAt: expires}, persisted, nil
}

func (m *Manager) recordAuditLocked(action, result, device, remote string) {
	m.state.Audit = append(m.state.Audit, AuditEvent{At: m.now(), Action: action, Result: result, Device: safeAuditValue(device), Remote: safeAuditValue(remote)})
	if len(m.state.Audit) > maxAuditEntries {
		m.state.Audit = append([]AuditEvent(nil), m.state.Audit[len(m.state.Audit)-maxAuditEntries:]...)
	}
}

func safeAuditValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 256 {
		return value[:256]
	}
	return value
}

func validateDevice(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "未命名设备"
	}
	if len([]byte(value)) > 128 || strings.ContainsAny(value, "\x00\r\n") {
		return "", ErrInvalidDevice
	}
	return value, nil
}

func normalizeCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, " ", "")
	return value
}

func groupCode(value string) string {
	parts := make([]string, 0, (len(value)+3)/4)
	for len(value) > 0 {
		n := 4
		if len(value) < n {
			n = len(value)
		}
		parts = append(parts, value[:n])
		value = value[n:]
	}
	return strings.Join(parts, "-")
}

func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func secureEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func passwordRecord(password string, random io.Reader) (string, string, error) {
	if strings.TrimSpace(password) == "" {
		return "", "", nil
	}
	if len([]byte(password)) < 12 || len([]byte(password)) > 1024 {
		return "", "", fmt.Errorf("本地账户密码长度必须为 12 至 1024 字节")
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", "", fmt.Errorf("生成密码盐失败: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(salt), derivePassword(password, salt), nil
}

func verifyPassword(password, saltEncoded, expected string) bool {
	salt, err := base64.RawStdEncoding.DecodeString(saltEncoded)
	if err != nil || len(salt) == 0 {
		return false
	}
	return secureEqual(derivePassword(password, salt), expected)
}

func derivePassword(password string, salt []byte) string {
	// PBKDF2-HMAC-SHA256，避免引入 cgo 或平台相关密码库。
	block := make([]byte, 4)
	block[3] = 1
	hash := hmac.New(sha256.New, []byte(password))
	_, _ = hash.Write(salt)
	_, _ = hash.Write(block)
	u := hash.Sum(nil)
	result := append([]byte(nil), u...)
	for index := 1; index < passwordIterations; index++ {
		hash = hmac.New(sha256.New, []byte(password))
		_, _ = hash.Write(u)
		u = hash.Sum(nil)
		for offset := range result {
			result[offset] ^= u[offset]
		}
	}
	return hex.EncodeToString(result)
}

func (m *Manager) load() error {
	path := filepath.Clean(m.config.StatePath)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("创建认证状态目录失败: %w", err)
		}
		m.state = persistedState{Version: stateVersion, Sessions: make([]persistedSession, 0), Audit: make([]AuditEvent, 0)}
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取认证状态属性失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("认证状态文件必须是普通文件")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取认证状态失败: %w", err)
	}
	if err := json.Unmarshal(content, &m.state); err != nil {
		return fmt.Errorf("认证状态不是有效 JSON: %w", err)
	}
	if m.state.Sessions == nil {
		m.state.Sessions = make([]persistedSession, 0)
	}
	if m.state.Audit == nil {
		m.state.Audit = make([]AuditEvent, 0)
	}
	return nil
}

func (m *Manager) saveLocked() error {
	if m.volatile {
		return nil
	}
	path := filepath.Clean(m.config.StatePath)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("拒绝覆盖符号链接形式的认证状态")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查认证状态失败: %w", err)
	}
	content, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("编码认证状态失败: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".auth-state-*.tmp")
	if err != nil {
		return fmt.Errorf("创建认证状态临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("设置认证状态权限失败: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入认证状态失败: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("同步认证状态失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭认证状态失败: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("发布认证状态失败: %w", err)
	}
	return nil
}

func (m *Manager) pruneExpiredLocked() {
	now := m.now()
	active := m.state.Sessions[:0]
	for _, session := range m.state.Sessions {
		if !session.RevokedAt.IsZero() && now.Sub(session.RevokedAt) > 24*time.Hour {
			continue
		}
		if session.RevokedAt.IsZero() && now.After(session.ExpiresAt) {
			continue
		}
		active = append(active, session)
	}
	m.state.Sessions = active
}
