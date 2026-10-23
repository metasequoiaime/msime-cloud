package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func banUndoCall(t *testing.T, a *Service, action, id, reason string, value any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"action": action, "id": id}
	if reason != "" {
		body["reason"] = reason
	}
	if value != nil {
		body["value"] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return usersCall(a, usersOwner, "POST", "/api/actions", string(encoded))
}

type banUndoPayload struct {
	Action string          `json:"action"`
	ID     string          `json:"id"`
	Reason string          `json:"reason"`
	Value  json.RawMessage `json:"value"`
}

func banUndoRequest(t *testing.T, w *httptest.ResponseRecorder) banUndoPayload {
	t.Helper()
	var response struct {
		Undo banUndoPayload `json:"undo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || response.Undo.ID == "" || len(response.Undo.Value) == 0 {
		t.Fatalf("缺少有效封禁撤销请求：%d %s，错误=%v", w.Code, w.Body.String(), err)
	}
	return response.Undo
}

func TestBanUndoPreservesLaterBan(t *testing.T) {
	db, a := usersTestStore(t)
	user := complete(t, db, Identity{"email", "ban-undo@example.test"})
	undo := banUndoRequest(t, banUndoCall(t, a, "ban_user", user.User.ID, "首次合成原因", nil))
	if w := banUndoCall(t, a, "unban_user", user.User.ID, "", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := banUndoCall(t, a, "ban_user", user.User.ID, "后续合成原因", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := banUndoCall(t, a, undo.Action, undo.ID, undo.Reason, undo.Value); w.Code != 409 {
		t.Fatal("旧撤销未拒绝", w.Code, w.Body.String())
	}
	var reason string
	if err := db.pool.QueryRow(t.Context(), `SELECT ban_reason FROM auth_users WHERE id=$1 AND banned_at IS NOT NULL`, user.User.ID).Scan(&reason); err != nil || reason != "后续合成原因" {
		t.Fatal("旧撤销覆盖了新封禁", reason, err)
	}
}

func TestUnbanUndoUsesActualReason(t *testing.T) {
	db, a := usersTestStore(t)
	user := complete(t, db, Identity{"email", "unban-undo@example.test"})
	if w := banUndoCall(t, a, "ban_user", user.User.ID, "数据库中的合成原因", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	undo := banUndoRequest(t, banUndoCall(t, a, "unban_user", user.User.ID, "", nil))
	if undo.Action != "undo_user_ban" || undo.Reason != "数据库中的合成原因" {
		t.Fatal("撤销未保存实际封禁原因", undo)
	}
	if w := banUndoCall(t, a, undo.Action, undo.ID, undo.Reason, undo.Value); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := banUndoCall(t, a, undo.Action, undo.ID, undo.Reason, undo.Value); w.Code != 409 {
		t.Fatal("重复撤销未拒绝", w.Code, w.Body.String())
	}
}

func TestBanUndoVersions(t *testing.T) {
	for _, scenario := range []string{"ban_success", "unban_roundtrip", "profile", "direct_sql", "deleted", "recreated"} {
		t.Run(scenario, func(t *testing.T) {
			db, a := usersTestStore(t)
			user := complete(t, db, Identity{"email", "ban-version@example.test"})
			undo := banUndoRequest(t, banUndoCall(t, a, "ban_user", user.User.ID, "合成原因", nil))
			want := 409
			switch scenario {
			case "ban_success":
				want = 200
			case "unban_roundtrip":
				undo = banUndoRequest(t, banUndoCall(t, a, "unban_user", user.User.ID, "", nil))
				if w := banUndoCall(t, a, "ban_user", user.User.ID, "后续封禁", nil); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				if w := banUndoCall(t, a, "unban_user", user.User.ID, "", nil); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "profile":
				if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET display_name='合成新昵称' WHERE id=$1`, user.User.ID); err != nil {
					t.Fatal(err)
				}
				want = 200
			case "direct_sql":
				if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET ban_reason='直接 SQL 的后续原因' WHERE id=$1`, user.User.ID); err != nil {
					t.Fatal(err)
				}
			case "deleted", "recreated":
				if _, err := db.pool.Exec(t.Context(), `DELETE FROM auth_users WHERE id=$1`, user.User.ID); err != nil {
					t.Fatal(err)
				}
				want = 404
				if scenario == "recreated" {
					if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id,created_at,banned_at,ban_reason) VALUES($1,'2000-01-01',now(),'重新创建')`, user.User.ID); err != nil {
						t.Fatal(err)
					}
					want = 409
				}
			}
			before := usersAuditCount(t, db, undo.Action)
			w := banUndoCall(t, a, undo.Action, undo.ID, undo.Reason, undo.Value)
			if w.Code != want {
				t.Fatalf("%s 撤销=%d %s，期望=%d", scenario, w.Code, w.Body.String(), want)
			}
			audits := before
			if want == 200 {
				audits++
				if replay := banUndoCall(t, a, undo.Action, undo.ID, undo.Reason, undo.Value); replay.Code != 409 {
					t.Fatal("重复撤销未拒绝", replay.Code, replay.Body.String())
				}
			}
			if after := usersAuditCount(t, db, undo.Action); after != audits {
				t.Fatal("撤销审计数量错误", before, after, audits)
			}
		})
	}
}

