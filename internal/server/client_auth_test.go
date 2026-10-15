package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func configuredAuthClients(tb testing.TB, count, tokenBytes int) (Config, []string) {
	tb.Helper()
	c := Config{}
	tokens := make([]string, count)
	for i := range tokens {
		prefix := fmt.Sprintf("synthetic-%04d-", i)
		tokens[i] = prefix + strings.Repeat("t", tokenBytes-len(prefix))
		env := fmt.Sprintf("TEST_AUTH_CLIENT_%d", i)
		tb.Setenv(env, tokens[i])
		c.Clients = append(c.Clients, Client{ID: fmt.Sprintf("client-%d", i), TokenEnv: env, RequestsPerMinute: 100000})
	}
	if err := c.Validate(); err != nil {
		tb.Fatal(err)
	}
	return c, tokens
}

func TestValidatedClientAuthenticationSnapshot(t *testing.T) {
	c, tokens := configuredAuthClients(t, 3, 32)
	for i, token := range tokens {
		if strings.Contains(fmt.Sprintf("%#v", c.Clients[i]), token) {
			t.Error("配置校验后应只保存固定客户端令牌的摘要")
		}
	}
	s := &Server{config: c, slots: make(chan struct{}, 1), buckets: map[string]bucket{}}
	handler := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Owner", r.Context().Value(skinJobOwnerKey{}).(string))
		w.WriteHeader(http.StatusNoContent)
	}))
	check := func(auth, owner string) {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/capabilities", nil)
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		wantCode := http.StatusNoContent
		if owner == "" {
			wantCode = http.StatusUnauthorized
		}
		if w.Code != wantCode || w.Header().Get("X-Test-Owner") != owner {
			t.Fatal("客户端鉴权状态或身份不正确", w.Code, wantCode)
		}
		if owner == "" && w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("拒绝响应缺少认证要求")
		}
	}
	for i, token := range tokens {
		check("Bearer "+token, c.Clients[i].ID)
	}
	for _, auth := range []string{"", tokens[0], "bearer " + tokens[0], "Bearer ", "Bearer  " + tokens[0], "Bearer " + tokens[0] + "x", "Bearer Bearer " + tokens[0]} {
		check(auth, "")
	}
	// 环境变量变化在下次校验时生效，已有校验快照不逐请求读取环境变量。
	rotated := strings.Repeat("r", 64)
	t.Setenv(c.Clients[0].TokenEnv, rotated)
	check("Bearer "+tokens[0], c.Clients[0].ID)
	check("Bearer "+rotated, "")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s.config = c
	check("Bearer "+tokens[0], "")
	check("Bearer "+rotated, c.Clients[0].ID)
}

// 测量拒绝未知令牌时的完整鉴权中间件，所有配置客户端都会参与比较，不受成功请求的限流额度影响。
func BenchmarkConfiguredClientAuthentication(b *testing.B) {
	for _, count := range []int{1, 16, 256} {
		for _, tokenBytes := range []int{32, 1024} {
			b.Run(fmt.Sprintf("clients_%d/bytes_%d", count, tokenBytes), func(b *testing.B) {
				c, _ := configuredAuthClients(b, count, tokenBytes)
				s := &Server{config: c}
				handler := s.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					b.Fatal("未知客户端不应通过鉴权")
				}))
				r := httptest.NewRequest("GET", "/v1/capabilities", nil)
				r.Header.Set("Authorization", "Bearer "+strings.Repeat("u", 32))
				b.ReportAllocs()
				for b.Loop() {
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					if w.Code != http.StatusUnauthorized {
						b.Fatal("未知客户端没有被拒绝", w.Code)
					}
				}
			})
		}
	}
}
