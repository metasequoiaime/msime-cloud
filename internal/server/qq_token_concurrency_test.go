package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type qqTokenResult struct {
	token string
	err   error
}

type qqTokenWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *qqTokenWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// blockedQQTokenRefresh 让真实 HTTP 换证请求停在响应前，测试通过通道控制结束。
func blockedQQTokenRefresh(t *testing.T, ctx context.Context) (*qqBot, func(), <-chan qqTokenResult, *atomic.Int32) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var started, released sync.Once
	calls := new(atomic.Int32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started.Do(func() { close(entered) })
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"access_token":"synthetic-token","expires_in":7200}`))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	unblock := func() { released.Do(func() { close(release) }) }
	b := &qqBot{config: QQBotConfig{APIURL: upstream.URL, AppID: "1", secret: "synthetic-secret"}, now: time.Now, client: upstream.Client()}
	b.client.Timeout = 5 * time.Second
	result := make(chan qqTokenResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		token, err := b.accessToken(ctx)
		result <- qqTokenResult{token, err}
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("换证调用没有结束")
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("换证请求未到达上游")
	}
	return b, unblock, result, calls
}

func TestQQTokenRefreshWaiterCancellation(t *testing.T) {
	b, unblock, _, _ := blockedQQTokenRefresh(t, t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiter := &qqTokenWaitContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := b.accessToken(waiter); done <- err }()
	// Done 在进入等待的 select 时求值，确保取消没有只命中入口检查。
	select {
	case <-waiter.waiting:
	case <-time.After(time.Second):
		cancel()
		unblock()
		<-done
		t.Fatal("调用者未进入共享换证的等待")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("等待换证的取消结果=%v", err)
		}
	case <-time.After(time.Second):
		unblock()
		<-done
		t.Fatal("等待换证时未响应调用者取消")
	}
}

func TestQQTokenRefreshDoesNotBlockEventProbe(t *testing.T) {
	b, unblock, _, _ := blockedQQTokenRefresh(t, t.Context())
	done := make(chan struct{})
	go func() { b.probeEventFields(json.RawMessage(`{"content":"合成消息"}`)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		unblock()
		<-done
		t.Fatal("凭证刷新阻塞了回调事件字段探测")
	}
}

func TestQQTokenRefreshSurvivesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b, unblock, first, calls := blockedQQTokenRefresh(t, ctx)
	cancel()
	select {
	case result := <-first:
		if !errors.Is(result.err, context.Canceled) {
			t.Errorf("发起换证的取消结果=%v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("发起换证的调用者取消后没有返回")
	}
	// 第一个调用者已退出，其他调用者仍能使用同一次换证，随后缓存也应命中。
	unblock()
	for range 2 {
		token, err := b.accessToken(t.Context())
		if err != nil || token != "synthetic-token" {
			t.Fatalf("共享换证结果=%q, %v", token, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("取消发起者后重复换证 %d 次，预期只请求一次", got)
	}
}
