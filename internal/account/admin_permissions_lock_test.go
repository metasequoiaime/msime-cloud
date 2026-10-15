package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdminPermissionRevokeSeesGrantCommittedDuringLockWait(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`)
	})
	if _, err := db.pool.Exec(ctx, `DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`); err != nil {
		t.Fatal(err)
	}
	grant, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Rollback(context.Background())
	if _, err := grant.Exec(ctx, `SELECT key FROM admin_roles WHERE key='readonly' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	access := AdminAccess{Actor: "google:o:owner@example.test", Role: RoleMaintainer, Permissions: []string{PermManagePermissions}}
	a := &Service{store: db}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := adminJSONRequest("POST", "/api/permissions", `{"action":"revoke","role":"readonly","permission":"ban_users"}`).WithContext(WithAdminAccess(ctx, access))
		w := httptest.NewRecorder()
		a.adminPermissionsAction(w, r, "")
		done <- w
	}()
	waitForAdminDatabaseLock(t, db, grant.Conn().PgConn().PID())
	if _, err := grant.Exec(ctx, `INSERT INTO admin_role_permissions(role,permission) VALUES('readonly','ban_users')`); err != nil {
		t.Fatal(err)
	}
	if err := grant.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w := <-done
	var response struct {
		Affected int64 `json:"affected"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Affected != 1 {
		t.Errorf("等待授权提交后的撤销未生效：状态=%d，响应=%s", w.Code, w.Body.String())
	}
	var remaining, audits int
	if err := db.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'),
 (SELECT count(*) FROM admin_audit WHERE action='permission_revoke' AND target='readonly')`).Scan(&remaining, &audits); err != nil || remaining != 0 || audits != 1 {
		t.Fatal("权限未被撤销或审计缺失", remaining, audits, err)
	}
}

func TestAdminPermissionChangeRollsBackWhenAuditFails(t *testing.T) {
	for _, action := range []string{"grant", "revoke"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			truncateAdminPersonal(t, db)
			ctx := t.Context()
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_audit DROP CONSTRAINT IF EXISTS permissions_test_reject;
DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`); err != nil {
					t.Error(err)
				}
			})
			if _, err := db.pool.Exec(ctx, `DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`); err != nil {
				t.Fatal(err)
			}
			want := 0
			if action == "revoke" {
				if _, err := db.pool.Exec(ctx, `INSERT INTO admin_role_permissions(role,permission) VALUES('readonly','ban_users')`); err != nil {
					t.Fatal(err)
				}
				want = 1
			}
			if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_audit ADD CONSTRAINT permissions_test_reject CHECK(action NOT IN ('permission_grant','permission_revoke')) NOT VALID`); err != nil {
				t.Fatal(err)
			}
			access := AdminAccess{Actor: "google:o:owner@example.test", Role: RoleMaintainer, Permissions: []string{PermManagePermissions}}
			w := permissionsRequest(&Service{store: db}, access, "POST", `{"action":"`+action+`","role":"readonly","permission":"ban_users"}`)
			if w.Code != 503 {
				t.Fatal("审计失败未返回服务错误", w.Code)
			}
			var remaining, audits int
			if err := db.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'),
 (SELECT count(*) FROM admin_audit WHERE action IN ('permission_grant','permission_revoke'))`).Scan(&remaining, &audits); err != nil || remaining != want || audits != 0 {
				t.Fatal("审计失败后权限或审计记录未回滚", remaining, audits, err)
			}
		})
	}
}
