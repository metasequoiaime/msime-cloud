package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func preferenceSchemaFixture(t testing.TB) (*Service, *http.Request, Tokens) {
	t.Helper()
	dsn := os.Getenv("MSIME_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("设置 MSIME_TEST_DATABASE_URL 运行 PostgreSQL 测试")
	}
	if !strings.Contains(dsn, "msime_auth_test") {
		t.Fatal("仅允许使用一次性测试数据库")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	c := Challenge{IDHash: hash(randomToken()), Provider: "email", Subject: randomToken() + "@example.test"}
	if err = s.PutChallenge(ctx, c); err != nil {
		t.Fatal(err)
	}
	tokens, err := s.Complete(ctx, c, Identity{"email", c.Subject})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), "DELETE FROM auth_users WHERE id=$1", tokens.User.ID); err != nil {
			t.Error(err)
		}
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/users/me/preferences/schema", nil)
	r.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	return &Service{store: s}, r, tokens
}

func TestPreferenceSchemaResponse(t *testing.T) {
	a, r, tokens := preferenceSchemaFixture(t)
	// 按旧处理函数编码整张字段表，独立核对缓存后的完整响应和所有元数据。
	want, err := json.Marshal(map[string]any{"fields": preferenceFields, "maximum_bytes": maximumPreferencesBytes, "update_mode": "replace", "revision_required": true})
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			a.preferencesSchema(w, r)
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
				t.Errorf("schema 响应变化：状态码 %d，字节数 %d，期望 %d", w.Code, w.Body.Len(), len(want))
			}
			for key, value := range map[string]string{"Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer"} {
				if got := w.Header().Get(key); got != value {
					t.Errorf("响应头 %s=%q，期望 %q", key, got, value)
				}
			}
		})
	}
	wg.Wait()
	for _, authorization := range []string{"", "Basic synthetic", "Bearer invalid-synthetic-token"} {
		request := r.Clone(r.Context())
		request.Header.Set("Authorization", authorization)
		w := httptest.NewRecorder()
		a.preferencesSchema(w, request)
		if w.Code != http.StatusUnauthorized || bytes.Contains(w.Body.Bytes(), []byte(`"fields"`)) {
			t.Fatalf("未认证请求返回了 schema：状态码 %d", w.Code)
		}
	}
	if _, err = a.store.pool.Exec(t.Context(), "UPDATE auth_users SET banned_at=now() WHERE id=$1", tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.preferencesSchema(w, r)
	if w.Code != http.StatusForbidden || bytes.Contains(w.Body.Bytes(), []byte(`"fields"`)) {
		t.Fatalf("封禁账号返回了 schema：状态码 %d", w.Code)
	}
	if _, err = a.store.pool.Exec(t.Context(), "UPDATE auth_users SET banned_at=NULL WHERE id=$1", tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.pool.Exec(t.Context(), "UPDATE auth_sessions SET revoked=true WHERE user_id=$1", tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.preferencesSchema(w, r)
	if w.Code != http.StatusUnauthorized || bytes.Contains(w.Body.Bytes(), []byte(`"fields"`)) {
		t.Fatalf("撤销会话返回了 schema：状态码 %d", w.Code)
	}
}

func TestPreferenceSchemaAllocationBudget(t *testing.T) {
	a, r, _ := preferenceSchemaFixture(t)
	var status int
	allocations := testing.AllocsPerRun(20, func() {
		w := httptest.NewRecorder()
		a.preferencesSchema(w, r)
		status = w.Code
	})
	if status != http.StatusOK {
		t.Fatalf("schema 请求失败：%d", status)
	}
	t.Logf("含数据库鉴权和响应记录的 schema 请求分配 %.0f 次", allocations)
	// 给数据库驱动、响应记录器和编码池留余量，禁止逐请求编码整张静态字段表。
	if allocations > 200 {
		t.Fatalf("schema 请求分配 %.0f 次，超过 200 次预算", allocations)
	}
}

func BenchmarkPreferenceSchema(b *testing.B) {
	a, r, _ := preferenceSchemaFixture(b)
	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		a.preferencesSchema(w, r)
		if w.Code != http.StatusOK {
			b.Fatal(w.Code)
		}
	}
}
