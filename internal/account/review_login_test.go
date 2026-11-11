package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewLoginConfigValidation(t *testing.T) {
	t.Setenv("REVIEW_TEST_DB", "postgres://example.test/db")
	t.Setenv("REVIEW_TEST_PEPPER", strings.Repeat("p", 32))
	base := Config{Enabled: true, DatabaseEnv: "REVIEW_TEST_DB", PepperEnv: "REVIEW_TEST_PEPPER"}
	for _, tc := range []struct {
		name  string
		email string
		env   string
		value string
		ok    bool
	}{
		{"未配置", "", "", "", true},
		{"有效配置", "play-review@example.test", "REVIEW_TEST_CODE", "024681", true},
		{"缺验证码变量名", "play-review@example.test", "", "", false},
		{"验证码不是数字", "play-review@example.test", "REVIEW_TEST_CODE", "12a456", false},
		{"验证码不是 6 位", "play-review@example.test", "REVIEW_TEST_CODE", "12345", false},
		{"验证码变量为空", "play-review@example.test", "REVIEW_TEST_CODE", "", false},
		{"邮箱有大写", "Play-Review@example.test", "REVIEW_TEST_CODE", "024681", false},
		{"邮箱带名字", "Review <play-review@example.test>", "REVIEW_TEST_CODE", "024681", false},
		{"只有变量名", "", "REVIEW_TEST_CODE", "024681", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REVIEW_TEST_CODE", tc.value)
			c := base
			c.ReviewLogin = ReviewLoginConfig{Email: tc.email, CodeEnv: tc.env}
			if e := c.Validate(); (e == nil) != tc.ok {
				t.Fatalf("期望有效=%v，得到错误 %v", tc.ok, e)
			}
		})
	}
}

func TestReviewLoginCodeMatchesOnlyLoginToConfiguredEmail(t *testing.T) {
	t.Setenv("REVIEW_TEST_CODE", "024681")
	a := &Service{config: Config{ReviewLogin: ReviewLoginConfig{Email: "play-review@example.test", CodeEnv: "REVIEW_TEST_CODE"}}}
	if code, ok := a.reviewLoginCode("email", "  Play-Review@Example.test ", ""); !ok || code != "024681" {
		t.Fatal("审核邮箱登录应忽略大小写和首尾空白", code, ok)
	}
	if _, ok := a.reviewLoginCode("email", "play-review@example.test", "login"); !ok {
		t.Fatal("purpose=login 应走审核账号")
	}
	for _, tc := range []struct{ provider, target, purpose string }{
		{"email", "play-review@example.test", "link"},
		{"email", "other@example.test", "login"},
		{"phone", "play-review@example.test", "login"},
	} {
		if _, ok := a.reviewLoginCode(tc.provider, tc.target, tc.purpose); ok {
			t.Fatal("不应走审核账号", tc)
		}
	}
	t.Setenv("REVIEW_TEST_CODE", "")
	if _, ok := a.reviewLoginCode("email", "play-review@example.test", ""); ok {
		t.Fatal("验证码变量被清空后应关闭审核账号")
	}
	if _, ok := (&Service{}).reviewLoginCode("email", "", ""); ok {
		t.Fatal("未配置时不应匹配空邮箱")
	}
}

func TestReviewLoginHTTPFlow(t *testing.T) {
	s := testStore(t)
	t.Setenv("AUTH_TEST_PEPPER", strings.Repeat("p", 32))
	t.Setenv("REVIEW_TEST_CODE", "024681")
	sender := &captureSender{}
	// 审核邮箱用的是不在常用服务商名单里的域名，正常流程会以 unsupported_email_domain 拒绝。
	a := &Service{store: s, config: Config{PepperEnv: "AUTH_TEST_PEPPER", Email: MailConfig{From: "login@example.test"}, ReviewLogin: ReviewLoginConfig{Email: "play-review@example.test", CodeEnv: "REVIEW_TEST_CODE"}}, sender: sender}
	mux := http.NewServeMux()
	Mount(mux, a)
	request := func(path string, v any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(v)
		r := httptest.NewRequest("POST", path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	begin := func() string {
		t.Helper()
		w := request("/v1/auth/challenges", map[string]string{"provider": "email", "target": "Play-Review@example.test", "purpose": "login"})
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "024681") {
			t.Fatal("响应不得泄露验证码")
		}
		var c struct {
			ID string `json:"challenge_id"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &c); e != nil || len(c.ID) != 64 {
			t.Fatal("应返回挑战 ID", w.Body.String(), e)
		}
		return c.ID
	}
	id := begin()
	if sender.sent != 0 {
		t.Fatal("审核账号不应发送邮件", sender.sent)
	}
	var deliveries int
	if e := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM auth_rates WHERE key='delivery-global'`).Scan(&deliveries); e != nil || deliveries != 0 {
		t.Fatal("审核账号不应占用全站发信额度", deliveries, e)
	}
	if w := request("/v1/auth/login", map[string]string{"challenge_id": id, "credential": "135790"}); w.Code != 401 {
		t.Fatal("错误验证码应失败", w.Code)
	}
	w := request("/v1/auth/login", map[string]string{"challenge_id": id, "credential": "024681"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var first Tokens
	if e := json.Unmarshal(w.Body.Bytes(), &first); e != nil || first.User.ID == "" {
		t.Fatal("应返回登录令牌", w.Body.String(), e)
	}
	if w = request("/v1/auth/login", map[string]string{"challenge_id": id, "credential": "024681"}); w.Code != 401 {
		t.Fatal("挑战只能用一次", w.Code)
	}
	if w = request("/v1/auth/challenges", map[string]string{"provider": "email", "target": "play-review@example.test"}); w.Code != 429 {
		t.Fatal("按目标的每分钟限额应对审核账号生效", w.Code, w.Body.String())
	}
	if _, e := s.pool.Exec(context.Background(), `UPDATE auth_rates SET expires_at=now()-interval '1 second' WHERE key LIKE 'target-minute:%'`); e != nil {
		t.Fatal(e)
	}
	w = request("/v1/auth/login", map[string]string{"challenge_id": begin(), "credential": "024681"})
	var second Tokens
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &second) != nil || second.User.ID != first.User.ID {
		t.Fatal("再次登录应回到同一个账号", w.Code, w.Body.String())
	}
	var subject string
	if e := s.pool.QueryRow(context.Background(), `SELECT subject FROM auth_identities WHERE provider='email' AND user_id=$1`, first.User.ID).Scan(&subject); e != nil || subject != "play-review@example.test" {
		t.Fatal("身份应记为配置里的小写邮箱", subject, e)
	}
	if sender.sent != 0 {
		t.Fatal("审核账号全程不应发送邮件", sender.sent)
	}
}