func TestBanUndoValidationAndPermissions(t *testing.T) {
	db, a := usersTestStore(t)
	user := complete(t, db, Identity{"email", "ban-invalid@example.test"})
	for _, value := range []any{map[string]any{}, map[string]any{"token": ""}, map[string]any{"token": strings.Repeat("g", 32)}, map[string]any{"token": strings.Repeat("a", 32), "extra": true}, 123, true, []string{"token"}} {
		if w := banUndoCall(t, a, "ban_user", user.User.ID, "合成原因", value); w.Code != 400 {
			t.Fatal(value, w.Code, w.Body.String())
		}
	}
	undo := banUndoRequest(t, banUndoCall(t, a, "ban_user", user.User.ID, "合成原因", nil))
	for _, value := range []any{nil, map[string]any{}, map[string]any{"token": strings.Repeat("a", 32)}, map[string]any{"token": strings.Repeat("a", 32), "banned": nil}, map[string]any{"token": "bad", "banned": false}, map[string]any{"token": strings.Repeat("g", 32), "banned": false}, map[string]any{"token": strings.Repeat("a", 32), "banned": false, "extra": true}, 123, true, []string{"token"}} {
		if w := banUndoCall(t, a, "undo_user_ban", user.User.ID, "", value); w.Code != 400 {
			t.Fatal("撤销接受了无效参数", value, w.Code, w.Body.String())
		}
	}
	for _, request := range []actionRequest{
		{Action: undo.Action, ID: undo.ID, IDs: []string{undo.ID}, Value: undo.Value},
		{Action: undo.Action, IDs: []string{undo.ID}, Value: undo.Value},
		{Action: undo.Action, ID: undo.ID, Reason: "解封不接受原因", Value: undo.Value},
		{Action: undo.Action, ID: undo.ID, Value: json.RawMessage(`{"token":"` + strings.Repeat("a", 32) + `","banned":true}`)},
	} {
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if w := usersCall(a, usersOwner, "POST", "/api/actions", string(encoded)); w.Code != 400 {
			t.Fatal("撤销接受了无效请求", request, w.Code, w.Body.String())
		}
	}
	body, err := json.Marshal(undo)
	if err != nil {
		t.Fatal(err)
	}
	access := AdminAccess{Actor: "pat:reader@example.test", Email: "reader@example.test", Role: "readonly", Permissions: []string{PermViewCloudUsage}}
	if w := usersCall(a, access, "POST", "/api/actions", string(body)); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := banUndoCall(t, a, "unban_user", user.User.ID, "", map[string]any{"token": "bad"}); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestBanUndoRechecksAfterLock(t *testing.T) {
	db, a := usersTestStore(t)
	user := complete(t, db, Identity{"email", "ban-lock@example.test"})
	undo := banUndoRequest(t, banUndoCall(t, a, "ban_user", user.User.ID, "合成原因", nil))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, `UPDATE auth_users SET ban_reason='锁内后续原因' WHERE id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- banUndoCall(t, a, undo.Action, undo.ID, undo.Reason, undo.Value) }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if w := <-done; w.Code != 409 {
		t.Fatal("锁等待后未重新校验版本", w.Code, w.Body.String())
	}
}

func TestBanRevisionMigration(t *testing.T) {
	db := testStore(t)
	for _, mutation := range []string{`DROP TRIGGER auth_user_ban_revision ON auth_users`, `ALTER TABLE auth_users DISABLE TRIGGER auth_user_ban_revision`, `ALTER TABLE auth_users DROP COLUMN ban_revision`} {
		if _, err := db.pool.Exec(t.Context(), mutation); err != nil {
			t.Fatal(err)
		}
		if err := db.Ready(t.Context()); err == nil {
			t.Fatal("封禁版本缺失时仍就绪", mutation)
		}
		for range 2 {
			if err := db.Migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Ready(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBanRevisionWithDMLRole(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "ban-dml@example.test"})
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	role := pgx.Identifier{"ban_dml_" + randomToken()}.Sanitize()
	var schema string
	if err = tx.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `CREATE ROLE `+role+` NOLOGIN; GRANT USAGE ON SCHEMA `+pgx.Identifier{schema}.Sanitize()+` TO `+role+`; GRANT SELECT,INSERT,UPDATE,DELETE ON auth_users TO `+role+`; SET LOCAL ROLE `+role); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err = tx.QueryRow(t.Context(), `UPDATE auth_users SET banned_at=now() WHERE id=$1 RETURNING ban_revision`, user.User.ID).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("DML 封禁版本未递增", revision, err)
	}
}
