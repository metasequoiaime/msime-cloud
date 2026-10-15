package account

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminPermissionChangeBatchesLockAndWrite(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`)
	})
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
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	a := &Service{store: &Store{pool: pool}}
	access := AdminAccess{Actor: "google:o:owner@example.test", Role: RoleMaintainer, Permissions: []string{PermManagePermissions}}
	if w := permissionsRequest(a, access, "POST", `{"action":"grant","role":"readonly","permission":"ban_users"}`); w.Code != 200 {
		t.Fatalf("权限修改失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 3 {
		t.Fatalf("权限修改使用了 %d 次数据库调用，期望事务、锁与写入批次、提交共 3 次", queries)
	}
}
