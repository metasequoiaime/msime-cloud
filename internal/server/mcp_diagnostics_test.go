package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestDiagnosticsMCPAuditsJSONBIncompatibleArguments(t *testing.T) {
	admin, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	s, err := New(Config{Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseAccounts)
	t.Cleanup(s.Close)
	db, err := account.Open(t.Context(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	sum := sha256.Sum256([]byte("diagnostic-arguments-challenge"))
	challenge := account.Challenge{IDHash: hex.EncodeToString(sum[:]), Provider: "email", Subject: "diagnostic-arguments@example.test"}
	if err := db.PutChallenge(t.Context(), challenge); err != nil {
		t.Fatal(err)
	}
	tokens, err := db.Complete(t.Context(), challenge, account.Identity{Provider: "email", Subject: challenge.Subject})
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w := request("/v1/users/me/diagnostics", tokens.AccessToken, `{"platform":"android","app_version":"1.0.0","ttl":"one_day","sections":{"input_events":[{"t_ms":1,"kind":"commit"}]}}`)
	var snapshot struct{ ID, Token string }
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &snapshot) != nil || snapshot.ID == "" || snapshot.Token == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		name, arguments string
		quoted          bool
	}{
		{"NUL", `{"kind":"\u0000"}`, true},
		{"high surrogate", `{"kind":"\ud800"}`, true},
		{"low surrogate", `{"kind":"\udc00"}`, true},
		{"numeric underflow", `{"limit":1e-20000}`, true},
		{"literal escape", `{"kind":"\\u0000"}`, false},
		{"surrogate pair", `{"kind":"\ud83d\ude00"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request("/mcp/s/"+snapshot.ID, snapshot.Token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_input_events","arguments":`+tc.arguments+`}}`)
			var response struct {
				Error  json.RawMessage `json:"error"`
				Result *struct {
					IsError bool `json:"isError"`
				} `json:"result"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Error) > 0 || response.Result == nil || response.Result.IsError {
				t.Fatalf("工具调用失败: %d %s", w.Code, w.Body.String())
			}
			var raw []byte
			if err := admin.QueryRow(t.Context(), "SELECT arguments FROM "+pgx.Identifier{schema, "diagnostic_accesses"}.Sanitize()+" WHERE snapshot_id=$1 ORDER BY id DESC LIMIT 1", snapshot.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if tc.quoted {
				var original string
				if json.Unmarshal(raw, &original) != nil || original != tc.arguments {
					t.Fatalf("参数原文没有保留: %s", raw)
				}
			} else {
				var equal bool
				if err := admin.QueryRow(t.Context(), "SELECT $1::jsonb=$2::jsonb", raw, tc.arguments).Scan(&equal); err != nil || !equal {
					t.Fatalf("正常 JSON 参数发生变化: %s %v", raw, err)
				}
			}
		})
	}
	var count int
	if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{schema, "diagnostic_accesses"}.Sanitize()+" WHERE snapshot_id=$1", snapshot.ID).Scan(&count); err != nil || count != 6 {
		t.Fatalf("访问记录数: %d %v", count, err)
	}
	// 无论直接写入还是转为原文后写入失败，都必须阻止读取结果返回。
	if _, err := admin.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{schema, "diagnostic_accesses"}.Sanitize()+" ADD CONSTRAINT reject_access CHECK(result_count<0) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{}`, `{"kind":"\u0000"}`} {
		w := request("/mcp/s/"+snapshot.ID, snapshot.Token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_input_events","arguments":`+arguments+`}}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), "access log unavailable") || strings.Contains(w.Body.String(), "structuredContent") || strings.Contains(w.Body.String(), "t_ms") {
			t.Fatalf("审计失败后仍返回了读取结果: %d %s", w.Code, w.Body.String())
		}
	}
}

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
