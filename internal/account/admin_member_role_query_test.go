package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func requestAdminMemberRole(ctx context.Context, a *Service, email, role string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"action": "set_role", "email": email, "role": role})
	r := jsonRequest("POST", "/api/admins", string(body), "").WithContext(WithAdminActor(ctx, "google:owner:owner@example.test"))
	w := httptest.NewRecorder()
	a.AdminMembersHTTP(w, r, []string{"owner@example.test"})
	return w
}

func TestAdminMemberRoleChangeQueryCount(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: &Store{pool: pool}}
	for _, state := range []string{"changed", "disabled", "same", "missing", "invalid_role", "missing_invalid_role"} {
		t.Run(state, func(t *testing.T) {
			email := randomToken()[:16] + "@example.test"
			missing := state == "missing" || state == "missing_invalid_role"
			var before time.Time
			if !missing {
				if err := db.pool.QueryRow(ctx, `INSERT INTO admin_members(email,role,enabled,updated_at) VALUES($1,'reviewer',$2,now()-interval '1 day') RETURNING updated_at`, email, state != "disabled").Scan(&before); err != nil {
					t.Fatal(err)
				}
			}
			role, wantCode, wantQueries, wantAffected := "operator", 200, 6, 1
			if state == "same" {
				role, wantQueries, wantAffected = "reviewer", 5, 0
			} else if state == "missing" {
				wantCode, wantQueries = 404, 5
			} else if state == "invalid_role" || state == "missing_invalid_role" {
				role, wantCode, wantQueries = "unknown", 400, 4
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			w := requestAdminMemberRole(ctx, a, email, role)
			trace.mu.Lock()
			calls := trace.seen
			trace.mu.Unlock()
			if w.Code != wantCode {
				t.Fatalf("修改角色返回 %d，期望 %d：%s", w.Code, wantCode, w.Body.String())
			}
			if calls != wantQueries {
				t.Errorf("修改角色使用 %d 次数据库调用，期望 %d 次（含事务、锁、校验与审计）", calls, wantQueries)
			}
			if wantCode == 200 {
				var response struct{ Affected int }
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Affected != wantAffected {
					t.Fatal("角色修改数量不正确", response, err)
				}
			}
			changed := wantCode == 200 && wantAffected == 1
			var audits int
			var detail map[string]string
			if err := db.pool.QueryRow(ctx, `SELECT count(*),max(detail::text)::jsonb FROM admin_audit WHERE action='admin_set_role' AND target=$1`, email).Scan(&audits, &detail); err != nil {
				t.Fatal(err)
			}
			if changed && (audits != 1 || detail["from"] != "reviewer" || detail["to"] != "operator") || !changed && audits != 0 {
				t.Fatal("角色修改审计不正确", audits, detail)
			}
			if !missing {
				var storedRole string
				var enabled bool
				var updatedAt time.Time
				if err := db.pool.QueryRow(ctx, `SELECT role,enabled,updated_at FROM admin_members WHERE email=$1`, email).Scan(&storedRole, &enabled, &updatedAt); err != nil {
					t.Fatal(err)
				}
				wantRole := "reviewer"
				if changed {
					wantRole = "operator"
				}
				if storedRole != wantRole || enabled != (state != "disabled") || changed && !updatedAt.After(before) || !changed && !updatedAt.Equal(before) {
					t.Fatal("角色、启用状态或更新时间不正确", storedRole, enabled, updatedAt)
				}
			}
		})
	}
}

func TestAdminMemberRoleChangeUsesLockedPreviousRole(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	email := randomToken()[:16] + "@example.test"
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email,role) VALUES($1,'reviewer')`, email); err != nil {
		t.Fatal(err)
	}
	block, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(context.Background())
	if _, err := block.Exec(ctx, `SELECT 1 FROM admin_members WHERE email=$1 FOR UPDATE`, email); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- requestAdminMemberRole(ctx, &Service{store: db}, email, "readonly") }()
	waitForAdminDatabaseLock(t, db, block.Conn().PgConn().PID())
	// 请求已等待成员行锁，再提交新的旧角色，审计必须以锁后读取的角色为准。
	if _, err := block.Exec(ctx, `UPDATE admin_members SET role='operator' WHERE email=$1`, email); err != nil {
		t.Fatal(err)
	}
	if err := block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if w := <-done; w.Code != 200 {
		t.Fatal("解除成员行锁后修改角色失败", w.Code, w.Body.String())
	}
	var role string
	var detail map[string]string
	if err := db.pool.QueryRow(ctx, `SELECT role,(SELECT detail FROM admin_audit WHERE action='admin_set_role' AND target=$1) FROM admin_members WHERE email=$1`, email).Scan(&role, &detail); err != nil || role != "readonly" || detail["from"] != "operator" || detail["to"] != "readonly" {
		t.Fatal("角色或审计没有使用等待期间提交的新状态", role, detail, err)
	}
}

func TestAdminMemberRoleChangeAuditFailureRollsBack(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	email := randomToken()[:16] + "@example.test"
	var before time.Time
	if err := db.pool.QueryRow(ctx, `INSERT INTO admin_members(email,role) VALUES($1,'reviewer') RETURNING updated_at`, email).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_audit ADD CONSTRAINT fail_member_role_audit CHECK(action<>'admin_set_role') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_audit DROP CONSTRAINT IF EXISTS fail_member_role_audit`); err != nil {
			t.Error(err)
		}
	})
	a := &Service{store: db}
	if w := requestAdminMemberRole(ctx, a, email, "operator"); w.Code != 503 {
		t.Fatal("审计失败未返回数据库错误", w.Code, w.Body.String())
	}
	var role string
	var updatedAt time.Time
	var audits int
	if err := db.pool.QueryRow(ctx, `SELECT role,updated_at,(SELECT count(*) FROM admin_audit WHERE action='admin_set_role' AND target=$1) FROM admin_members WHERE email=$1`, email).Scan(&role, &updatedAt, &audits); err != nil || role != "reviewer" || !updatedAt.Equal(before) || audits != 0 {
		t.Fatal("审计失败后仍留下角色修改", role, updatedAt, audits, err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_audit DROP CONSTRAINT fail_member_role_audit`); err != nil {
		t.Fatal(err)
	}
	if w := requestAdminMemberRole(ctx, a, email, "operator"); w.Code != 200 {
		t.Fatal("审计恢复后无法重试角色修改", w.Code, w.Body.String())
	}
}
