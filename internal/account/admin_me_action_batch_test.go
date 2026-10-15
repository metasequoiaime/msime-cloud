package account

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminMeActionTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *adminMeActionTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "admin_preferences") || strings.Contains(data.SQL, "admin_audit") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}
func (*adminMeActionTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminMePreferenceAndAuditUseOneDatabaseStatement(t *testing.T) {
	db := testStore(t)
	trace := &adminMeActionTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	access := AdminAccess{Actor: "google:o:owner@example.test", Email: "owner@example.test", Role: RoleMaintainer}
	if w := meRequest(a, access, "POST", `{"action":"set_pref","key":"weekly_digest","value":true}`, ""); w.Code != 200 {
		t.Fatalf("个人偏好写入失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("个人偏好和审计使用了 %d 条相关数据库语句，期望 1 条", queries)
	}
}
