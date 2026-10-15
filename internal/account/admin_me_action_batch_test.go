package account

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminMeAtomicActionsUseOneDatabaseCall(t *testing.T) {
	for _, action := range []string{"set_pref", "revoke_session"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			truncateAdminPersonal(t, db)
			ctx := t.Context()
			const email = "owner@example.test"
			const session = "0123456789abcdef"
			if _, err := db.pool.Exec(ctx, `INSERT INTO admin_sessions(id,email,token_hash,subject,expires_at) VALUES($1,$2,'synthetic-hash','owner',now()+interval '1 day')`, session, email); err != nil {
				t.Fatal(err)
			}
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if err := pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			a := &Service{store: &Store{pool: pool}}
			access := AdminAccess{Actor: "google:o:" + email, Email: email, Role: RoleMaintainer}
			body := `{"action":"set_pref","key":"weekly_digest","value":true}`
			if action == "revoke_session" {
				body = `{"action":"revoke_session","id":"` + session + `"}`
			}
			if w := meRequest(a, access, "POST", body, ""); w.Code != 200 {
				t.Fatalf("个人操作失败：%d %s", w.Code, w.Body.String())
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != 1 {
				t.Errorf("个人操作及审计使用了 %d 次数据库调用，期望 1 次", queries)
			}
			var auditAction string
			var detail map[string]any
			if err := db.pool.QueryRow(ctx, `SELECT action,detail FROM admin_audit WHERE actor=$1`, access.Actor).Scan(&auditAction, &detail); err != nil {
				t.Fatal(err)
			}
			if action == "set_pref" {
				var prefs map[string]bool
				if err := db.pool.QueryRow(ctx, `SELECT prefs FROM admin_preferences WHERE email=$1`, email).Scan(&prefs); err != nil || !prefs["weekly_digest"] || auditAction != "admin_pref_set" || detail["key"] != "weekly_digest" || detail["value"] != true {
					t.Fatal("个人偏好或审计不正确", err)
				}
			} else if auditAction != "admin_session_revoke" || clipboardTestCount(t, db, `SELECT count(*) FROM admin_sessions WHERE id=$1`, session) != 0 {
				t.Fatal("个人会话撤销或审计不正确")
			}
		})
	}
}

func TestAdminMeAtomicActionsRollbackOnAuditFailure(t *testing.T) {
	for _, action := range []string{"set_pref", "revoke_session"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			truncateAdminPersonal(t, db)
			ctx := t.Context()
			const email = "owner@example.test"
			const session = "0123456789abcdef"
			if _, err := db.pool.Exec(ctx, `INSERT INTO admin_sessions(id,email,token_hash,subject,expires_at) VALUES($1,$2,'synthetic-hash','owner',now()+interval '1 day')`, session, email); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `INSERT INTO admin_preferences(email,prefs) VALUES($1,'{"weekly_digest":false,"theme":"dark"}')`, email); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_audit ADD CONSTRAINT admin_me_test_reject CHECK(action NOT IN ('admin_pref_set','admin_session_revoke')) NOT VALID`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_audit DROP CONSTRAINT IF EXISTS admin_me_test_reject`); err != nil {
					t.Error(err)
				}
			})
			body := `{"action":"set_pref","key":"weekly_digest","value":true}`
			if action == "revoke_session" {
				body = `{"action":"revoke_session","id":"` + session + `"}`
			}
			access := AdminAccess{Actor: "google:o:" + email, Email: email, Role: RoleMaintainer}
			if w := meRequest(&Service{store: db}, access, "POST", body, ""); w.Code != 503 {
				t.Fatal("审计失败未返回服务错误", w.Code)
			}
			var prefs map[string]any
			if err := db.pool.QueryRow(ctx, `SELECT prefs FROM admin_preferences WHERE email=$1`, email).Scan(&prefs); err != nil || len(prefs) != 2 || prefs["theme"] != "dark" || prefs["weekly_digest"] != false {
				t.Fatal("审计失败后个人偏好改变", prefs, err)
			}
			if clipboardTestCount(t, db, `SELECT count(*) FROM admin_sessions WHERE id=$1`, session) != 1 || clipboardTestCount(t, db, `SELECT count(*) FROM admin_audit`) != 0 {
				t.Fatal("审计失败后会话或审计记录未回滚")
			}
		})
	}
}
