package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readWaitContext 在调用者开始等待结果时通知测试，不用休眠猜测并发执行顺序。
type readWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *readWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func awaitRead(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(5 * time.Second):
		t.Fatal("等待并发读取事件超时")
	}
}

type readResult struct {
	response Response
	err      error
}

func startRead(c Client, ctx context.Context, path string) <-chan readResult {
	result := make(chan readResult, 1)
	go func() {
		r, err := c.Get(ctx, "synthetic-token", path)
		result <- readResult{r, err}
	}()
	return result
}

func finishRead(t *testing.T, result <-chan readResult) readResult {
	t.Helper()
	select {
	case r := <-result:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("等待读取结果超时")
		return readResult{}
	}
}

func TestGetSharesConcurrentReads(t *testing.T) {
	for _, scenario := range []string{"冷缓存", "过期缓存", "上游错误"} {
		t.Run(scenario, func(t *testing.T) {
			const callers = 8
			var calls atomic.Int64
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if scenario == "过期缓存" && n == 1 {
					w.Header().Set("ETag", `"v1"`)
					_, _ = w.Write([]byte("cached-body"))
					return
				}
				first := int64(1)
				if scenario == "过期缓存" {
					first++
				}
				if n == first {
					close(entered)
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if scenario == "过期缓存" {
					if r.Header.Get("If-None-Match") != `"v1"` {
						t.Error("过期读取没有携带 ETag")
					}
					w.WriteHeader(http.StatusNotModified)
				} else if scenario == "上游错误" && n == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
				} else {
					_, _ = w.Write([]byte("cached-body"))
				}
			}))
			t.Cleanup(upstream.Close)
			c := Client{APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &Cache{}, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
			if scenario == "过期缓存" {
				if r, err := c.Get(t.Context(), "synthetic-token", "/repos/example/dictionary/releases"); err != nil || !r.OK() {
					t.Fatal(r.Status, err)
				}
				clock.Add(int64(ReadTTL))
			}
			results := []<-chan readResult{startRead(c, t.Context(), "/repos/example/dictionary/releases")}
			awaitRead(t, entered)
			for range callers - 1 {
				ctx := &readWaitContext{Context: t.Context(), waiting: make(chan struct{})}
				results = append(results, startRead(c, ctx, "/repos/example/dictionary/releases"))
				awaitRead(t, ctx.waiting)
			}
			unblock()
			for _, result := range results {
				r := finishRead(t, result)
				if r.err != nil {
					t.Fatal(r.err)
				}
				if scenario == "上游错误" {
					if r.response.Status != http.StatusServiceUnavailable {
						t.Fatal("并发调用没有共享上游错误", r.response.Status)
					}
				} else if !r.response.OK() || string(r.response.Body) != "cached-body" {
					t.Fatal(r.response.Status, string(r.response.Body))
				}
			}
			want := int64(1)
			if scenario == "过期缓存" {
				want++
			}
			if calls.Load() != want {
				t.Fatalf("8 个并发读取触发 %d 次上游调用，期望 %d 次", calls.Load(), want)
			}
			if r, err := c.Get(t.Context(), "synthetic-token", "/repos/example/dictionary/releases"); err != nil || !r.OK() {
				t.Fatal(r.Status, err)
			}
			if scenario == "上游错误" {
				want++
			}
			if calls.Load() != want {
				t.Fatal("成功响应未缓存或错误响应被缓存", calls.Load(), want)
			}
		})
	}
}

func TestGetSharedReadSurvivesCallerCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			_, _ = w.Write([]byte("shared-body"))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	c := Client{APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &Cache{}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := startRead(c, ctx, "/repos/example/dictionary/releases")
	awaitRead(t, entered)
	follower := &readWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	second := startRead(c, follower, "/repos/example/dictionary/releases")
	awaitRead(t, follower.waiting)
	cancel()
	if r := finishRead(t, first); !errors.Is(r.err, context.Canceled) {
		t.Fatal("取消的调用者没有停止等待", r.err)
	}
	unblock()
	if r := finishRead(t, second); r.err != nil || !r.response.OK() || string(r.response.Body) != "shared-body" {
		t.Fatal("单个调用者取消影响了其他读取", r.response.Status, r.err)
	}
	if calls.Load() != 1 {
		t.Fatal("取消后重复访问上游", calls.Load())
	}
}

func TestGetInvalidationSeparatesInFlightReads(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
				_, _ = w.Write([]byte("before-write"))
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte("after-write"))
	}))
	t.Cleanup(upstream.Close)
	c := Client{APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &Cache{}}
	path := "/repos/example/dictionary/releases"
	old := startRead(c, t.Context(), path)
	awaitRead(t, entered)
	c.Invalidate("/repos/example/dictionary/")
	if r := finishRead(t, startRead(c, t.Context(), path)); r.err != nil || string(r.response.Body) != "after-write" {
		t.Fatal("失效后的读取复用了旧请求", r.response.Status, r.err)
	}
	unblock()
	if r := finishRead(t, old); r.err != nil || string(r.response.Body) != "before-write" {
		t.Fatal(r.response.Status, r.err)
	}
	if r, err := c.Get(t.Context(), "synthetic-token", path); err != nil || string(r.Body) != "after-write" || calls.Load() != 2 {
		t.Fatal("旧请求覆盖了新缓存", string(r.Body), calls.Load(), err)
	}
}
