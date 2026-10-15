package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

const feedbackWriteTestID = "00000000-0000-4000-8000-000000000001"

func insertAdminFeedback(t *testing.T, db *Store, status string) {
	t.Helper()
	user := complete(t, db, Identity{"email", "feedback-writes@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO feedback(id,user_id,type,text,platform,app_version,status)
 VALUES($1,$2,'bug','合成反馈','ios','1.0',$3)`, feedbackWriteTestID, user.User.ID, status); err != nil {
		t.Fatal(err)
	}
}

func adminFeedbackState(t *testing.T, db *Store) (string, string) {
	t.Helper()
	var status, version string
	if err := db.pool.QueryRow(t.Context(), `SELECT status,xmin::text FROM feedback WHERE id=$1`, feedbackWriteTestID).Scan(&status, &version); err != nil {
		t.Fatal(err)
	}
	return status, version
}

func TestAdminFeedbackSkipsUnchangedWrites(t *testing.T) {
	db, a := usersTestStore(t)
	insertAdminFeedback(t, db, "new")
	for _, tc := range []struct{ action, status string }{
		{"resolve_feedback", "resolved"},
		{"reopen_feedback", "new"},
	} {
		body, err := json.Marshal(map[string]string{"action": tc.action, "id": feedbackWriteTestID})
		if err != nil {
			t.Fatal(err)
		}
		for attempt := range 2 {
			_, before := adminFeedbackState(t, db)
			w := usersCall(a, usersOwner, "POST", "/api/actions", string(body))
			var response struct {
				OK       bool
				Affected int
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || !response.OK || response.Affected != 1 {
				t.Fatal("反馈状态操作响应不兼容", w.Code, w.Body.String())
			}
			status, after := adminFeedbackState(t, db)
			if status != tc.status {
				t.Fatalf("反馈状态为 %s，期望 %s", status, tc.status)
			}
			if changed := before != after; changed != (attempt == 0) {
				t.Errorf("%s 第 %d 次操作是否重写行=%v，期望 %v", tc.action, attempt+1, changed, attempt == 0)
			}
		}
		if n := usersAuditCount(t, db, tc.action); n != 2 {
			t.Fatal("重复操作应继续逐次写入审计", tc.action, n)
		}
		missing, err := json.Marshal(map[string]string{"action": tc.action, "id": "missing-feedback"})
		if err != nil {
			t.Fatal(err)
		}
		if w := usersCall(a, usersOwner, "POST", "/api/actions", string(missing)); w.Code != 404 {
			t.Fatal("不存在的反馈应返回 404", w.Code, w.Body.String())
		}
		if n := usersAuditCount(t, db, tc.action); n != 2 {
			t.Fatal("不存在的反馈不应产生审计", tc.action, n)
		}
	}
}

func TestAdminFeedbackUsesStatusAfterLockWait(t *testing.T) {
	for _, tc := range []struct{ action, wanted, initial string }{
		{"resolve_feedback", "resolved", "new"},
		{"reopen_feedback", "new", "resolved"},
	} {
		for _, mutation := range []string{"same", "opposite", "delete"} {
			t.Run(tc.action+"/"+mutation, func(t *testing.T) {
				db, a := usersTestStore(t)
				insertAdminFeedback(t, db, tc.initial)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				blocker, err := db.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(context.Background())
				var version string
				if mutation == "delete" {
					_, err = blocker.Exec(ctx, `DELETE FROM feedback WHERE id=$1`, feedbackWriteTestID)
				} else {
					status := tc.wanted
					if mutation == "opposite" {
						status = tc.initial
					}
					err = blocker.QueryRow(ctx, `UPDATE feedback SET status=$2 WHERE id=$1 RETURNING xmin::text`, feedbackWriteTestID, status).Scan(&version)
				}
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(map[string]string{"action": tc.action, "id": feedbackWriteTestID})
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
				wantCode, wantAudits := 200, 1
				if mutation == "delete" {
					wantCode, wantAudits = 404, 0
				}
				if w.Code != wantCode {
					t.Fatalf("锁等待后返回 %d，期望 %d：%s", w.Code, wantCode, w.Body.String())
				}
				if mutation != "delete" {
					var response struct{ Affected int }
					if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Affected != 1 {
						t.Fatal("锁等待后响应不兼容", response, err)
					}
					status, after := adminFeedbackState(t, db)
					if status != tc.wanted || (after == version) != (mutation == "same") {
						t.Fatal("锁等待后未按最新状态决定是否写入", status, after, version)
					}
				}
				if n := usersAuditCount(t, db, tc.action); n != wantAudits {
					t.Fatal("锁等待后的审计数量不正确", n, wantAudits)
				}
			})
		}
	}
}
