package account

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func BenchmarkCommunityPluginVisibleKindsHTTP(b *testing.B) {
	dsn := os.Getenv("MSIME_TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("设置 MSIME_TEST_DATABASE_URL 运行 PostgreSQL 列表基准")
	}
	if !strings.Contains(dsn, "msime_auth_test") {
		b.Fatal("仅允许使用一次性测试数据库")
	}
	store, err := Open(b.Context(), dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(store.Close)
	if err = store.Migrate(b.Context()); err != nil {
		b.Fatal(err)
	}
	if _, err = store.pool.Exec(b.Context(), "TRUNCATE community_plugins CASCADE"); err != nil {
		b.Fatal(err)
	}
	a := &Service{store: store}
	for _, tc := range []struct{ name, query string }{
		{"default", ""},
		{"unknown", "?kinds=future_type,,UNKNOWN"},
		{"extension", "?kinds=helpcode"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := httptest.NewRequest("GET", "/v1/community/plugins"+tc.query, nil)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				a.communityPluginList(w, r)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"plugins":[]`) {
					b.Fatalf("空插件列表响应变化：%d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
