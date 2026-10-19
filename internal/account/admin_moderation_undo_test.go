package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModerationUndoRejectsLaterRemoval(t *testing.T) {
	db, _, _, _, call := moderationFixture(t)
	first := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"第一次下架"}`)
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	var response struct {
		Undo json.RawMessage `json:"undo"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"再次下架"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	undo := string(response.Undo)
	if undo == "" {
		t.Fatal("缺少服务端撤销快照")
	}
	if w := call("POST", "/api/actions", undo); w.Code != 409 || !strings.Contains(w.Body.String(), "conflict") {
		t.Fatal("过期撤销未被拒绝", w.Code, w.Body.String())
	}
	state, _, reason, _ := moderationState(t, db, "community_skins", "skin-a")
	if state != "removed" || reason == nil || *reason != "再次下架" {
		t.Fatal(state, reason)
	}
}

func moderationUndoBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var result struct {
		Undo json.RawMessage `json:"undo"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Undo) == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	return string(result.Undo)
}

func seedModerationUndoSection(t *testing.T, db *Store, owner Tokens, section string) string {
	t.Helper()
	switch section {
	case "skins":
		return "skin-a"
	case "dictionaries":
		return "dict-a"
	case "replies":
		return "reply-a"
	case "phrases":
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('phrase-a',$1,'phrase','短语','{"phrases":[{"text":"你好"}]}')`, owner.User.ID); err != nil {
			t.Fatal(err)
		}
		return "phrase-a"
	case "plugins":
		const id = "b4334455-1234-4234-8234-123456789abc"
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES($1,$2,'sound','com.example.undo','插件','1','MIT','{}','zip',repeat('a',64))`, id, owner.User.ID); err != nil {
			t.Fatal(err)
		}
		return id
	default:
		const id = "b4334455-1234-4234-8234-123456789abd"
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES($1,$2,'undo','候选皮肤','1','MIT','{}','preview.png',repeat('a',64))`, id, owner.User.ID); err != nil {
			t.Fatal(err)
		}
		return id
	}
}

