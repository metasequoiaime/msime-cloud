package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQQRoleQueryDoesNotOverwriteNewerEvent(t *testing.T) {
	for _, eventRole := range []string{"member", "admin", "owner"} {
		t.Run(eventRole, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started, proceed := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(proceed) })
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-proceed:
				case <-r.Context().Done():
					return
				}
				oldRole := "admin"
				if eventRole != "member" {
					oldRole = "member"
				}
				fmt.Fprintf(w, `{"member_role":%q}`, oldRole)
			}))
			defer upstream.Close()
			defer release()
			s := qqRoleOrderServer(upstream)
			result := make(chan bool, 1)
			go func() { result <- s.qqIsAdmin(ctx, "GROUP1", "MEMBER1", "") }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("身份查询未开始")
			}
			want := eventRole != "member"
			if got := s.qqIsAdmin(ctx, "GROUP1", "MEMBER1", eventRole); got != want {
				t.Fatalf("新事件身份=%v，期望=%v", got, want)
			}
			release()
			select {
			case got := <-result:
				if got != want {
					t.Errorf("旧查询覆盖了新事件：管理员=%v，期望=%v", got, want)
				}
			case <-ctx.Done():
				t.Fatal("旧身份查询未结束")
			}
			if got := s.qqIsAdmin(ctx, "GROUP1", "MEMBER1", ""); got != want {
				t.Errorf("后续请求读到旧身份：管理员=%v，期望=%v", got, want)
			}
		})
	}
}

func TestQQRoleQueriesKeepNewerRequestResult(t *testing.T) {
	for _, newerFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("新查询先结束=%v", newerFirst), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			proceed := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			release := [2]func(){sync.OnceFunc(func() { close(proceed[0]) }), sync.OnceFunc(func() { close(proceed[1]) })}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				index := int(calls.Add(1)) - 1
				if index > 1 {
					t.Error("不应重复请求新缓存中的身份")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				close(started[index])
				select {
				case <-proceed[index]:
				case <-r.Context().Done():
					return
				}
				role := "admin"
				if index == 1 {
					role = "member"
				}
				fmt.Fprintf(w, `{"member_role":%q}`, role)
			}))
			defer upstream.Close()
			defer release[0]()
			defer release[1]()
			s := qqRoleOrderServer(upstream)
			results := [2]chan bool{make(chan bool, 1), make(chan bool, 1)}
			for i := range results {
				go func() { results[i] <- s.qqIsAdmin(ctx, "GROUP1", "MEMBER1", "") }()
				select {
				case <-started[i]:
				case <-ctx.Done():
					t.Fatal("身份查询未开始")
				}
			}
			order := []int{0, 1}
			if newerFirst {
				order = []int{1, 0}
			}
			for _, index := range order {
				release[index]()
				select {
				case got := <-results[index]:
					want := index == 0 && !newerFirst
					if got != want {
						t.Errorf("查询 %d 返回管理员=%v，期望=%v", index, got, want)
					}
				case <-ctx.Done():
					t.Fatal("身份查询未结束")
				}
			}
			if s.qqIsAdmin(ctx, "GROUP1", "MEMBER1", "") || calls.Load() != 2 {
				t.Fatal("后续请求未使用较新的普通成员身份缓存")
			}
		})
	}
}

func qqRoleOrderServer(upstream *httptest.Server) *Server {
	// 固定时间确保测试依赖事件与查询的顺序，而非时间戳恰好不同。
	now := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	return &Server{qq: &qqBot{
		config: QQBotConfig{APIURL: upstream.URL, Groups: []string{"GROUP1"}},
		client: upstream.Client(), token: "synthetic-token", expires: now.Add(time.Hour), now: func() time.Time { return now },
	}}
}
