package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClipboardRetentionRejectsAccountDeletedWhileWaiting(t *testing.T) {
	assertClipboardWriteRejectsDeleted(t, http.MethodPut, "/v1/users/me/clipboard/retention", `{"days":7}`)
}

func TestClipboardSettingsRejectsAccountDeletedWhileWaiting(t *testing.T) {
	for _, body := range []string{`{"enabled":true}`, `{"enabled":false}`} {
		t.Run(body, func(t *testing.T) {
			assertClipboardWriteRejectsDeleted(t, http.MethodPut, "/v1/users/me/clipboard/settings", body)
		})
	}
}

func assertClipboardWriteRejectsDeleted(t *testing.T, method, path, body string) {
	t.Helper()
	db := testStore(t)
	user := complete(t, db, Identity{"email", "delete-profile@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM auth_users WHERE id=$1 FOR UPDATE`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+user.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		done <- w
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if _, err := blocker.Exec(ctx, `DELETE FROM auth_users WHERE id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w := <-done
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"code":"invalid_credentials"`) {
		t.Fatalf("账号注销后迟到剪贴板设置应返回401 invalid_credentials，实际%d %s", w.Code, w.Body.String())
	}
}
