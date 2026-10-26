package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestQQBotGapKeepsDedupeAfterLostGitHubWriteResponse(t *testing.T) {
	s, fake, limiter := qqServer(t)
	s.qq.config.GapsRepo = "o/docs"
	fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题",` + qqAppAuthor + `}]`

	client := *s.adminGitHub.HTTP
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	commentPosts := 0
	lostResponse := true
	client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			commentPosts++
			if lostResponse {
				lostResponse = false
				return response, errors.New("synthetic connection reset after GitHub write")
			}
		}
		return response, err
	})
	s.adminGitHub.HTTP = &client

	if s.recordQQGap(context.Background(), "群友问：", "候选窗透明度") {
		t.Fatal("响应丢失时不应报告为已确认记录")
	}
	key := "qq-bot-gap|候选窗透明度"
	if limiter.counts[key] != 1 || len(limiter.released) != 0 {
		t.Fatalf("GitHub 已写入但去重额度被退回：counts=%v released=%v", limiter.counts, limiter.released)
	}
	if commentPosts != 1 {
		t.Fatalf("首次请求次数=%d，期望 1", commentPosts)
	}

	if !s.recordQQGap(context.Background(), "群友问：", "候选窗透明度") {
		t.Fatal("已保留的去重记录应让重试视为已记录")
	}
	if commentPosts != 1 {
		t.Fatalf("重试重复写入评论，请求次数=%d", commentPosts)
	}
}
