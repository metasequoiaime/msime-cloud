package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// harmonySignupFixture 是同时开着词条表单（提供 Turnstile）和鸿蒙内测配置的服务，华为接口指向 fakeAGC。
func harmonySignupFixture(t *testing.T) (*Server, *fakeWordsUpstream, *fakeLimiter, *fakeAGC) {
	t.Helper()
	s, f, limiter := wordsFixture(t)
	f.turnstile["action"] = "harmony"
	agc := &fakeAGC{t: t, members: map[string]string{}, nickNames: map[string]string{}, responses: map[string][2]string{}}
	upstream := httptest.NewServer(agc)
	t.Cleanup(upstream.Close)
	s.qq = newQQBot(QQBotConfig{AppID: "102030405", secret: qqTestSecret})
	s.qq.harmony = &harmonyTesters{config: HarmonyTestConfig{AppID: "1234567890", GroupID: "G42", APIURL: upstream.URL, keyID: "kid-1", subAccount: "sub-1", key: wordsKey(t)}, client: upstream.Client(), now: time.Now}
	return s, f, limiter, agc
}

func postHarmonySignup(s *Server, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", harmonySignupPath, strings.NewReader(body))
	r.Header.Set("Origin", wordsTestOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "198.51.100.7:4567"
	for _, m := range mutate {
		m(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestHarmonySignupSettings(t *testing.T) {
	get := func(s *Server) map[string]any {
		r := httptest.NewRequest("GET", harmonySignupPath, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		return decodeBody(t, w)
	}
	if v := get(fixture(t, nil)); v["enabled"] != false || v["site_key"] != "" {
		t.Fatal("unconfigured", v)
	}
	// 只开词条表单、没有鸿蒙内测配置时不开放。
	s, _, _ := wordsFixture(t)
	if v := get(s); v["enabled"] != false {
		t.Fatal("words only", v)
	}
	s, _, _, _ = harmonySignupFixture(t)
	if v := get(s); v["enabled"] != true || v["site_key"] != "0x4AAAAAAA-site-key" {
		t.Fatal("configured", v)
	}
}

func TestHarmonySignupRegisters(t *testing.T) {
	s, f, limiter, agc := harmonySignupFixture(t)
	w := postHarmonySignup(s, `{"account":" 138 0013 8000 ","token":"turnstile-token"}`)
	if w.Code != 200 || decodeBody(t, w)["registered"] != true {
		t.Fatal(w.Code, w.Body.String())
	}
	if agc.members["86-13800138000"] == "" || agc.nickNames["86-13800138000"] != "官网" {
		t.Fatal("account not added", agc.members, agc.nickNames)
	}
	// 官网登记只加进群组，不发通知，也不为了通知再查一次成员。
	if len(agc.notified) != 0 || !slices.Equal(agc.calls, []string{"GET /publish/v2/test/group", "POST /publish/v2/test/user"}) {
		t.Fatal("unexpected calls", agc.calls, agc.notified)
	}
	if f.count("POST /siteverify") != 1 || !slices.Equal(limiter.calls, []string{"harmony-testers-10m|198.51.100.7", "harmony-testers-day|198.51.100.7", "harmony-testers-total|all"}) {
		t.Fatal("verification or limits", f.count("POST /siteverify"), limiter.calls)
	}
	// 已经在群组里的账号返回同样的结果，不重复添加。
	agc.calls = nil
	w = postHarmonySignup(s, `{"account":"+86 13800138000","token":"turnstile-token"}`)
	if w.Code != 200 || decodeBody(t, w)["registered"] != true {
		t.Fatal("existing", w.Code, w.Body.String())
	}
	if !slices.Equal(agc.calls, []string{"GET /publish/v2/test/group"}) || len(agc.notified) != 0 {
		t.Fatal("existing member re-added or notified", agc.calls)
	}
	w = postHarmonySignup(s, `{"account":"Tester@Example.com","token":"turnstile-token"}`)
	if w.Code != 200 || agc.members["tester@example.com"] == "" {
		t.Fatal("email", w.Code, w.Body.String(), agc.members)
	}
}

func TestHarmonySignupRejections(t *testing.T) {
	s := fixture(t, nil)
	if w := postHarmonySignup(s, `{}`, func(r *http.Request) { r.Header.Del("Origin") }); w.Code != 503 || decodeBody(t, w)["code"] != "harmony_signup_disabled" {
		t.Fatal("disabled", w.Code, w.Body.String())
	}
	for name, c := range map[string]struct {
		body   string
		mutate func(*http.Request)
		status int
		code   string
	}{
		"no origin":      {`{"account":"13800138000","token":"turnstile-token"}`, func(r *http.Request) { r.Header.Del("Origin") }, 403, "origin_required"},
		"content type":   {`{"account":"13800138000","token":"turnstile-token"}`, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, "json_required"},
		"too large":      {`{"account":"` + strings.Repeat("1", harmonySignupBodyBytes) + `","token":"t"}`, nil, 413, "request_too_large"},
		"bad json":       {`{"account":`, nil, 400, "invalid_json"},
		"unknown field":  {`{"account":"13800138000","token":"t","notify":true}`, nil, 400, "invalid_json"},
		"empty account":  {`{"account":"  ","token":"turnstile-token"}`, nil, 400, "invalid_account"},
		"not an account": {`{"account":"hello","token":"turnstile-token"}`, nil, 400, "invalid_account"},
		"two accounts":   {`{"account":"13800138000 a@example.com","token":"turnstile-token"}`, nil, 400, "invalid_account"},
		"long account":   {`{"account":"` + strings.Repeat("a", harmonySignupAccountChars) + `@example.com","token":"turnstile-token"}`, nil, 400, "invalid_account"},
		"no token":       {`{"account":"13800138000"}`, nil, 400, "token_required"},
		"long token":     {`{"account":"13800138000","token":"` + strings.Repeat("t", wordSubmissionTokenBytes+1) + `"}`, nil, 400, "token_required"},
	} {
		t.Run(name, func(t *testing.T) {
			s, f, limiter, agc := harmonySignupFixture(t)
			mutate := []func(*http.Request){}
			if c.mutate != nil {
				mutate = append(mutate, c.mutate)
			}
			w := postHarmonySignup(s, c.body, mutate...)
			if w.Code != c.status || decodeBody(t, w)["code"] != c.code {
				t.Fatal(w.Code, w.Body.String())
			}
			if f.count("POST /siteverify") != 0 || len(limiter.calls) != 0 || len(agc.calls) != 0 {
				t.Fatal("rejected request went further")
			}
		})
	}
}

func TestHarmonySignupTurnstile(t *testing.T) {
	const body = `{"account":"13800138000","token":"turnstile-token"}`
	for name, c := range map[string]struct {
		status int
		result map[string]any
		code   int
	}{
		"failed": {200, map[string]any{"success": false}, 403},
		// 词条表单的令牌不能拿来登记。
		"words token":  {200, map[string]any{"success": true, "action": "words", "hostname": "msime.app"}, 403},
		"wrong host":   {200, map[string]any{"success": true, "action": "harmony", "hostname": "evil.example"}, 403},
		"server error": {500, map[string]any{}, 503},
	} {
		t.Run(name, func(t *testing.T) {
			s, f, limiter, agc := harmonySignupFixture(t)
			f.turnstileStatus, f.turnstile = c.status, c.result
			w := postHarmonySignup(s, body)
			if w.Code != c.code || f.count("POST /siteverify") != 1 {
				t.Fatal(w.Code, w.Body.String())
			}
			if len(limiter.calls) != 0 || len(agc.calls) != 0 {
				t.Fatal("unverified request went further")
			}
		})
	}
}

func TestHarmonySignupRateLimit(t *testing.T) {
	const body = `{"account":"13800138000","token":"turnstile-token"}`
	s, _, limiter, agc := harmonySignupFixture(t)
	limiter.err = account.ErrLimited
	w := postHarmonySignup(s, body)
	if w.Code != 429 || w.Header().Get("Retry-After") != "600" || decodeBody(t, w)["code"] != "rate_limit_exceeded" || len(limiter.calls) != 1 || len(agc.calls) != 0 {
		t.Fatal(w.Code, w.Body.String(), limiter.calls)
	}
	limiter.err = errors.New("database down")
	if w = postHarmonySignup(s, body); w.Code != 503 || decodeBody(t, w)["code"] != "rate_limit_unavailable" {
		t.Fatal("limiter failure", w.Code, w.Body.String())
	}
	// 全站每天的总数用完时提示明天再来。
	s, _, _, agc = harmonySignupFixture(t)
	s.words.limiter = &scopedLimiter{limited: "harmony-testers-total"}
	w = postHarmonySignup(s, body)
	if w.Code != 429 || w.Header().Get("Retry-After") != "3600" || !strings.Contains(decodeBody(t, w)["error"].(string), "明天") || len(agc.calls) != 0 {
		t.Fatal("global limit", w.Code, w.Body.String())
	}
	// Turnstile 前的闸门按地址计数，用完返回 429，数据库不可用返回 503，都不调 siteverify。
	s, f, _, _ := harmonySignupFixture(t)
	gate := &fakeLimiter{err: account.ErrLimited}
	s.limits = gate
	if w = postHarmonySignup(s, body); w.Code != 429 || w.Header().Get("Retry-After") != "60" || len(gate.calls) != 1 || gate.calls[0] != "harmony-testers|198.51.100.7" {
		t.Fatal("shared gate", w.Code, gate.calls)
	}
	gate.err = errors.New("database down")
	if w = postHarmonySignup(s, body); w.Code != 503 || decodeBody(t, w)["code"] != "rate_limit_unavailable" || f.count("POST /siteverify") != 0 {
		t.Fatal("shared gate failure", w.Code, w.Body.String())
	}
}

// scopedLimiter 只拒绝一个范围。
type scopedLimiter struct{ limited string }

func (l *scopedLimiter) RateLimit(_ context.Context, scope, _ string, _ int, _ time.Duration) error {
	if scope == l.limited {
		return account.ErrLimited
	}
	return nil
}

func (l *scopedLimiter) RateLimitUntil(ctx context.Context, scope, subject string, limit int, window time.Duration) (time.Duration, error) {
	return 0, l.RateLimit(ctx, scope, subject, limit, window)
}

func (l *scopedLimiter) ReleaseRateLimit(context.Context, string, string) error { return nil }

func TestHarmonySignupAGCFailures(t *testing.T) {
	const body = `{"account":"13800138000","token":"turnstile-token"}`
	for name, c := range map[string]struct {
		key, status, response, code string
	}{
		"group full":   {"POST /publish/v2/test/user", "200", `{"rtnCode":1,"businessCode":101}`, "group_full"},
		"query denied": {"GET /publish/v2/test/group", "200", `{"ret":{"code":2}}`, "agc_unavailable"},
		"http error":   {"POST /publish/v2/test/user", "500", `boom`, "agc_unavailable"},
		"dropped":      {"GET /publish/v2/test/group", "drop", ``, "agc_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, agc := harmonySignupFixture(t)
			agc.responses[c.key] = [2]string{c.status, c.response}
			w := postHarmonySignup(s, body)
			if w.Code != 503 || w.Header().Get("Retry-After") != "60" || decodeBody(t, w)["code"] != c.code {
				t.Fatal(w.Code, w.Body.String())
			}
			// 上游正文和账号不能出现在响应里。
			if strings.Contains(w.Body.String(), "boom") || strings.Contains(w.Body.String(), "13800138000") {
				t.Fatal("leaked", w.Body.String())
			}
		})
	}
}
