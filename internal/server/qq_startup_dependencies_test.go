package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// qqStartupLog 在发版自测实际跑完时通知测试，不干预后台任务的执行顺序。
type qqStartupLog struct{ summary chan []byte }

func (l qqStartupLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"msg":"qq bot: selftest"`)) {
		l.summary <- bytes.Clone(p)
	}
	return len(p), nil
}

func TestQQStartupIncludesLearnedCasesWithoutDataRace(t *testing.T) {
	disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_ADMIN_TOKEN", strings.Repeat("a", 48))
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	t.Setenv("TEST_QQ_APP_KEY", wordsKeyPEM(t, false))
	previousTransport := http.DefaultTransport
	http.DefaultTransport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = previousTransport }()

	c := qqValidConfig()
	c.Auth.DatabaseEnv, c.Auth.PepperEnv = "MSIME_TEST_DATABASE_URL", "TEST_AUTH_PEPPER"
	c.Admin.Host, c.Admin.TokenEnv = "admin.example.test", "TEST_ADMIN_TOKEN"
	c.Admin.GitHub = AdminGitHubConfig{AppID: 42, InstallationID: 77, PrivateKeyEnv: "TEST_QQ_APP_KEY", APIURL: "https://upstream.example.test"}
	c.Chat.URL = "https://upstream.example.test/v1/chat/completions"
	c.QQBot.EmbeddingModel, c.QQBot.ImprovementRepo = "none", "none"
	seed, err := account.New(t.Context(), c.Auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.AddQQSelfTestCase(t.Context(), "启动时学到的问题", "合成的参考答案"); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	seed.Close()

	summary := make(chan []byte, 1)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(qqStartupLog{summary}, nil)))
	defer slog.SetDefault(previous)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseAccounts()
	defer s.Close()
	select {
	case line := <-summary:
		var result struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(line, &result); err != nil {
			t.Fatal(err)
		}
		if result.Total != len(qqSelfTestCases)+1 {
			t.Fatalf("启动自测遗漏了已存的题目：得到 %d 题", result.Total)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("启动后的发版自测没有完成")
	}
}
