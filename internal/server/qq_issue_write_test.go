package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func TestQQWriteIssueInvalidatesAroundRequest(t *testing.T) {
	for _, mode := range []string{"success", "rejected", "lost-response"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := qqServer(t)
			ctx := t.Context()
			token, err := s.adminGitHub.Token(ctx, "o/r", issueTokenPerms)
			if err != nil {
				t.Fatal(err)
			}
			client := *s.adminGitHub.HTTP
			original := client.Transport
			if original == nil {
				original = http.DefaultTransport
			}
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var stage atomic.Int64
			sentinel := errors.New("合成写入响应丢失")
			client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/repos/o/r/issues/7/comments" {
					return original.RoundTrip(r)
				}
				if r.Method == http.MethodGet {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`[{"stage":%d}]`, stage.Load())))}, nil
				}
				close(started)
				<-release
				stage.Store(2)
				if mode == "lost-response" {
					return nil, sentinel
				}
				status := 201
				if mode == "rejected" {
					status = 422
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":17}`))}, nil
			})
			s.adminGitHub.HTTP = &client
			path := "/repos/o/r/issues/7/comments"
			read := func(want string) {
				t.Helper()
				r, err := s.adminGitHub.Get(ctx, token, path)
				if err != nil || !strings.Contains(string(r.Body), want) {
					t.Fatalf("读取=%s，错误=%v，期望%s", r.Body, err, want)
				}
			}
			read(`"stage":0`)
			stage.Store(1)
			type result struct {
				response githubapp.Response
				err      error
			}
			done := make(chan result, 1)
			go func() {
				response, err := qqWriteIssue(ctx, s.adminGitHub, token, "o/r", "POST", path, map[string]string{"body": "合成评论"})
				done <- result{response, err}
			}()
			<-started
			// 写请求尚未返回时，预热缓存必须已经失效；这次读取重新缓存写入前状态。
			read(`"stage":1`)
			unblock()
			got := <-done
			if mode == "lost-response" {
				if !errors.Is(got.err, sentinel) {
					t.Fatal(got.err)
				}
			} else {
				want := 201
				if mode == "rejected" {
					want = 422
				}
				if got.err != nil || got.response.Status != want || string(got.response.Body) != `{"id":17}` {
					t.Fatal(got)
				}
			}
			read(`"stage":2`)
		})
	}
}

func TestQQImprovementLabels(t *testing.T) {
	for _, tc := range []struct {
		labels string
		found  bool
	}{
		{`[{"name":"qq-bot"}]`, true},
		{`[{"name":"QQ-BOT"}]`, false},
		{`[{"name":"documentation"}]`, false},
		{`[]`, false},
		{`{`, false},
	} {
		t.Run(tc.labels, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			fake.repoLabels = map[string]string{"o/r": tc.labels}
			token, err := s.adminGitHub.Token(context.Background(), "o/r", issueTokenPerms)
			if err != nil {
				t.Fatal(err)
			}
			got := s.qqImprovementLabels(t.Context(), s.adminGitHub, token, "o/r")
			if tc.found {
				if len(got) != 1 || got[0] != qqImprovementLabel {
					t.Fatal(got)
				}
			} else if got != nil {
				t.Fatal("无匹配时须保留 nil 标签", got)
			}
		})
	}
}
