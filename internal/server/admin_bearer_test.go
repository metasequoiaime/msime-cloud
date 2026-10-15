package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func validatedLegacyAdmin(tb testing.TB, token string) AdminConfig {
	tb.Helper()
	tb.Setenv("TEST_ADMIN_HASH_TOKEN", token)
	c := AdminConfig{Enabled: true, Host: "admin.example.test", TokenEnv: "TEST_ADMIN_HASH_TOKEN"}
	if err := c.validate(true, nil); err != nil {
		tb.Fatal(err)
	}
	return c
}

func TestAdminBearerAvoidsRedundantAllocations(t *testing.T) {
	long := strings.Repeat("a", 1024)
	for _, tc := range []struct {
		name, configured, header string
		want                     bool
		maximum                  float64
	}{
		{"disabled", "", "Bearer " + long, false, 0},
		{"invalid_prefix", long, long, false, 0},
		{"wrong_short", long, "Bearer " + strings.Repeat("b", 32), false, 0},
		{"valid_long", long, "Bearer " + long, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			if tc.configured != "" {
				s.config.Admin = validatedLegacyAdmin(t, tc.configured)
			}
			r := httptest.NewRequest("GET", "/api/auth/session", nil)
			r.Header.Set("Authorization", tc.header)
			var got bool
			allocations := testing.AllocsPerRun(100, func() { got = s.adminBearer(r) })
			if got != tc.want {
				t.Fatal("旧管理员密钥鉴权结果不正确", got, tc.want)
			}
			if allocations > tc.maximum {
				t.Fatalf("旧管理员密钥鉴权每次分配 %.0f 次，上限 %.0f 次", allocations, tc.maximum)
			}
		})
	}
}

func TestAdminBearerCredentialSnapshot(t *testing.T) {
	token := strings.Repeat("a", 40)
	s := &Server{config: Config{Admin: validatedLegacyAdmin(t, token)}}
	check := func(header string, want bool) {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/auth/session", nil)
		r.Header.Set("Authorization", header)
		if got := s.adminBearer(r); got != want {
			t.Fatal("旧管理员密钥鉴权结果不正确", got, want)
		}
	}
	check("Bearer "+token, true)
	for _, header := range []string{"", "Bearer ", token, "bearer " + token, "Bearer  " + token, "Bearer " + token + "x"} {
		check(header, false)
	}
	// 环境变量变化在重新校验时生效，避免请求路径重新读取密钥配置。
	rotated := strings.Repeat("r", 64)
	t.Setenv(s.config.Admin.TokenEnv, rotated)
	check("Bearer "+token, true)
	check("Bearer "+rotated, false)
	if err := s.config.Admin.validate(true, nil); err != nil {
		t.Fatal(err)
	}
	check("Bearer "+token, false)
	check("Bearer "+rotated, true)
	// 只启用 Google 登录时，旧密钥及空 Bearer 都不能通过旧密钥鉴权。
	t.Setenv(s.config.Admin.TokenEnv, "")
	t.Setenv("TEST_ADMIN_HASH_GOOGLE_SECRET", "synthetic-google-secret")
	s.config.Admin.Google = AdminGoogleConfig{ClientID: "synthetic-client", SecretEnv: "TEST_ADMIN_HASH_GOOGLE_SECRET", AllowedEmails: []string{"owner@example.test"}}
	if err := s.config.Admin.validate(true, nil); err != nil {
		t.Fatal(err)
	}
	check("Bearer "+rotated, false)
	check("Bearer ", false)
}

func BenchmarkAdminBearer(b *testing.B) {
	short, long := strings.Repeat("a", 40), strings.Repeat("a", 1024)
	for _, tc := range []struct {
		name, configured, header string
		want                     bool
	}{
		{"disabled_long", "", "Bearer " + long, false},
		{"invalid_prefix", long, long, false},
		{"wrong_short", long, "Bearer " + strings.Repeat("b", 32), false},
		{"valid_short", short, "Bearer " + short, true},
		{"valid_long", long, "Bearer " + long, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			s := &Server{}
			if tc.configured != "" {
				s.config.Admin = validatedLegacyAdmin(b, tc.configured)
			}
			r := httptest.NewRequest("GET", "/api/auth/session", nil)
			r.Header.Set("Authorization", tc.header)
			b.ReportAllocs()
			for b.Loop() {
				if got := s.adminBearer(r); got != tc.want {
					b.Fatal("旧管理员密钥鉴权结果不正确", got, tc.want)
				}
			}
		})
	}
}
