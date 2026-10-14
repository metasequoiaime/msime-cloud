package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func waitForAdminDatabaseLock(t *testing.T, db *Store, blocker uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, int32(blocker)).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("请求未进入数据库锁等待")
		case <-ticker.C:
		}
	}
}

func TestAdminTokenRegenerateSourceIdentity(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	a := &Service{store: db}
	email := randomToken()[:16] + "@example.test"
	access := AdminAccess{Actor: "google:g:" + email, Email: email, Role: RoleMaintainer, Owner: true}
	create := func(subject, email string) string {
		t.Helper()
		token, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: subject, Email: email})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	source := create("g", email)
	w := meRequest(a, access, "POST", `{"action":"regenerate_token"}`, source)
	var issued struct {
		Token string `json:"token"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &issued) != nil || issued.Token == "" {
		t.Fatal("有效来源会话未能签发令牌", w.Code)
	}
	for name, cookie := range map[string]string{
		"缺少来源": "",
		"未知会话": randomToken(),
		"邮箱不同": create("g", "other@example.test"),
		"主体不同": create("other", email),
	} {
		t.Run(name, func(t *testing.T) {
			w := meRequest(a, access, "POST", `{"action":"regenerate_token"}`, cookie)
			if w.Code != http.StatusUnauthorized {
				t.Fatal("来源会话未与已认证身份匹配", w.Code)
			}
			if _, err := a.AdminTokenIdentity(ctx, issued.Token); err != nil {
				t.Fatal("无效签发请求破坏了原有有效令牌", err)
			}
		})
	}
}

func TestAdminTokenRegenerateRejectsRevokedSource(t *testing.T) {
	for _, action := range []string{"revoke", "disable", "logout", "revoke_session", "expire"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			a := &Service{store: db}
			email := randomToken()[:16] + "@example.test"
			if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email) VALUES($1)`, email); err != nil {
				t.Fatal(err)
			}
			source, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
			if err != nil {
				t.Fatal(err)
			}
			access := AdminAccess{Actor: "google:g:" + email, Email: email, Role: RoleMaintainer, Permissions: AllAdminPermissions()}
			block, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer block.Rollback(context.Background())
			if _, err = block.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin_tokens:'||$1,0))`, email); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- meRequest(a, access, "POST", `{"action":"regenerate_token"}`, source)
			}()
			// 以真实锁等待为依据，保证来源身份已进入签发流程，而撤销先于令牌写入。
			waitForAdminDatabaseLock(t, db, block.Conn().PgConn().PID())
			switch action {
			case "revoke", "disable":
				r := jsonRequest("POST", "/api/admins", `{"action":"`+action+`","email":"`+email+`"}`, "")
				w := httptest.NewRecorder()
				a.AdminMembersHTTP(w, r, []string{"owner@example.test"})
				if w.Code != http.StatusOK {
					t.Fatal("撤销成员凭据失败", w.Code)
				}
			case "logout":
				if err := a.DeleteAdminSession(ctx, source); err != nil {
					t.Fatal(err)
				}
			case "revoke_session":
				var id string
				if err := db.pool.QueryRow(ctx, `SELECT id FROM admin_sessions WHERE token_hash=$1`, hash(source)).Scan(&id); err != nil {
					t.Fatal(err)
				}
				if w := meRequest(a, access, "POST", `{"action":"revoke_session","id":"`+id+`"}`, source); w.Code != http.StatusOK {
					t.Fatal("吊销会话失败", w.Code)
				}
			case "expire":
				// 到期时间在签发事务开始后、当前时钟前，防止误用固定在事务开始时的 now()。
				if _, err := db.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=(
 SELECT max(xact_start)+interval '1 microsecond' FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))
) WHERE token_hash=$2`, int32(block.Conn().PgConn().PID()), hash(source)); err != nil {
					t.Fatal(err)
				}
			}
			if err := block.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case w := <-done:
				if w.Code != http.StatusUnauthorized {
					t.Errorf("来源会话已失效，签发仍返回 HTTP %d", w.Code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("解除锁后签发请求未完成")
			}
			var tokens, audits int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_tokens WHERE email=$1`, email).Scan(&tokens); err != nil {
				t.Fatal(err)
			}
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE action='admin_token_regenerate' AND target=$1`, email).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if tokens != 0 || audits != 0 {
				t.Errorf("来源会话失效后仍留下 %d 个令牌、%d 条签发审计", tokens, audits)
			}
		})
	}
}

func TestAdminTokenRegenerateSerializesMemberRevocation(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	a := &Service{store: db}
	email := randomToken()[:16] + "@example.test"
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email) VALUES($1)`, email); err != nil {
		t.Fatal(err)
	}
	source, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithAdminSession(WithAdminAccess(ctx, AdminAccess{Actor: "google:g:" + email, Email: email}), source)
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	token, _, err := issueAdminToken(ctx, tx, email)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := jsonRequest("POST", "/api/admins", `{"action":"revoke","email":"`+email+`"}`, "")
		w := httptest.NewRecorder()
		a.AdminMembersHTTP(w, r, []string{"owner@example.test"})
		done <- w
	}()
	// 签发先获得来源会话的行锁时，撤销必须等它提交，再删除刚签发的令牌。
	waitForAdminDatabaseLock(t, db, tx.Conn().PgConn().PID())
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatal("凭据撤销失败", w.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("签发提交后凭据撤销未完成")
	}
	if _, err := a.AdminTokenIdentity(ctx, token); err != ErrInvalid {
		t.Fatal("并发撤销完成后仍保留签发的令牌", err)
	}
}