func TestModerationUndoRestoresDatabaseSnapshot(t *testing.T) {
	for section, table := range moderationSections {
		for _, tc := range []struct{ action, before string }{
			{"approve_content", "pending"}, {"approve_content", "removed"}, {"restore_content", "removed"}, {"remove_content", "pending"}, {"remove_content", "approved"}, {"remove_content", "removed"},
		} {
			t.Run(section+"/"+tc.action+"/"+tc.before, func(t *testing.T) {
				db, _, owner, _, call := moderationFixture(t)
				id := seedModerationUndoSection(t, db, owner, section)
				if _, err := db.pool.Exec(t.Context(), `UPDATE `+table.table+` SET moderation=$2,previous_moderation=CASE WHEN $2='removed' THEN 'pending' END,moderation_reason='原始原因' WHERE id=$1`, id, tc.before); err != nil {
					t.Fatal(err)
				}
				request, _ := json.Marshal(actionRequest{Action: tc.action, Section: section, ID: id, Reason: "本次下架"})
				undo := moderationUndoBody(t, call("POST", "/api/actions", string(request)))
				if w := call("POST", "/api/actions", undo); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				state, previous, reason, by := moderationState(t, db, table.table, id)
				if state != tc.before || (tc.before == "removed" && (previous == nil || *previous != "pending")) || (tc.before != "removed" && previous != nil) || reason == nil || *reason != "原始原因" || by == nil || *by != "legacy-token" {
					t.Fatal(state, previous, reason, by)
				}
				if w := call("POST", "/api/actions", undo); w.Code != 409 {
					t.Fatal("重复撤销未冲突", w.Code, w.Body.String())
				}
				var n int
				if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit WHERE action='undo_moderation'`).Scan(&n); err != nil || n != 1 {
					t.Fatal(n, err)
				}
			})
		}
	}
}

func TestModerationUndoRejectsEditsAndReplacements(t *testing.T) {
	for section, table := range moderationSections {
		for _, change := range []string{"edit", "replace"} {
			t.Run(section+"/"+change, func(t *testing.T) {
				db, _, owner, _, call := moderationFixture(t)
				id := seedModerationUndoSection(t, db, owner, section)
				request, _ := json.Marshal(actionRequest{Action: "remove_content", Section: section, ID: id, Reason: "下架"})
				undo := moderationUndoBody(t, call("POST", "/api/actions", string(request)))
				query := `UPDATE ` + table.table + ` SET name='作者新编辑' WHERE id=$1`
				if change == "replace" {
					var columns string
					if err := db.pool.QueryRow(t.Context(), `SELECT string_agg(quote_ident(attname),',' ORDER BY attnum) FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated=''`, table.table).Scan(&columns); err != nil {
						t.Fatal(err)
					}
					query = `WITH deleted AS (DELETE FROM ` + table.table + ` WHERE id=$1 RETURNING *) INSERT INTO ` + table.table + ` (` + columns + `) SELECT ` + columns + ` FROM deleted`
				}
				if _, err := db.pool.Exec(t.Context(), query, id); err != nil {
					t.Fatal(err)
				}
				if w := call("POST", "/api/actions", undo); w.Code != 409 || !strings.Contains(w.Body.String(), "conflict") {
					t.Fatal(w.Code, w.Body.String())
				}
				state, _, _, _ := moderationState(t, db, table.table, id)
				if state != "removed" {
					t.Fatal(state)
				}
			})
		}
	}
}

func TestModerationUndoWaitsForLatestSnapshot(t *testing.T) {
	db, a, _, _, call := moderationFixture(t)
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = actionRemoveContent(a, t.Context(), tx, actionRequest{Section: "skins", ID: "skin-a", Reason: "先提交的原因"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"后提交的原因"}`)
	}()
	waitForAdminDatabaseLock(t, db, tx.Conn().PgConn().PID())
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	undo := moderationUndoBody(t, <-done)
	if w := call("POST", "/api/actions", undo); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	state, previous, reason, _ := moderationState(t, db, "community_skins", "skin-a")
	if state != "removed" || previous == nil || *previous != "pending" || reason == nil || *reason != "先提交的原因" {
		t.Fatal(state, previous, reason)
	}
}

func TestModerationUndoValidationAndBannedOwner(t *testing.T) {
	db, a, owner, _, call := moderationFixture(t)
	undo := moderationUndoBody(t, call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"下架"}`))
	for _, body := range []string{
		`{"action":"undo_moderation","section":"skins","id":"skin-a"}`,
		`{"action":"undo_moderation","section":"skins","ids":["skin-a"],"value":{}}`,
		strings.Replace(undo, `"section":"skins"`, `"section":"unknown"`, 1),
		strings.Replace(undo, `"moderation":"pending"`, `"moderation":"invalid"`, 1),
		strings.Replace(undo, `"previous":null`, `"previous":"removed"`, 1),
		strings.Replace(undo, `"reason":null`, `"reason":"\u0000"`, 1),
	} {
		if w := call("POST", "/api/actions", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String(), body)
		}
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=now() WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/api/actions", undo); w.Code != 409 || !strings.Contains(w.Body.String(), "owner_banned") {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=NULL WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/api/actions", undo); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// 撤销仍走统一权限检查。
	r := jsonRequest("POST", "/api/actions", undo, "")
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	// 批量审核保持原有语义，不返回单项撤销。
	w = call("POST", "/api/actions", `{"action":"remove_content","section":"skins","ids":["skin-a","skin-b"],"reason":"批量下架"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"undo"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestModerationUndoRescreensPendingSnapshot(t *testing.T) {
	db, a, _, _, call := moderationFixture(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_sensitive_words,admin_sensitive_hits`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `TRUNCATE admin_sensitive_words,admin_sensitive_hits`)
	})
	undo := moderationUndoBody(t, call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"人工下架原因"}`))
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_sensitive_words(pattern,category,level,created_by) VALUES('春日','ad','review','test')`); err != nil {
		t.Fatal(err)
	}
	a.sensitive.invalidate()
	if w := call("POST", "/api/actions", undo); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	state, _, reason, _ := moderationState(t, db, "community_skins", "skin-a")
	if state != "pending" || reason == nil || *reason != "命中敏感词：「春日」" {
		t.Fatal(state, reason)
	}
}

func TestModerationUndoWaitsForConcurrentChanges(t *testing.T) {
	for _, change := range []string{"edit", "ban", "owner"} {
		t.Run(change, func(t *testing.T) {
			db, a, owner, other, call := moderationFixture(t)
			undo := moderationUndoBody(t, call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"下架"}`))
			tx, err := db.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			switch change {
			case "ban":
				_, err = actionBanUser(a, t.Context(), tx, actionRequest{ID: owner.User.ID, Reason: "封禁"})
			case "owner":
				_, err = tx.Exec(t.Context(), `UPDATE community_skins SET owner_id=$1 WHERE id='skin-a'`, other.User.ID)
			default:
				_, err = tx.Exec(t.Context(), `UPDATE community_skins SET name='新的编辑' WHERE id='skin-a'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- call("POST", "/api/actions", undo) }()
			waitForAdminDatabaseLock(t, db, tx.Conn().PgConn().PID())
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			code := "conflict"
			if change == "ban" {
				// 封禁不会改写已下架主行，撤销由最新作者状态拒绝。
				code = "owner_banned"
			}
			if w := <-done; w.Code != 409 || !strings.Contains(w.Body.String(), code) {
				t.Fatal(w.Code, w.Body.String())
			}
			state, _, _, _ := moderationState(t, db, "community_skins", "skin-a")
			if state != "removed" {
				t.Fatal(state)
			}
		})
	}
}
