package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// /mcp/s/{id} 绕过全局 Bearer 检查（设备令牌不能读快照），没有用户数据库时返回 503；跨站浏览器请求照样被 Origin 检查拦下，不带 Origin 的 MCP 客户端不受影响。
func TestDiagnosticsMCPRoute(t *testing.T) {
	s := fixture(t, nil)
	for _, tc := range []struct {
		method, origin, token string
		status                int
	}{
		{"POST", "", "", 503},
		{"POST", "", testToken, 503},
		{"GET", "", "msk_x", 503},
		{"POST", "https://untrusted.example", "msk_x", 403},
		{"PUT", "", "msk_x", 405},
	} {
		r := httptest.NewRequest(tc.method, "/mcp/s/0123456789abcdef01234567", strings.NewReader(`{}`))
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
}

func TestDiagnosticsMCPRejectsUnknownIDsWithoutRateRows(t *testing.T) {
	db, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	s, err := New(Config{Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseAccounts)
	t.Cleanup(s.Close)
	for i := range 12 {
		r := httptest.NewRequest("POST", fmt.Sprintf("/mcp/s/%024x", i), strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer msk_wrong")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("未知快照请求返回 %d: %s", w.Code, w.Body.String())
		}
	}
	var count int
	if err := db.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{schema, "auth_rates"}.Sanitize()+" WHERE key LIKE 'mcp-snapshot:%'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("未知快照创建了 %d 条限流记录", count)
	}
}
