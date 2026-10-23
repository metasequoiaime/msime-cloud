package account

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPruneDoesNotDeadlockWithDeleteUserData(t *testing.T) {
	testPruneDeleteLockOrder(t, []string{
		`INSERT INTO user_clipboard_settings(user_id,enabled,retention_days) VALUES($1,true,1)`,
		`INSERT INTO user_clipboard(id,user_id,text,text_hash,sequence,ciphertext,key_id,updated_at) VALUES('synthetic-prune-clipboard',$1,'','synthetic-hash',1,'synthetic-ciphertext','synthetic-key',now()-interval '2 days')`,
		`INSERT INTO voice_contributions(id,user_id,language,provider,duration_ms,transcript,app_version,audio_mime,audio,created_at) VALUES('synthetic-prune-voice',$1,'zh','test',1,'synthetic transcript','test','audio/wav','synthetic audio',now()-interval '181 days')`,
	}, []string{"user_clipboard", "voice_contributions"}, "OLD.id='synthetic-prune-clipboard'", func(ctx context.Context, db *Store, token Tokens) error {
		return db.DeleteUserData(ctx, token.User.ID, map[string]bool{"clipboard": true, "voice": true})
	})
}

func TestPruneDoesNotDeadlockWithTypingStatisticsDelete(t *testing.T) {
	testPruneDeleteLockOrder(t, []string{
		`INSERT INTO typing_statistics_devices(user_id,device,updated_at) VALUES($1,'synthetic-prune-device',now()-interval '63 days')`,
		`INSERT INTO typing_statistics_days(user_id,device,day,characters,active_seconds) VALUES($1,'synthetic-prune-device',current_date-64,1,1)`,
	}, []string{"typing_statistics_devices", "typing_statistics_days"}, "OLD.device='synthetic-prune-device'", func(ctx context.Context, db *Store, token Tokens) error {
		mux := http.NewServeMux()
		Mount(mux, &Service{store: db})
		request := httptest.NewRequest(http.MethodDelete, "/v1/users/me/typing-statistics", nil).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			return fmt.Errorf("清空打字统计返回 %d", response.Code)
		}
		return nil
	})
}

func testPruneDeleteLockOrder(t *testing.T, setup, tables []string, predicate string, deleteData func(context.Context, *Store, Tokens) error) {
	t.Helper()
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	token := complete(t, db, Identity{"email", "prune-lock-order@example.test"})
	for _, query := range setup {
		if _, err := db.pool.Exec(ctx, query, token.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES('synthetic-prune-rate',1,now()-interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	// 用户清空已锁住首张表的目标行时暂停，让定时清理有机会按自己的顺序取得锁。
	const gateKey int64 = 0x6d73697072756e65
	if _, err := db.pool.Exec(ctx, `CREATE FUNCTION synthetic_prune_delete_gate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF `+predicate+` THEN
  PERFORM pg_advisory_xact_lock(7886763304133422693);
 END IF;
 RETURN OLD;
END $$;
CREATE TRIGGER synthetic_prune_delete_gate BEFORE DELETE ON `+tables[0]+` FOR EACH ROW EXECUTE FUNCTION synthetic_prune_delete_gate()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := db.pool.Exec(cleanup, `DROP TRIGGER synthetic_prune_delete_gate ON `+tables[0]+`; DROP FUNCTION synthetic_prune_delete_gate()`); err != nil {
			t.Error(err)
		}
	})
	gate, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, gateKey); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		defer close(deleted)
		deleted <- deleteData(ctx, db, token)
	}()
	t.Cleanup(func() { cancel(); <-deleted })
	waitForAdminDatabaseLock(t, db, gate.Conn().PgConn().PID())
	var deletePID uint32
	if err := db.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, int32(gate.Conn().PgConn().PID())).Scan(&deletePID); err != nil {
		t.Fatal(err)
	}
	pruned := make(chan struct{})
	go func() {
		defer close(pruned)
		db.Prune(ctx)
	}()
	t.Cleanup(func() { cancel(); <-pruned })
	waitForAdminDatabaseLock(t, db, deletePID)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	deleteErr := <-deleted
	<-pruned
	if deleteErr != nil {
		t.Fatalf("清理与用户清空交叉持锁：%v", deleteErr)
	}
	// Prune 不返回错误；它若成为死锁牺牲者，较早删除的过期限流行也会回滚。
	var remaining int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM auth_rates WHERE key='synthetic-prune-rate'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("定时清理未提交：剩余行=%d，错误=%v", remaining, err)
	}
	for _, table := range tables {
		if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE user_id=$1`, token.User.ID).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatalf("用户数据未清空：表=%s，剩余行=%d，错误=%v", table, remaining, err)
		}
	}
}
