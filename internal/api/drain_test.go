// 中文说明：验证退出时拒绝新工作，并等待已经接收的请求、会话执行和归档任务，不靠强制中断退出。
package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDrainWaitsForAcceptedRequestsAndExecution(t *testing.T) {
	server := NewServer("")
	defer server.Close()
	entered, finish := make(chan struct{}), make(chan struct{})
	handler := server.drainMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-finish
		server.executionMu.Lock()
		server.executionActive = "accepted-background-task"
		server.executionMu.Unlock()
	}))
	requestDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/work", nil))
		close(requestDone)
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- server.Drain(ctx) }()
	for {
		server.drainMu.Lock()
		stopping := server.draining
		server.drainMu.Unlock()
		if stopping {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("退出没有开始")
		case <-time.After(time.Millisecond):
		}
	}
	newRequest := httptest.NewRecorder()
	handler.ServeHTTP(newRequest, httptest.NewRequest("POST", "/work", nil))
	if newRequest.Code != 503 {
		t.Fatalf("退出期间仍接受新任务：%d", newRequest.Code)
	}
	close(finish)
	<-requestDone
	select {
	case err := <-drained:
		t.Fatalf("后台操作尚未完成却退出：%v", err)
	default:
	}
	server.executionMu.Lock()
	server.executionActive = ""
	server.executionMu.Unlock()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
}

func TestDrainTimeoutDoesNotCancelCloudTask(t *testing.T) {
	server := NewServer("")
	defer server.Close()
	server.cloud = &cloudBackupState{active: "archive-task"}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := server.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应等待归档：%v", err)
	}
	if server.cloud.active != "archive-task" {
		t.Fatal("退出检查取消了归档任务")
	}
	server.cloud.active = ""
	if err := server.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}
