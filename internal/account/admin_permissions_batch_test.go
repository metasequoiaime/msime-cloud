package account

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type permissionsWriteTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *permissionsWriteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "admin_roles") || strings.Contains(data.SQL, "admin_role_permissions") || strings.Contains(data.SQL, "admin_audit") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}
func (*permissionsWriteTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminPermissionChangeUsesOneDatabaseStatement(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`)
	})
	cfg := db.pool.Config()
	trace := &permissionsWriteTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	access := AdminAccess{Actor: "google:o:owner@example.test", Role: RoleMaintainer, Permissions: []string{PermManagePermissions}}
	if w := permissionsRequest(a, access, "POST", `{"action":"grant","role":"readonly","permission":"ban_users"}`); w.Code != 200 {
		t.Fatalf("权限修改失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("权限修改和审计使用了 %d 条相关数据库语句，期望 1 条", queries)
	}
}
