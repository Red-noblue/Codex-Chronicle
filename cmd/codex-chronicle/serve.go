// 中文说明：提供安装版私有控制通道和优雅退出；先等待数据任务完成，再关闭 HTTP 和数据库。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codex-chronicle.local/internal/api"
	"codex-chronicle.local/internal/desktop"
)

func portUnavailable(err error) bool {
	// Windows Winsock 使用 10048（占用）和 10013（系统保留），不等同于 Go 的 Unix errno。
	return errors.Is(err, syscall.EADDRINUSE) || runtime.GOOS == "windows" &&
		(errors.Is(err, syscall.Errno(10048)) || errors.Is(err, syscall.Errno(10013)))
}

// listen 直接绑定并持有 socket；只在安装版自动模式遇到端口占用时让系统选择空闲端口。
func listen(address string, autoPort bool) (net.Listener, error) {
	listener, err := net.Listen("tcp", address)
	if autoPort && portUnavailable(err) {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			return nil, fmt.Errorf("自动端口仅支持本机回环 IP：%s", address)
		}
		listener, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err == nil {
			log.Printf("%s 已被占用或不可用，工作台改用 %s", address, listener.Addr())
		}
	}
	if portUnavailable(err) {
		return nil, fmt.Errorf("监听地址 %s 已被占用或不可用；显式指定的端口不会自动切换，关闭自动回退时同样如此；请在端口设置中修改，或更换 --address：%w", address, err)
	}
	return listener, err
}

func serve(address string, handler http.Handler, server *api.Server, lease *desktop.Lease, autoPort bool) error {
	listener, err := listen(address, autoPort && lease != nil)
	if err != nil {
		return err
	}
	defer listener.Close()
	stop := make(chan struct{})
	var once sync.Once
	var stopping atomic.Bool
	requestStop := func() { once.Do(func() { stopping.Store(true); close(stop) }) }
	if lease != nil {
		if err := lease.Publish(listener.Addr().String(), version); err != nil {
			return err
		}
		if autoPort {
			if err := lease.RememberAddress(); err != nil {
				return err
			}
		}
		handler = lease.Handler(handler, requestStop, stopping.Load)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveError := make(chan error, 1)
	go func() { serveError <- httpServer.Serve(listener) }()
	updateCtx, cancelUpdate := context.WithCancel(context.Background())
	updateDone := make(chan struct{})
	go func() {
		defer close(updateDone)
		if lease != nil {
			server.RunAutomaticUpdates(updateCtx, lease.StartupPaused)
		}
	}()
	defer func() { cancelUpdate(); <-updateDone }()
	signalCtx, cancelSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelSignal()
	log.Printf("Codex Chronicle %s 正在监听 %s", version, listener.Addr())
	select {
	case err := <-serveError:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-signalCtx.Done():
		requestStop()
	case <-stop:
	}
	log.Print("正在等待已有数据任务完成；不会强杀任务")
	cancelUpdate()
	<-updateDone
	if err := server.Drain(context.Background()); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		// 数据任务已完成，剩余长连接仅用于阅读或订阅状态。
		return httpServer.Close()
	}
	return nil
}
