// 中文说明：提供安装版私有控制通道和优雅退出；先等待数据任务完成，再关闭 HTTP 和数据库。
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codex-chronicle.local/internal/api"
	"codex-chronicle.local/internal/desktop"
)

func serve(address string, handler http.Handler, server *api.Server, lease *desktop.Lease) error {
	listener, err := net.Listen("tcp", address)
	if err != nil && lease != nil && errors.Is(err, syscall.EADDRINUSE) {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
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
		handler = lease.Handler(handler, requestStop, stopping.Load)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveError := make(chan error, 1)
	go func() { serveError <- httpServer.Serve(listener) }()
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
