package account

import (
	"context"
	"testing"
	"time"
)

func TestDiagnosticRateWindowStartsAfterLockWait(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "diagnostics-window@example.test"})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	id := newDiagnosticID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO diagnostic_snapshots(id,user_id,token_hash,token_hint,platform,app_version,sections,content,bytes,expires_at) VALUES($1,$2,'synthetic-hash','hint','android','1.0.0',ARRAY['perf_trace'],'{}',2,clock_timestamp()+interval '1 hour')`, id, user.User.ID); err != nil {
		t.Fatal(err)
	}
	key := "mcp-snapshot:" + hash(id)
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,1,clock_timestamp()-interval '1 hour')`, key); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT key FROM auth_rates WHERE key=$1 FOR UPDATE`, key); err != nil {
		t.Fatal(err)
	}
	const window = time.Minute
	done := make(chan error, 1)
	go func() { done <- a.RateDiagnosticSnapshot(ctx, id, 1, window) }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	// 以解除锁前的数据库时钟为下界，检查新窗口没有被等待锁的时间缩短。
	var unlockedAt time.Time
	if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&unlockedAt); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("诊断限流新窗口记次失败", err)
	}
	var count int
	var expires time.Time
	if err := db.pool.QueryRow(ctx, `SELECT count,expires_at FROM auth_rates WHERE key=$1`, key).Scan(&count, &expires); err != nil {
		t.Fatal(err)
	}
	if count != 1 || expires.Before(unlockedAt.Add(window)) {
		t.Fatalf("诊断限流窗口没有从锁后开始：count=%d expires=%s earliest=%s", count, expires, unlockedAt.Add(window))
	}
}
