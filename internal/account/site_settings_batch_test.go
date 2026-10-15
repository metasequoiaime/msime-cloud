package account

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type siteSettingsWriteTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *siteSettingsWriteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "site_settings") || strings.Contains(data.SQL, "admin_audit") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (*siteSettingsWriteTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSiteSettingsWriteAndAuditUseOneDatabaseStatement(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE site_settings,admin_audit`); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &siteSettingsWriteTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	r := httptest.NewRequest("POST", "/api/site-settings", bytes.NewBufferString(`{"lanzou_url":"https://example.com/a"}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(WithAdminAccess(r.Context(), AdminAccess{Actor: "google:sub admin@example.test", Email: "admin@example.test", Role: "operator", Permissions: []string{PermPublishNotices}}))
	w := httptest.NewRecorder()
	a.adminSiteSettings(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("写入站点设置失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("站点设置和审计写入使用了 %d 条相关数据库语句，期望 1 条", queries)
	}
}
