package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQQResearchRechecksPreviouslyPublicRepository(t *testing.T) {
	for _, test := range []struct{ name, visibility, want string }{
		{"转为私有", `{"private":true,"visibility":"private"}`, "这个仓库不公开，不能查。"},
		{"转为内部", `{"private":false,"visibility":"internal"}`, "这个仓库不公开，不能查。"},
		{"检查失败", "!500", "确认不了这个仓库是否公开，不能查。"},
		{"响应缺字段", `{"visibility":"public"}`, "确认不了这个仓库是否公开，不能查。"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			const repo = "metasequoiaime/example"
			const first = "/repos/" + repo + "/contents/public.md"
			const next = "/repos/" + repo + "/contents/new.md"
			fake.extraGitHub = map[string]string{first: `{"name":"public.md"}`, next: `{"content":"private-marker"}`}
			if got := s.qqResearchCall(context.Background(), qqTestGitHubGet(first)); !strings.Contains(got, "public.md") {
				t.Fatal("公开仓库查询失败：", got)
			}
			fake.mu.Lock()
			fake.repoVisibility = map[string]string{repo: test.visibility}
			fake.mu.Unlock()
			if got := s.qqResearchCall(context.Background(), qqTestGitHubGet(next)); got != test.want {
				t.Fatalf("先前公开的状态仍允许读取新内容：%s", got)
			}
		})
	}
}

func TestQQResearchDiscardsReadWhenRepositoryBecomesPrivate(t *testing.T) {
	s, fake, _ := qqServer(t)
	now := time.Now()
	s.qq.now = func() time.Time { return now }
	const repo = "metasequoiaime/example"
	const target = "/repos/" + repo + "/contents/new.md"
	fake.extraGitHub = map[string]string{target: `{"content":"private-marker"}`}
	transport := s.adminGitHub.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var reads atomic.Int32
	client := *s.adminGitHub.HTTP
	client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil && r.URL.Path == target {
			// 内容请求在途时改为私有，返回之前必须重新确认可见性。
			if reads.Add(1) == 1 {
				fake.mu.Lock()
				fake.repoVisibility = map[string]string{repo: `{"private":true,"visibility":"private"}`}
				fake.mu.Unlock()
			}
		}
		return response, err
	})
	s.adminGitHub.HTTP = &client
	if got := s.qqResearchCall(context.Background(), qqTestGitHubGet(target)); got != "这个仓库不公开，不能查。" || reads.Load() != 1 {
		t.Fatalf("读取期间转为私有的内容仍被交给模型：%s，读取次数=%d", got, reads.Load())
	}
	// 拒绝缓存到期后重新公开，不能复用先前私有读取的内容缓存。
	now = now.Add(qqResearchVisibilityTTL)
	fake.mu.Lock()
	fake.repoVisibility = nil
	fake.extraGitHub[target] = `{"content":"public-marker"}`
	fake.mu.Unlock()
	if got := s.qqResearchCall(context.Background(), qqTestGitHubGet(target)); !strings.Contains(got, "public-marker") || reads.Load() != 2 {
		t.Fatalf("重新公开后复用了被拒绝的内容缓存：%s，读取次数=%d", got, reads.Load())
	}
}

func TestQQResearchRefusesPrivateDefaultRepositoryScope(t *testing.T) {
	for _, target := range []string{"/orgs/metasequoiaime/repos", "/search/issues?q=org%3Ametasequoiaime"} {
		t.Run(target, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			fake.repoVisibility = map[string]string{s.qq.config.Repo: `{"private":true,"visibility":"private"}`}
			fake.extraGitHub = map[string]string{target: `{"items":[{"name":"private-marker"}]}`}
			if got := s.qqResearchCall(context.Background(), qqTestGitHubGet(target)); got != "这个仓库不公开，不能查。" {
				t.Fatalf("组织或搜索请求使用了私有默认仓库的令牌：%s", got)
			}
		})
	}
}
