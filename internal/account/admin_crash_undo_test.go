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

func crashStatusAction(t *testing.T, a *Service, signature string, value any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"action": "crash_group_status", "id": signature, "value": value})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.AdminHTTP(w, adminJSONRequest("POST", "/api/actions", string(body)))
	return w
}

func crashStatusUndo(t *testing.T, a *Service, signature string) json.RawMessage {
	t.Helper()
	w := crashStatusAction(t, a, signature, "known")
	var result struct {
		Undo struct {
			Action string          `json:"action"`
			ID     string          `json:"id"`
			Value  json.RawMessage `json:"value"`
		} `json:"undo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Undo.Action != "crash_group_status" || result.Undo.ID != signature || len(result.Undo.Value) == 0 {
		t.Fatalf("缺少有效撤销请求：%d %s，错误=%v", w.Code, w.Body.String(), err)
	}
	return result.Undo.Value
}

func TestCrashGroupStatusUndo(t *testing.T) {
	for _, scenario := range []string{"success", "telemetry", "new_status", "same_status", "status_roundtrip", "issue", "issue_attempt", "recreated"} {
		t.Run(scenario, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			resetCrashTables(t, a)
			signature := seedCrash(t, a, "crash-undo-00001", "ios", "1", "合成崩溃", "", "", time.Minute)
			undo := crashStatusUndo(t, a, signature)
			ctx := t.Context()
			wantCode, wantStatus := 409, "known"
			switch scenario {
			case "success":
				wantCode, wantStatus = 200, "open"
			case "telemetry":
				seedCrash(t, a, "crash-undo-00002", "ios", "2", "合成崩溃", "", "", 0)
				wantCode, wantStatus = 200, "open"
			case "new_status", "same_status", "status_roundtrip":
				next := "known"
				if scenario != "same_status" {
					next = "fixed"
				}
				if w := crashStatusAction(t, a, signature, next); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				wantStatus = next
				if scenario == "status_roundtrip" {
					if w := crashStatusAction(t, a, signature, "known"); w.Code != 200 {
						t.Fatal(w.Code, w.Body.String())
					}
					wantStatus = "known"
				}
			case "issue":
				if err := a.SetCrashGroupIssue(ctx, signature, "synthetic/ios", 1, "https://example.test/issues/1"); err != nil {
					t.Fatal(err)
				}
			case "issue_attempt":
				if w := crashStatusAction(t, a, signature, "open"); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				locked, err := a.LockCrashGroupIssue(ctx, signature)
				if err != nil {
					t.Fatal(err)
				}
				defer locked.Release()
				if err = locked.Start(ctx, "synthetic/ios"); err != nil {
					t.Fatal(err)
				}
			case "recreated":
				if _, err := db.pool.Exec(ctx, `DELETE FROM admin_crash_groups WHERE signature=$1`, signature); err != nil {
					t.Fatal(err)
				}
				seedCrash(t, a, "crash-undo-00003", "ios", "1", "合成崩溃", "", "", 0)
				if w := crashStatusAction(t, a, signature, "known"); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			var auditsBefore, auditsAfter int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE target=$1`, signature).Scan(&auditsBefore); err != nil {
				t.Fatal(err)
			}
			w := crashStatusAction(t, a, signature, undo)
			if w.Code != wantCode {
				t.Fatalf("撤销响应=%d %s，期望=%d", w.Code, w.Body.String(), wantCode)
			}
			var status string
			if err := db.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM admin_audit WHERE target=$1) FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&status, &auditsAfter); err != nil || status != wantStatus {
				t.Fatalf("撤销后状态=%q，期望=%q，错误=%v", status, wantStatus, err)
			}
			wantAudits := auditsBefore
			if wantCode == 200 {
				wantAudits++
				if replay := crashStatusAction(t, a, signature, undo); replay.Code != 409 {
					t.Fatal("重复撤销未拒绝", replay.Code, replay.Body.String())
				}
			}
			if auditsAfter != wantAudits {
				t.Fatalf("审计数=%d，期望=%d", auditsAfter, wantAudits)
			}
		})
	}
}

func TestCrashIssueRejectPreservesLaterTriage(t *testing.T) {
	for _, next := range []string{"known", "fixed", "roundtrip"} {
		t.Run(next, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			resetCrashTables(t, a)
			signature := seedCrash(t, a, "crash-reject-001", "ios", "1", "合成崩溃", "", "", time.Minute)
			locked, err := a.LockCrashGroupIssue(t.Context(), signature)
			if err != nil {
				t.Fatal(err)
			}
			defer locked.Release()
			if err = locked.Start(t.Context(), "synthetic/ios"); err != nil {
				t.Fatal(err)
			}
			status := next
			if next == "roundtrip" {
				status = "fixed"
			}
			if w := crashStatusAction(t, a, signature, status); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if next == "roundtrip" {
				status = "known"
				if w := crashStatusAction(t, a, signature, status); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if err = locked.Reject(t.Context(), "synthetic/ios"); err != nil {
				t.Fatal(err)
			}
			var got string
			if err = db.pool.QueryRow(context.Background(), `SELECT status FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&got); err != nil || got != status {
				t.Fatalf("拒绝回滚覆盖了后续分诊：状态=%s，期望=%s，错误=%v", got, status, err)
			}
		})
	}
}

func TestCrashIssueRecordPreservesLaterTriage(t *testing.T) {
	for _, status := range []string{"open", "known", "fixed"} {
		t.Run(status, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			resetCrashTables(t, a)
			signature := seedCrash(t, a, "crash-record-001", "ios", "1", "合成崩溃", "", "", time.Minute)
			locked, err := a.LockCrashGroupIssue(t.Context(), signature)
			if err != nil {
				t.Fatal(err)
			}
			defer locked.Release()
			if err = locked.Start(t.Context(), "synthetic/ios"); err != nil {
				t.Fatal(err)
			}
			if w := crashStatusAction(t, a, signature, status); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if _, err = locked.Record(t.Context(), "synthetic/ios", 1, "https://example.test/issues/1"); err != nil {
				t.Fatal(err)
			}
			var got, url, detail string
			if err = db.pool.QueryRow(t.Context(), `SELECT status,issue_url,(SELECT detail::text FROM admin_audit WHERE action='crash_group_issue' AND target=$1 ORDER BY id DESC LIMIT 1) FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&got, &url, &detail); err != nil || got != status || url != "https://example.test/issues/1" || !strings.Contains(detail, `"to": "`+status+`"`) {
				t.Fatalf("Issue 登记覆盖了后续分诊：状态=%s，链接=%s，审计=%s，错误=%v", got, url, detail, err)
			}
		})
	}
}

