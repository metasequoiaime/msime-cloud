package account

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminPermissionChangeBatchesLockAndWrite(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`); err != nil {
		t.Fatal(err)
	}
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
	a := &Service{store: &Store{pool: pool}}
	access := AdminAccess{Actor: "google:o:owner@example.test", Role: RoleMaintainer, Permissions: []string{PermManagePermissions}}
	for _, tc := range []struct {
		action, role string
		status       int
		affected     int64
		remaining    int
		audits       int
	}{
		{"grant", "readonly", 200, 1, 1, 1},
		{"grant", "readonly", 200, 0, 1, 1},
		{"revoke", "readonly", 200, 1, 0, 2},
		{"revoke", "readonly", 200, 0, 0, 2},
		{"grant", "missing-role", 404, 0, 0, 2},
		{"revoke", "missing-role", 404, 0, 0, 2},
	} {
		trace.mu.Lock()
		trace.seen, trace.statements = 0, nil
		trace.mu.Unlock()
		w := permissionsRequest(a, access, "POST", `{"action":"`+tc.action+`","role":"`+tc.role+`","permission":"ban_users"}`)
		if w.Code != tc.status {
			t.Fatalf("权限修改返回错误状态：%d %s", w.Code, w.Body.String())
		}
		if w.Code == 200 {
			var response struct {
				Affected int64 `json:"affected"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Affected != tc.affected {
				t.Fatal("权限修改行数错误", response.Affected, tc.affected, err)
			}
		}
		trace.mu.Lock()
		queries := trace.seen
		trace.mu.Unlock()
		if queries != 1 {
			t.Errorf("%s %s 使用了 %d 次数据库调用，期望 1 次", tc.action, tc.role, queries)
		}
		var remaining, audits int
		if err := db.pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'),
 (SELECT count(*) FROM admin_audit WHERE action IN ('permission_grant','permission_revoke'))`).Scan(&remaining, &audits); err != nil || remaining != tc.remaining || audits != tc.audits {
			t.Fatal("权限或审计状态错误", remaining, audits, err)
		}
	}
}
