package account

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSiteSettingsWriteAndAuditUseOneDatabaseCall(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE site_settings,admin_audit`); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &statementCancellation{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: &Store{pool: pool}}
	for _, value := range []string{"https://example.com/a", "https://example.com/b", ""} {
		t.Run(value, func(t *testing.T) {
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			r := httptest.NewRequest("POST", "/api/site-settings", bytes.NewBufferString(`{"lanzou_url":"`+value+`"}`))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(WithAdminAccess(r.Context(), AdminAccess{Actor: "google:sub admin@example.test", Email: "admin@example.test", Role: "operator", Permissions: []string{PermPublishNotices}}))
			w := httptest.NewRecorder()
			a.adminSiteSettings(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("写入站点设置失败：%d %s", w.Code, w.Body.String())
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != 1 {
				t.Errorf("站点设置、审计与响应使用了 %d 次数据库调用，期望 1 次", queries)
			}
			stored, err := a.loadLanzouSetting(t.Context())
			if err != nil || stored.LanzouURL != value || stored.UpdatedBy != "google:sub admin@example.test" || stored.UpdatedAt == "" {
				t.Fatal("写入后的站点设置不正确", stored, err)
			}
		})
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM admin_audit WHERE action IN ('set_lanzou_url','clear_lanzou_url')`); n != 3 {
		t.Fatal("站点设置审计数量不正确", n)
	}
}
