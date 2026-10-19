package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestDiagnosticsMCPBatchCannotAmplifySharedQuota(t *testing.T) {
	db, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	s, err := New(Config{Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseAccounts)
	t.Cleanup(s.Close)
	const id = "0123456789abcdef01234567"
	const token = "msk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const content = `{"config_snapshot":{"theme":"synthetic-setting"}}`
	digest := sha256.Sum256([]byte(token))
	if _, err := db.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{schema, "auth_users"}.Sanitize()+"(id) VALUES('mcp-batch-user')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{schema, "diagnostic_snapshots"}.Sanitize()+`(id,user_id,token_hash,token_hint,platform,app_version,sections,content,bytes,expires_at)
VALUES($1,'mcp-batch-user',$2,'AAAA','android','1.0.0',ARRAY['config_snapshot'],$3,$4,clock_timestamp()+interval '1 hour')`, id, hex.EncodeToString(digest[:]), content, len(content)); err != nil {
		t.Fatal(err)
	}
	request := func(token, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "/mcp/s/"+id, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_config_snapshot","arguments":{}}}`
	batch := "[" + call + "," + strings.Replace(call, `"id":1`, `"id":2`, 1) + "]"
	if w := request(token, batch); w.Code != 400 || strings.Contains(w.Body.String(), "synthetic-setting") {
		t.Errorf("批请求未拒绝：%d %s", w.Code, w.Body.String())
	}
	var accesses, count int
	if err := db.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{schema, "diagnostic_accesses"}.Sanitize()).Scan(&accesses); err != nil || accesses != 0 {
		t.Errorf("批请求访问记录=%d err=%v", accesses, err)
	}
	if err := db.QueryRow(t.Context(), "SELECT count FROM "+pgx.Identifier{schema, "auth_rates"}.Sanitize()+" WHERE key LIKE 'mcp-snapshot:%'").Scan(&count); err != nil || count != 1 {
		t.Errorf("批请求共享配额=%d err=%v", count, err)
	}
	if w := request(token, call); w.Code != 200 || !strings.Contains(w.Body.String(), "synthetic-setting") {
		t.Fatalf("单条工具调用失败：%d %s", w.Code, w.Body.String())
	}
	if w := request("msk_wrong", batch); w.Code != 401 {
		t.Errorf("错误令牌未先鉴权：%d %s", w.Code, w.Body.String())
	}
	if _, err := db.Exec(t.Context(), "UPDATE "+pgx.Identifier{schema, "auth_rates"}.Sanitize()+" SET count=60 WHERE key LIKE 'mcp-snapshot:%'"); err != nil {
		t.Fatal(err)
	}
	if w := request(token, batch); w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Errorf("批请求绕过共享限流：%d %s", w.Code, w.Body.String())
	}
	if err := db.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{schema, "diagnostic_accesses"}.Sanitize()).Scan(&accesses); err != nil || accesses != 1 {
		t.Fatalf("额外执行了工具：accesses=%d err=%v", accesses, err)
	}
}
