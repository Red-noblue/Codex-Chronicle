// 中文说明：管理安装版的单实例锁、私有控制凭证及健康探测；不扫描或修改 Codex 对话。
package desktop

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var ErrAlreadyRunning = errors.New("此用户数据目录已有后台实例")

const controlPath = "/internal/desktop"

// Instance 的 Token 仅存入权限受限的本机文件，禁止返回给网页或输出到日志。
type Instance struct {
	URL           string `json:"url"`
	Version       string `json:"version"`
	PID           int    `json:"pid"`
	ID            string `json:"id"`
	Token         string `json:"control_token,omitempty"`
	Stopping      bool   `json:"stopping,omitempty"`
	StartupPaused bool   `json:"startup_paused,omitempty"`
}

type Lease struct {
	file     *os.File
	dir      string
	Instance Instance
	paused   atomic.Bool
}

func (l *Lease) SetStartupPaused(paused bool) { l.paused.Store(paused) }

// PrepareDirectory 拒绝将运行数据放入 .codex，也不沿符号链接创建控制文件。
func PrepareDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil || strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("用户数据目录无效")
	}
	for current := abs; ; current = filepath.Dir(current) {
		if strings.EqualFold(filepath.Base(current), ".codex") {
			return "", fmt.Errorf("用户数据必须位于 .codex 外部")
		}
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 && current != abs {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			relative, err := filepath.Rel(current, abs)
			if err != nil {
				return "", err
			}
			return PrepareDirectory(filepath.Join(resolved, relative))
		}
		if statErr == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return "", fmt.Errorf("用户数据路径包含非目录或符号链接：%s", current)
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", statErr
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return abs, nil
}

// Acquire 在任何数据库打开前取得内核文件锁；进程意外退出时由操作系统释放。
func Acquire(dir string) (*Lease, error) {
	abs, err := PrepareDirectory(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(abs, "instance.lock")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("实例锁不是普通文件")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockInstance(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		_ = file.Close()
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Lease{file: file, dir: abs, Instance: Instance{PID: os.Getpid(), ID: hex.EncodeToString(id), Token: hex.EncodeToString(secret)}}, nil
}

func (l *Lease) Publish(address, version string) error {
	l.Instance.URL, l.Instance.Version = "http://"+address, version
	l.Instance.StartupPaused = l.paused.Load()
	data, err := json.Marshal(l.Instance)
	if err != nil {
		return err
	}
	path := filepath.Join(l.dir, "instance.json")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("实例记录不是普通文件")
	}
	file, err := os.CreateTemp(l.dir, ".instance-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// 锁文件仍锁住本实例；Windows 不支持覆盖已存在的 JSON 文件，先清理自己的旧实例记录。
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (l *Lease) Close() error {
	_ = os.Remove(filepath.Join(l.dir, "instance.json"))
	return l.file.Close()
}

// Handler 的控制入口只接受私有凭证；网页和其他占用同一端口的服务不能被误认或退出。
func (l *Lease) Handler(next http.Handler, stop func(), stopping func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != controlPath && r.URL.Path != controlPath+"/quit" && r.URL.Path != controlPath+"/activate" {
			if l.paused.Load() && r.URL.Path != "/healthz" && r.URL.Path != "/api/v1/health/preflight" {
				http.Error(w, "安装检查尚未完成，暂不接受操作", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() ||
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+l.Instance.Token)) != 1 {
			http.Error(w, "需要有效的本机控制凭证", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == controlPath+"/quit" {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			stop()
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.URL.Path == controlPath+"/activate" {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			l.paused.Store(false)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		info := l.Instance
		info.Token, info.Stopping = "", stopping()
		info.StartupPaused = l.paused.Load()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})
}

func LoadInstance(dir string) (Instance, error) {
	path := filepath.Join(dir, "instance.json")
	info, err := os.Lstat(path)
	if err != nil {
		return Instance{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return Instance{}, fmt.Errorf("实例记录异常")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Instance{}, err
	}
	var instance Instance
	if err := json.Unmarshal(data, &instance); err != nil {
		return instance, err
	}
	if _, err := instance.controlURL(""); err != nil {
		return Instance{}, err
	}
	return instance, nil
}

func (i Instance) controlURL(suffix string) (string, error) {
	parsed, err := url.Parse(i.URL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" ||
		net.ParseIP(parsed.Hostname()) == nil || !net.ParseIP(parsed.Hostname()).IsLoopback() || parsed.Port() == "" || len(i.Token) != 64 || i.ID == "" {
		return "", fmt.Errorf("实例地址或身份无效")
	}
	return i.URL + controlPath + suffix, nil
}

func (i Instance) request(ctx context.Context, method, suffix string) (*http.Response, error) {
	address, err := i.controlURL(suffix)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+i.Token)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	return client.Do(r)
}

func (i Instance) Status(ctx context.Context) (Instance, error) {
	response, err := i.request(ctx, http.MethodGet, "")
	if err != nil {
		return Instance{}, err
	}
	defer response.Body.Close()
	var actual Instance
	if response.StatusCode != http.StatusOK {
		return actual, fmt.Errorf("实例认证失败：%d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&actual); err != nil {
		return actual, err
	}
	if actual.ID != i.ID || actual.PID != i.PID || actual.Version != i.Version || actual.URL != i.URL {
		return actual, fmt.Errorf("实例身份已变化")
	}
	return actual, nil
}

func (i Instance) RequestQuit(ctx context.Context) error {
	response, err := i.request(ctx, http.MethodPost, "/quit")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("后台拒绝退出请求：%d", response.StatusCode)
	}
	return nil
}

func (i Instance) Activate(ctx context.Context) error {
	response, err := i.request(ctx, http.MethodPost, "/activate")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("后台拒绝开放：%d", response.StatusCode)
	}
	return nil
}

// CheckHealth 核验实际候选进程、项目数据库迁移与兼容层，不使用旧服务进程的检查结果代替。
func (i Instance) CheckHealth(ctx context.Context) error {
	if _, err := i.controlURL(""); err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for _, path := range []string{"/healthz", "/api/v1/health/preflight"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, i.URL+path, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		var check struct {
			Status  string `json:"status"`
			Version string `json:"version"`
			Blocked bool   `json:"blocked"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&check)
		_ = response.Body.Close()
		if decodeErr != nil || response.StatusCode != 200 {
			return fmt.Errorf("候选健康检查失败：%s", path)
		}
		if path == "/healthz" && (check.Status != "ok" || check.Version != i.Version) {
			return fmt.Errorf("健康检查版本与实际进程不一致")
		}
		if path != "/healthz" && (check.Blocked || check.Status != "ready") {
			return fmt.Errorf("候选数据库或兼容层预检未通过")
		}
	}
	return nil
}
