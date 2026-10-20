package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

type platformReleaseResponse struct {
	status int
	body   string
}

func platformReleaseReadFixture(tb testing.TB, delay time.Duration, before func(*http.Request)) (*Server, map[string]platformReleaseResponse, func() map[string]int) {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	responses := map[string]platformReleaseResponse{
		"o/first":  {200, `[{"tag_name":"v1","published_at":"2026-10-01"},{"tag_name":"android-v2","published_at":"2026-10-02"}]`},
		"o/second": {200, `[{"tag_name":"v3","published_at":"2026-10-03"}]`},
		"o/main":   {200, `[{"tag_name":"macos-v4","published_at":"2026-10-04"},{"tag_name":"windows-v5","prerelease":true},{"tag_name":"linux-v6"},{"tag_name":"android-v7"}]`},
	}
	var mu sync.Mutex
	counts := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/77/access_tokens" {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-token", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/releases")
		if r.Method != http.MethodGet || r.URL.Query().Get("per_page") != "50" {
			tb.Error("Release 请求方法或分页参数改变")
		}
		mu.Lock()
		counts[repo]++
		mu.Unlock()
		if before != nil {
			before(r)
		}
		if delay > 0 {
			// 基准刻意模拟每个独立 Release 读取的上游延迟。
			time.Sleep(delay)
		}
		response, ok := responses[repo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(response.status)
		_, _ = io.WriteString(w, response.body)
	}))
	tb.Cleanup(upstream.Close)
	s := &Server{
		qq: &qqBot{config: QQBotConfig{Repo: "o/main"}},
		adminGitHub: &githubapp.Client{AppID: 42, InstallationID: 77, Key: key, APIURL: upstream.URL,
			HTTP: upstream.Client(), Cache: &githubapp.Cache{}},
	}
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{
		{ID: "windows", Repo: "o/first", TagPrefix: "v"},
		{ID: "linux", Repo: "o/second", TagPrefix: "v"},
		{ID: "android", Repo: "o/first", TagPrefix: "android-v"},
		{ID: "macos", Repo: "o/main", TagPrefix: "macos-v"},
		{ID: "ignored", Repo: "o/unused"},
	}
	return s, responses, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(counts)
	}
}

func startPlatformReleaseRead(ctx context.Context, s *Server) <-chan []qqPlatformRelease {
	done := make(chan []qqPlatformRelease, 1)
	go func() { done <- s.qqLatestReleases(ctx) }()
	return done
}

func TestQQPlatformReleaseReadOverlap(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "无缓存", true: "带缓存"}[cached], func(t *testing.T) {
			entered, release := make(chan string, 8), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			s, _, counts := platformReleaseReadFixture(t, 0, func(r *http.Request) {
				entered <- r.URL.Path
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			if !cached {
				s.adminGitHub.Cache = nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := startPlatformReleaseRead(ctx, s)
			paths := map[string]bool{}
			for range 3 {
				select {
				case path := <-entered:
					paths[path] = true
				case <-ctx.Done():
					t.Fatal("三个平台仓库未同时进入上游：", paths)
				}
			}
			if len(paths) != 3 {
				t.Fatal("重复请求未去重，或未读取全部独立平台仓库：", paths)
			}
			unblock()
			select {
			case list := <-done:
				var ids, stable, next []string
				for _, p := range list {
					ids = append(ids, p.ID)
					if p.Stable != nil {
						stable = append(stable, p.Stable.TagName)
					}
					for _, r := range p.NextGen {
						next = append(next, r.TagName)
					}
				}
				if !reflect.DeepEqual(ids, []string{"windows", "linux", "android", "macos"}) || !reflect.DeepEqual(stable, []string{"v1", "v3", "android-v2", "macos-v4"}) || !reflect.DeepEqual(next, []string{"windows-v5", "linux-v6", "android-v7"}) {
					t.Fatalf("配置顺序或版本选择改变：平台=%v，正式版=%v，新版=%v", ids, stable, next)
				}
				if got := counts(); !reflect.DeepEqual(got, map[string]int{"o/first": 1, "o/second": 1, "o/main": 1}) {
					t.Fatal("同仓库重复读取或读取了空前缀平台：", got)
				}
			case <-ctx.Done():
				t.Fatal("上游放行后目录读取未结束")
			}
		})
	}
}

func TestQQPlatformReleaseReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second platformReleaseResponse
		main          platformReleaseResponse
		wantIDs       []string
		wantMain      int
	}{
		{"平台均失败", platformReleaseResponse{503, "{}"}, platformReleaseResponse{200, "["}, platformReleaseResponse{200, "[]"}, nil, 0},
		{"部分平台失败", platformReleaseResponse{403, "{}"}, platformReleaseResponse{200, `[{"tag_name":"v3"}]`}, platformReleaseResponse{200, "[]"}, []string{"linux"}, 1},
		{"新版仓库失败", platformReleaseResponse{200, "[]"}, platformReleaseResponse{200, "null"}, platformReleaseResponse{503, "{}"}, []string{"windows", "linux", "android"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, responses, counts := platformReleaseReadFixture(t, 0, nil)
			// 关闭底层缓存，确认目录自己对成功与失败读取都只执行一次。
			s.adminGitHub.Cache = nil
			s.config.Admin.GitHub.Platforms = s.config.Admin.GitHub.Platforms[:3]
			responses["o/first"], responses["o/second"], responses["o/main"] = tc.first, tc.second, tc.main
			var ids []string
			for _, platform := range s.qqLatestReleases(t.Context()) {
				ids = append(ids, platform.ID)
				if len(platform.NextGen) != 0 {
					t.Fatal("失败或空的新版仓库不应产生版本：", platform)
				}
			}
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("读取失败的平台筛选改变：结果=%v，期望=%v", ids, tc.wantIDs)
			}
			got := counts()
			if got["o/first"] != 1 || got["o/second"] != 1 || got["o/main"] != tc.wantMain {
				t.Fatal("失败请求未去重或提前读取了不需要的新版仓库：", got)
			}
		})
	}
}

func TestQQPlatformReleaseReadCancellation(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "无缓存", true: "带缓存"}[cached], func(t *testing.T) {
			entered, release := make(chan struct{}, 8), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			s, _, _ := platformReleaseReadFixture(t, 0, func(r *http.Request) {
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			if !cached {
				s.adminGitHub.Cache = nil
			}
			deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(deadline)
			defer cancel()
			done := startPlatformReleaseRead(ctx, s)
			for range 3 {
				select {
				case <-entered:
				case <-deadline.Done():
					t.Fatal("取消测试未进入全部平台读取")
				}
			}
			cancel()
			select {
			case list := <-done:
				if len(list) != 0 || deadline.Err() != nil {
					t.Fatal("主动取消后不应保留尚未读取的平台或继续等待：", list)
				}
			case <-deadline.Done():
				t.Fatal("主动取消后目录读取未退出")
			}
		})
	}
}

func BenchmarkQQPlatformReleaseColdReads(b *testing.B) {
	s, _, _ := platformReleaseReadFixture(b, 20*time.Millisecond, nil)
	for _, repo := range []string{"o/first", "o/second", "o/main"} {
		if _, err := s.adminGitHub.Token(b.Context(), repo, qqGitHubReadPerms); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		s.adminGitHub.Invalidate("/repos/")
		if got := s.qqLatestReleases(b.Context()); len(got) != 4 {
			b.Fatal("合成版本目录读取失败：", got)
		}
	}
}
