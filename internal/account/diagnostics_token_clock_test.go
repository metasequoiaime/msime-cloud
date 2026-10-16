package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticsTokenRejectsSnapshotExpiredDuringLockWait(t *testing.T) {
	for _, lock := range []string{"user", "snapshot"} {
		t.Run(lock, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			a := &Service{store: db}
			mux := http.NewServeMux()
			Mount(mux, a)
			user := complete(t, db, Identity{"anonymous", "diagnostic-expiry-device"})
			w := userDataRequest(mux, "POST", diagnosticsPath, user.AccessToken, diagnosticBody(`{"input_events":[]}`))
			if w.Code != http.StatusCreated {
				t.Fatal(w.Code, w.Body.String())
			}
			var snapshot map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if lock == "user" {
				_, err = blocker.Exec(ctx, `SELECT id FROM auth_users WHERE id=$1 FOR UPDATE`, user.User.ID)
			} else {
				_, err = blocker.Exec(ctx, `SELECT id FROM diagnostic_snapshots WHERE id=$1 FOR UPDATE`, snapshot["id"])
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- userDataRequest(mux, "POST", diagnosticsPath+"/token", user.AccessToken, "")
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			// 确认换令牌请求已经等锁后再让快照过期，固定时间必须晚于该请求的事务开始。
			if _, err := blocker.Exec(ctx, `UPDATE diagnostic_snapshots SET expires_at=clock_timestamp() WHERE id=$1`, snapshot["id"]); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			w = <-done
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"diagnostics_not_found"`) {
				t.Errorf("锁等待期间过期的快照仍换发令牌：%d %s", w.Code, w.Body.String())
			}
			var stored string
			if err := db.pool.QueryRow(ctx, `SELECT token_hash FROM diagnostic_snapshots WHERE id=$1`, snapshot["id"]).Scan(&stored); err != nil || stored != hash(snapshot["token"]) {
				t.Errorf("过期快照的原令牌被修改：err=%v", err)
			}
		})
	}
}