func TestCrashGroupStatusUndoValidation(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	signature := seedCrash(t, a, "crash-invalid-01", "ios", "1", "合成崩溃", "", "", time.Minute)
	var before int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit WHERE target=$1`, signature).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{
		map[string]any{"status": "open"},
		map[string]any{"status": "open", "token": ""},
		map[string]any{"status": "open", "token": strings.Repeat("g", 32)},
		map[string]any{"status": "closed", "token": strings.Repeat("a", 32)},
		map[string]any{"status": "open", "token": strings.Repeat("a", 32), "other": true},
		map[string]any{"status": "open", "token": 123}, nil, true, []string{"open"},
	} {
		if w := crashStatusAction(t, a, signature, value); w.Code != 400 {
			t.Fatalf("无效撤销参数=%v，响应=%d %s", value, w.Code, w.Body.String())
		}
	}
	if w := crashStatusAction(t, a, "0123456789abcdef", map[string]any{"status": "open", "token": strings.Repeat("a", 32)}); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit WHERE target=$1`, signature).Scan(&count); err != nil || count != before {
		t.Fatal("无效参数留下成功审计", count, err)
	}
}

func TestCrashGroupStatusUndoRechecksAfterLock(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	signature := seedCrash(t, a, "crash-lock-00001", "ios", "1", "合成崩溃", "", "", time.Minute)
	undo := crashStatusUndo(t, a, signature)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, `UPDATE admin_crash_groups SET status='fixed' WHERE signature=$1`, signature); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- crashStatusAction(t, a, signature, undo) }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w := <-done
	if w.Code != 409 {
		t.Fatal("等待后仍使用旧版本撤销", w.Code, w.Body.String())
	}
	var status string
	if err = db.pool.QueryRow(ctx, `SELECT status FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&status); err != nil || status != "fixed" {
		t.Fatal(status, err)
	}
}

func TestCrashStatusVersionMigration(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	signature := seedCrash(t, a, "crash-migrate-01", "ios", "1", "合成崩溃", "", "", time.Minute)
	for _, change := range []string{
		`DROP TRIGGER admin_crash_status_token ON admin_crash_groups`,
		`ALTER TABLE admin_crash_groups DISABLE TRIGGER admin_crash_status_token`,
		`ALTER TABLE admin_crash_groups DROP COLUMN status_token`,
	} {
		if _, err := db.pool.Exec(t.Context(), change); err != nil {
			t.Fatal(err)
		}
		if err := db.Ready(t.Context()); err == nil {
			t.Fatal("状态版本迁移缺失时仍报告就绪", change)
		}
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		var before, after string
		if err := db.pool.QueryRow(t.Context(), `SELECT status_token FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&before); err != nil || len(before) != 32 {
			t.Fatal(before, err)
		}
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := db.pool.QueryRow(t.Context(), `SELECT status_token FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&after); err != nil || after != before {
			t.Fatal("幂等迁移改变了有效版本", before, after, err)
		}
		if err := db.Ready(t.Context()); err != nil {
			t.Fatal(err)
		}
		// 直接写 SQL 模拟滚动升级期间的旧副本，同状态写入也应使旧撤销失效。
		undo := crashStatusUndo(t, a, signature)
		if _, err := db.pool.Exec(t.Context(), `UPDATE admin_crash_groups SET status='known' WHERE signature=$1`, signature); err != nil {
			t.Fatal(err)
		}
		if w := crashStatusAction(t, a, signature, undo); w.Code != 409 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestCrashStatusVersionWithDMLRole(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	signature := seedCrash(t, a, "crash-dml-000001", "ios", "1", "合成崩溃", "", "", time.Minute)
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	role := pgx.Identifier{"crash_dml_" + randomToken()}.Sanitize()
	var schema string
	if err = tx.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `CREATE ROLE `+role+` NOLOGIN; GRANT USAGE ON SCHEMA `+pgx.Identifier{schema}.Sanitize()+` TO `+role+`; GRANT SELECT,INSERT,UPDATE,DELETE ON admin_crash_groups TO `+role+`; SET LOCAL ROLE `+role); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err = tx.QueryRow(t.Context(), `SELECT status_token FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(t.Context(), `UPDATE admin_crash_groups SET status='known' WHERE signature=$1 RETURNING status_token`, signature).Scan(&after); err != nil || before == after {
		t.Fatal("DML 角色未能更新状态版本", before, after, err)
	}
}
