package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type statusUndoRequest struct {
	Action  string          `json:"action"`
	ID      string          `json:"id"`
	Section string          `json:"section,omitempty"`
	Value   json.RawMessage `json:"value"`
}

func statusUndoCall(t *testing.T, a *Service, request any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return usersCall(a, usersOwner, "POST", "/api/actions", string(body))
}

func statusUndoFrom(t *testing.T, w *httptest.ResponseRecorder) statusUndoRequest {
	t.Helper()
	var response struct {
		Undo statusUndoRequest `json:"undo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || response.Undo.Action == "" || len(response.Undo.Value) == 0 {
		t.Fatalf("缺少有效撤销：%d %s，错误=%v", w.Code, w.Body.String(), err)
	}
	return response.Undo
}

func feedbackStatusCall(t *testing.T, a *Service, action string) *httptest.ResponseRecorder {
	t.Helper()
	return statusUndoCall(t, a, map[string]any{"action": action, "id": feedbackWriteTestID})
}

func TestFeedbackUndoVersions(t *testing.T) {
	for _, action := range []string{"resolve_feedback", "reopen_feedback"} {
		for _, mutation := range []string{"none", "roundtrip", "same", "sql", "deleted", "recreated"} {
			t.Run(action+"/"+mutation, func(t *testing.T) {
				db, a := usersTestStore(t)
				initial, opposite := "new", "reopen_feedback"
				if action == "reopen_feedback" {
					initial, opposite = "resolved", "resolve_feedback"
				}
				insertAdminFeedback(t, db, initial)
				undo := statusUndoFrom(t, feedbackStatusCall(t, a, action))
				if undo.Action != "undo_feedback_status" {
					t.Fatal("撤销未使用独立动作", undo)
				}
				want := 409
				switch mutation {
				case "none":
					want = 200
				case "roundtrip":
					for _, next := range []string{opposite, action} {
						if w := feedbackStatusCall(t, a, next); w.Code != 200 {
							t.Fatal(w.Code, w.Body.String())
						}
					}
				case "same":
					_, before := adminFeedbackState(t, db)
					if w := feedbackStatusCall(t, a, action); w.Code != 200 {
						t.Fatal(w.Code, w.Body.String())
					}
					if _, after := adminFeedbackState(t, db); before != after {
						t.Fatal("同值操作重写了主行")
					}
				case "sql":
					if _, err := db.pool.Exec(t.Context(), `UPDATE feedback SET status=status WHERE id=$1`, feedbackWriteTestID); err != nil {
						t.Fatal(err)
					}
				case "deleted", "recreated":
					if _, err := db.pool.Exec(t.Context(), `DELETE FROM feedback WHERE id=$1`, feedbackWriteTestID); err != nil {
						t.Fatal(err)
					}
					want = 404
					if mutation == "recreated" {
						insertAdminFeedback(t, db, initial)
						want = 409
					}
				}
				before := usersAuditCount(t, db, undo.Action)
				w := statusUndoCall(t, a, undo)
				if w.Code != want {
					t.Fatalf("撤销=%d %s，期望=%d", w.Code, w.Body.String(), want)
				}
				if want == 200 {
					before++
					if status, _ := adminFeedbackState(t, db); status != initial {
						t.Fatal("未恢复真实前态", status, initial)
					}
					if replay := statusUndoCall(t, a, undo); replay.Code != 409 {
						t.Fatal("重复撤销未拒绝", replay.Code, replay.Body.String())
					}
				}
				if after := usersAuditCount(t, db, undo.Action); before != after {
					t.Fatal("成功审计数量错误", before, after)
				}
			})
		}
	}
}

func TestFeedbackUnchangedStatusHasNoUndo(t *testing.T) {
	for _, tc := range []struct{ status, action string }{{"new", "reopen_feedback"}, {"resolved", "resolve_feedback"}} {
		t.Run(tc.status, func(t *testing.T) {
			db, a := usersTestStore(t)
			insertAdminFeedback(t, db, tc.status)
			w := feedbackStatusCall(t, a, tc.action)
			var response map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response["undo"] != nil {
				t.Fatal("同值操作不应提供反向撤销", w.Code, w.Body.String())
			}
		})
	}
}

func TestFeedbackUndoAfterLockWait(t *testing.T) {
	db, a := usersTestStore(t)
	insertAdminFeedback(t, db, "new")
	undo := statusUndoFrom(t, feedbackStatusCall(t, a, "resolve_feedback"))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	// 同值操作只加行锁和审计；等待者必须在取锁后用新快照读取审计版本。
	if _, err = blocker.Exec(ctx, `SELECT id FROM feedback WHERE id=$1 FOR NO KEY UPDATE`, feedbackWriteTestID); err != nil {
		t.Fatal(err)
	}
	if err = auditTx(ctx, blocker, "resolve_feedback", feedbackWriteTestID, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- statusUndoCall(t, a, undo) }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if w := <-done; w.Code != 409 {
		t.Fatal("等待锁后遗漏同值审计", w.Code, w.Body.String())
	}
}

func TestFeedbackUndoValidation(t *testing.T) {
	db, a := usersTestStore(t)
	insertAdminFeedback(t, db, "new")
	for _, value := range []any{nil, true, map[string]any{}, map[string]any{"token": strings.Repeat("a", 32), "status": "bad"}, map[string]any{"token": "bad", "status": "new"}, map[string]any{"token": strings.Repeat("g", 32), "status": "new"}, map[string]any{"token": strings.Repeat("a", 32), "status": "new", "extra": true}} {
		w := statusUndoCall(t, a, map[string]any{"action": "undo_feedback_status", "id": feedbackWriteTestID, "value": value})
		if w.Code != 400 {
			t.Fatal(value, w.Code, w.Body.String())
		}
	}
	undo := statusUndoFrom(t, feedbackStatusCall(t, a, "resolve_feedback"))
	encoded, err := json.Marshal(undo)
	if err != nil {
		t.Fatal(err)
	}
	if w := usersCall(a, AdminAccess{Role: "readonly"}, "POST", "/api/actions", string(encoded)); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := statusUndoCall(t, a, map[string]any{"action": undo.Action, "id": undo.ID, "ids": []string{undo.ID}, "value": undo.Value}); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func seedCategoryUndo(t *testing.T, db *Store, section string) string {
	t.Helper()
	user := complete(t, db, Identity{"email", "category-undo@example.test"})
	id := "cd334455-1234-4234-8234-123456789abc"
	if section == "candidate-skins" {
		insertCandidateSkin(t, db, id, user.User.ID, "合成候选皮肤")
	} else if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'合成皮肤','{}')`, id, user.User.ID); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCategoryUndoVersions(t *testing.T) {
	for _, section := range []string{"skins", "candidate-skins"} {
		for _, mutation := range []string{"none", "roundtrip", "same", "sql", "deleted", "recreated", "batch"} {
			t.Run(section+"/"+mutation, func(t *testing.T) {
				db, a := usersTestStore(t)
				id := seedCategoryUndo(t, db, section)
				action := "set_skin_category"
				if section == "candidate-skins" {
					action = "set_candidate_skin_category"
				}
				call := func(value any) *httptest.ResponseRecorder {
					return statusUndoCall(t, a, map[string]any{"action": action, "id": id, "value": value})
				}
				undo := statusUndoFrom(t, call(map[string]any{"category": "tech"}))
				want := 409
				switch mutation {
				case "none":
					want = 200
				case "roundtrip":
					for _, category := range []string{"cute", "tech"} {
						if w := call(map[string]any{"category": category}); w.Code != 200 {
							t.Fatal(w.Code, w.Body.String())
						}
					}
				case "same":
					if w := call(map[string]any{"category": "tech"}); w.Code != 200 {
						t.Fatal(w.Code, w.Body.String())
					}
				case "sql":
					if _, err := db.pool.Exec(t.Context(), `UPDATE `+moderationSections[section].table+` SET category=category WHERE id=$1`, id); err != nil {
						t.Fatal(err)
					}
				case "deleted", "recreated":
					if _, err := db.pool.Exec(t.Context(), `DELETE FROM `+moderationSections[section].table+` WHERE id=$1`, id); err != nil {
						t.Fatal(err)
					}
					if mutation == "recreated" {
						seedCategoryUndo(t, db, section)
					}
				case "batch":
					w := statusUndoCall(t, a, map[string]any{"action": action, "ids": []string{id}, "value": map[string]any{"category": "cute"}})
					var response map[string]any
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response["undo"] != nil {
						t.Fatal("批量操作不应返回单项撤销", w.Code, w.Body.String())
					}
				}
				before := usersAuditCount(t, db, action)
				w := statusUndoCall(t, a, undo)
				if w.Code != want {
					t.Fatalf("撤销=%d %s，期望=%d", w.Code, w.Body.String(), want)
				}
				if want == 200 {
					before++
					var category string
					if err := db.pool.QueryRow(t.Context(), `SELECT category FROM `+moderationSections[section].table+` WHERE id=$1`, id).Scan(&category); err != nil || category != "other" {
						t.Fatal("未恢复真实前态", category, err)
					}
					if replay := statusUndoCall(t, a, undo); replay.Code != 409 {
						t.Fatal("重复撤销未拒绝", replay.Code, replay.Body.String())
					}
				}
				if after := usersAuditCount(t, db, action); after != before {
					t.Fatal("审计数量错误", before, after)
				}
			})
		}
	}
}

func TestCategoryUndoValidationAndLockWait(t *testing.T) {
	for _, section := range []string{"skins", "candidate-skins"} {
		t.Run(section, func(t *testing.T) {
			db, a := usersTestStore(t)
			id := seedCategoryUndo(t, db, section)
			action := "set_skin_category"
			if section == "candidate-skins" {
				action = "set_candidate_skin_category"
			}
			for _, token := range []any{"", "bad", strings.Repeat("g", 32), true} {
				w := statusUndoCall(t, a, map[string]any{"action": action, "id": id, "value": map[string]any{"category": "cute", "token": token}})
				if w.Code != 400 {
					t.Fatal("接受了无效版本", token, w.Code, w.Body.String())
				}
			}
			w := statusUndoCall(t, a, map[string]any{"action": action, "id": id, "value": map[string]any{"category": "other"}})
			var unchanged map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &unchanged) != nil || unchanged["undo"] != nil {
				t.Fatal("同值分类操作不应返回撤销", w.Code, w.Body.String())
			}
			undo := statusUndoFrom(t, statusUndoCall(t, a, map[string]any{"action": action, "id": id, "value": map[string]any{"category": "tech"}}))
			if w := statusUndoCall(t, a, map[string]any{"action": action, "ids": []string{id}, "value": undo.Value}); w.Code != 400 {
				t.Fatal("版本撤销接受了批量目标", w.Code, w.Body.String())
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err = blocker.Exec(ctx, `UPDATE `+moderationSections[section].table+` SET category=category WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- statusUndoCall(t, a, undo) }()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if w := <-done; w.Code != 409 {
				t.Fatal("锁等待后未重新校验分类版本", w.Code, w.Body.String())
			}
		})
	}
}

func TestFeedbackUndoIndexMigration(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX admin_audit_feedback_version`); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(t.Context()); err == nil {
		t.Fatal("缺少反馈版本索引时仍就绪")
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

func TestFeedbackUndoRollsBackOnAuditFailure(t *testing.T) {
	db, a := usersTestStore(t)
	insertAdminFeedback(t, db, "new")
	undo := statusUndoFrom(t, feedbackStatusCall(t, a, "resolve_feedback"))
	if _, err := db.pool.Exec(t.Context(), `ALTER TABLE admin_audit ADD CONSTRAINT reject_feedback_undo CHECK(action<>'undo_feedback_status') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_audit DROP CONSTRAINT reject_feedback_undo`); err != nil {
			t.Error(err)
		}
	})
	if w := statusUndoCall(t, a, undo); w.Code != 503 {
		t.Fatal("审计失败未返回错误", w.Code, w.Body.String())
	}
	if status, _ := adminFeedbackState(t, db); status != "resolved" {
		t.Fatal("审计失败仍修改了状态", status)
	}
	if n := usersAuditCount(t, db, undo.Action); n != 0 {
		t.Fatal("失败撤销写入成功审计", n)
	}
}

func TestFeedbackUndoAuditLookupUsesIndex(t *testing.T) {
	db := testStore(t)
	insertAdminFeedback(t, db, "new")
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_audit;
INSERT INTO admin_audit(action,target) SELECT 'resolve_feedback','synthetic-other-'||n FROM generate_series(1,2000) n;
ANALYZE admin_audit; ANALYZE feedback`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.pool.Query(t.Context(), `EXPLAIN (COSTS OFF) SELECT `+feedbackVersionSQL+` FROM feedback f WHERE f.id=$1`, feedbackWriteTestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "admin_audit_feedback_version") {
		t.Fatal("反馈版本查询未使用目标索引", plan.String())
	}
}
