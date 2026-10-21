package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

type upstreamBaseURLCase struct {
	name, method, path string
	validate           func(string) (string, error)
	request            func(context.Context, *http.Client, string) error
}

func upstreamBaseURLCases(t *testing.T) []upstreamBaseURLCase {
	t.Helper()
	words := wordsConfig(t, "https://upstream.example.test")
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	t.Setenv("TEST_BASE_URL_TELEGRAM", testTelegramToken)
	githubRequest := func(ctx context.Context, client *http.Client, base string) error {
		c := githubapp.Client{APIURL: base, HTTP: client}
		r, err := c.Do(ctx, "synthetic-installation", http.MethodGet, "/repos/synthetic/project/issues?state=open", nil)
		if err == nil && !r.OK() {
			return fmt.Errorf("GitHub 请求状态错误：%d", r.Status)
		}
		return err
	}
	return []upstreamBaseURLCase{
		{
			name: "后台 GitHub", method: http.MethodGet, path: "/repos/synthetic/project/issues?state=open",
			validate: func(raw string) (string, error) {
				c := AdminGitHubConfig{AppID: 42, InstallationID: 77, PrivateKeyEnv: "TEST_WORDS_APP_KEY", APIURL: raw}
				err := c.validate()
				return c.APIURL, err
			},
			request: githubRequest,
		},
		{
			name: "投稿 GitHub", method: http.MethodGet, path: "/repos/synthetic/project/issues?state=open",
			validate: func(raw string) (string, error) {
				c := words
				c.GitHub.APIURL = raw
				err := c.validate(true, []string{wordsTestOrigin})
				return c.GitHub.APIURL, err
			},
			request: githubRequest,
		},
		{
			name: "QQ", method: http.MethodPost, path: "/app/getAppAccessToken",
			validate: func(raw string) (string, error) {
				c := qqValidConfig()
				c.QQBot.APIURL = raw
				err := c.QQBot.validate(&c)
				return c.QQBot.APIURL, err
			},
			request: func(ctx context.Context, client *http.Client, base string) error {
				b := &qqBot{config: QQBotConfig{AppID: "102030405", APIURL: base, secret: qqTestSecret}, client: client, now: time.Now}
				_, err := b.accessToken(ctx)
				return err
			},
		},
		{
			name: "Telegram", method: http.MethodPost, path: "/bot" + testTelegramToken + "/sendMessage",
			validate: func(raw string) (string, error) {
				c := AdminTelegramConfig{ChatID: "@synthetic_news", BotTokenEnv: "TEST_BASE_URL_TELEGRAM", APIURL: raw}
				err := c.validate()
				return c.APIURL, err
			},
			request: func(ctx context.Context, client *http.Client, base string) error {
				s := &Server{config: Config{Admin: AdminConfig{Telegram: AdminTelegramConfig{ChatID: "@synthetic_news", APIURL: base, botToken: testTelegramToken}}}}
				b := s.noticeBroadcaster().(*telegramNotices)
				b.client = client
				return b.BroadcastNotice(ctx, account.Notice{Title: "合成公告"})
			},
		},
		{
			name: "Loki", method: http.MethodGet, path: "/loki/api/v1/query?" + url.Values{"query": {`{job="synthetic"}`}}.Encode(),
			validate: func(raw string) (string, error) {
				c := AdminLogsConfig{LokiURL: raw}
				err := c.validate()
				return c.LokiURL, err
			},
			request: func(ctx context.Context, client *http.Client, base string) error {
				s := &Server{config: Config{Admin: AdminConfig{Logs: AdminLogsConfig{LokiURL: base}}}, loki: client}
				var result map[string]any
				return s.lokiGet(ctx, "/loki/api/v1/query", url.Values{"query": {`{job="synthetic"}`}}, &result)
			},
		},
	}
}

func TestUpstreamBaseURLsRejectEmptyFragments(t *testing.T) {
	for _, tc := range upstreamBaseURLCases(t) {
		for _, base := range []string{"https://upstream.example.test", "https://upstream.example.test/proxy"} {
			for _, suffix := range []string{"#", "/#", "#/", "/#/", "?", "?#", "#fragment"} {
				t.Run(tc.name+"/"+base+suffix, func(t *testing.T) {
					if _, err := tc.validate(base + suffix); err == nil {
						t.Fatal("含查询或片段分隔符的基础地址通过了校验")
					}
				})
			}
		}
	}
}

func TestUpstreamBaseURLsPreserveRequests(t *testing.T) {
	for _, tc := range upstreamBaseURLCases(t) {
		for _, prefix := range []string{"", "/proxy", "/api%23proxy", "/api%3Fproxy"} {
			for _, slash := range []string{"", "/"} {
				t.Run(tc.name+prefix+slash, func(t *testing.T) {
					requests := make(chan string, 1)
					upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests <- r.Method + " " + r.RequestURI
						_, _ = io.WriteString(w, `{"access_token":"synthetic-access","expires_in":3600,"ok":true}`)
					}))
					defer upstream.Close()
					base, err := tc.validate(upstream.URL + prefix + slash)
					if err != nil {
						t.Fatal("合法基础地址校验失败", err)
					}
					if err := tc.request(t.Context(), upstream.Client(), base); err != nil {
						t.Fatal("合法基础地址请求失败", err)
					}
					select {
					case got := <-requests:
						if want := tc.method + " " + prefix + tc.path; got != want {
							t.Fatalf("上游请求路径或查询发生变化：got=%q want=%q", got, want)
						}
					default:
						t.Fatal("未向上游发送请求")
					}
				})
			}
		}
	}
}

func TestTurnstileURLRejectsEmptyFragment(t *testing.T) {
	c := wordsConfig(t, "https://upstream.example.test")
	c.Turnstile.SiteverifyURL += "#"
	if err := c.validate(true, []string{wordsTestOrigin}); err == nil || !strings.Contains(err.Error(), "siteverify_url") {
		t.Fatal("完整验证地址的空片段应在共用校验中被拒绝", err)
	}
}
