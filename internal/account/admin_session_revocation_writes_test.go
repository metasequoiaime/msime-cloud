package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func adminSessionVersions(t *testing.T, db *Store) map[string]string {
	t.Helper()
	rows, err := db.pool.Query(t.Context(), `SELECT id,xmin::text FROM auth_sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	versions := map[string]string{}
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			t.Fatal(err)
		}
		versions[id] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return versions
}

func TestAdminSessionRevocationSkipsUnchangedWrites(t *testing.T) {
	for _, action := range []string{"revoke_session", "revoke_sessions"} {
		t.Run(action, func(t *testing.T) {
			for _, state := range []string{"active", "revoked", "expired", "missing", "empty", "foreign"} {
				if action == "revoke_session" && state == "empty" || action == "revoke_sessions" && state == "foreign" {
					continue
				}
				t.Run(state, func(t *testing.T) {
					db, _ := usersTestStore(t)
					ctx := t.Context()
					owner := complete(t, db, Identity{"email", "revoke-owner@example.test"})
					other := complete(t, db, Identity{"email", "revoke-bystander@example.test"})
					p, err := db.Authenticate(ctx, owner.AccessToken)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.pool.Exec(ctx, `INSERT INTO auth_sessions(id,user_id,access_hash,refresh_hash,access_expires,expires_at,revoked)
 SELECT 'synthetic-admin-session-'||n,$1,'access-admin-'||n,'refresh-admin-'||n,now()+interval '15 minutes',
 CASE WHEN n=4 THEN now()-interval '1 day' ELSE now()+interval '30 days' END,n<=2 FROM generate_series(1,4) n`, owner.User.ID); err != nil {
						t.Fatal(err)
					}
					id, userID := p.SessionID, owner.User.ID
					if action == "revoke_sessions" {
						id = owner.User.ID
					}
					wantCode, wantAffected, wantWrites, wantCalls := 200, 1, 1, 4
					if action == "revoke_sessions" {
						wantAffected, wantWrites = 5, 3
					}
					switch state {
					case "revoked":
						_, err = db.pool.Exec(ctx, `UPDATE auth_sessions SET revoked=true WHERE user_id=$1`, owner.User.ID)
						wantWrites = 0
					case "expired":
						_, err = db.pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=now()-interval '1 day' WHERE user_id=$1`, owner.User.ID)
					case "missing":
						id, wantCode, wantCalls, wantWrites = "missing", 404, 3, 0
					case "foreign":
						userID, wantCode, wantCalls, wantWrites = other.User.ID, 404, 3, 0
					case "empty":
						_, err = db.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id=$1`, owner.User.ID)
						wantAffected, wantWrites = 0, 0
					}
					if err != nil {
						t.Fatal(err)
					}
					trace := &statementCancellation{}
					cfg := db.pool.Config()
					cfg.ConnConfig.Tracer = trace
					pool, err := pgxpool.NewWithConfig(ctx, cfg)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(pool.Close)
					a := &Service{store: &Store{pool: pool}}
					body, err := json.Marshal(map[string]string{"action": action, "id": id, "user_id": userID})
					if err != nil {
						t.Fatal(err)
					}
					for attempt := range 2 {
						before := adminSessionVersions(t, db)
						trace.mu.Lock()
						trace.seen = 0
						trace.mu.Unlock()
						w := usersCall(a, usersOwner, "POST", "/api/actions", string(body))
						trace.mu.Lock()
						calls := trace.seen
						trace.mu.Unlock()
						if w.Code != wantCode {
							t.Fatalf("撤销会话返回 %d，期望 %d：%s", w.Code, wantCode, w.Body.String())
						}
						if calls != wantCalls {
							t.Errorf("撤销会话使用 %d 次数据库调用，期望 %d 次（含事务和审计）", calls, wantCalls)
						}
						if wantCode == 200 {
							var response struct {
								OK       bool
								Affected int
							}
							if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || !response.OK || response.Affected != wantAffected {
								t.Fatal("撤销响应不兼容", response, err)
							}
						}
						changed := 0
						for id, version := range adminSessionVersions(t, db) {
							if before[id] != version {
								changed++
							}
						}
						if changed != wantWrites {
							t.Errorf("第 %d 次撤销实际写入 %d 行，期望 %d 行", attempt+1, changed, wantWrites)
						}
						if _, err := db.Authenticate(ctx, other.AccessToken); err != nil {
							t.Fatal("旁观用户会话被撤销", err)
						}
						if wantCode == 200 && state != "empty" {
							var revoked bool
							if err := db.pool.QueryRow(ctx, `SELECT revoked FROM auth_sessions WHERE id=$1`, p.SessionID).Scan(&revoked); err != nil || !revoked {
								t.Fatal("目标会话没有撤销", revoked, err)
							}
						}
						wantWrites = 0
					}
					wantAudits := 2
					if wantCode != 200 {
						wantAudits = 0
					}
					if n := usersAuditCount(t, db, action); n != wantAudits {
						t.Fatal("撤销审计数量不正确", n, wantAudits)
					}
				})
			}
		})
	}
}

func TestAdminSessionRevocationAuditFailureRollsBack(t *testing.T) {
	for _, action := range []string{"revoke_session", "revoke_sessions"} {
		t.Run(action, func(t *testing.T) {
			db, a := usersTestStore(t)
			ctx := t.Context()
			owner := complete(t, db, Identity{"email", "revoke-audit@example.test"})
			p, err := db.Authenticate(ctx, owner.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_audit ADD CONSTRAINT reject_session_revocation_audit CHECK(action NOT IN ('revoke_session','revoke_sessions')) NOT VALID`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_audit DROP CONSTRAINT reject_session_revocation_audit`); err != nil {
					t.Error(err)
				}
			})
			id := p.SessionID
			if action == "revoke_sessions" {
				id = owner.User.ID
			}
			body, err := json.Marshal(map[string]string{"action": action, "id": id, "user_id": owner.User.ID})
			if err != nil {
				t.Fatal(err)
			}
			before := adminSessionVersions(t, db)
			w := usersCall(a, usersOwner, "POST", "/api/actions", string(body))
			if w.Code != 503 {
				t.Fatal("审计失败没有返回错误", w.Code, w.Body.String())
			}
			if got := adminSessionVersions(t, db)[p.SessionID]; got != before[p.SessionID] {
				t.Fatal("审计失败没有回滚会话写入")
			}
			if _, err := db.Authenticate(ctx, owner.AccessToken); err != nil {
				t.Fatal("审计失败后会话不可用", err)
			}
			if n := usersAuditCount(t, db, action); n != 0 {
				t.Fatal("失败操作写入了审计", n)
			}
		})
	}
}

func TestAdminSessionRevocationRejectsDeletedUserAfterWait(t *testing.T) {
	for _, action := range []string{"revoke_session", "revoke_sessions"} {
		t.Run(action, func(t *testing.T) {
			db, a := usersTestStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			owner := complete(t, db, Identity{"email", "revoke-deleted@example.test"})
			p, err := db.Authenticate(ctx, owner.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `DELETE FROM auth_users WHERE id=$1`, owner.User.ID); err != nil {
				t.Fatal(err)
			}
			id := p.SessionID
			if action == "revoke_sessions" {
				id = owner.User.ID
			}
			body, err := json.Marshal(map[string]string{"action": action, "id": id, "user_id": owner.User.ID})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := adminJSONRequest("POST", "/api/actions", string(body)).WithContext(WithAdminAccess(ctx, usersOwner))
				w := httptest.NewRecorder()
				a.AdminHTTP(w, r)
				done <- w
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if w := <-done; w.Code != 404 {
				t.Fatal("等待期间已删除的用户应返回 404", w.Code, w.Body.String())
			}
			if n := usersAuditCount(t, db, action); n != 0 {
				t.Fatal("已删除用户的撤销操作不应写审计", n)
			}
		})
	}
}

func TestAdminSessionRevocationPreservesConcurrentRevoke(t *testing.T) {
	for _, action := range []string{"revoke_session", "revoke_sessions"} {
		t.Run(action, func(t *testing.T) {
			db, a := usersTestStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			owner := complete(t, db, Identity{"email", "revoke-concurrent@example.test"})
			p, err := db.Authenticate(ctx, owner.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `UPDATE auth_sessions SET revoked=true WHERE id=$1`, p.SessionID); err != nil {
				t.Fatal(err)
			}
			var version string
			if err := blocker.QueryRow(ctx, `SELECT xmin::text FROM auth_sessions WHERE id=$1`, p.SessionID).Scan(&version); err != nil {
				t.Fatal(err)
			}
			id := p.SessionID
			if action == "revoke_sessions" {
				id = owner.User.ID
			}
			body, err := json.Marshal(map[string]string{"action": action, "id": id, "user_id": owner.User.ID})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := adminJSONRequest("POST", "/api/actions", string(body)).WithContext(WithAdminAccess(ctx, usersOwner))
				w := httptest.NewRecorder()
				a.AdminHTTP(w, r)
				done <- w
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			w := <-done
			var response struct{ Affected int }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Affected != 1 {
				t.Fatal("并发撤销的响应不兼容", w.Code, w.Body.String())
			}
			if got := adminSessionVersions(t, db)[p.SessionID]; got != version {
				t.Fatal("锁等待后重写了已撤销会话")
			}
		})
	}
}
