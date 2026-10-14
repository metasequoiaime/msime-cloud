package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRefreshRejectsSessionExpiredWhileWaitingForLock(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first := complete(t, db, Identity{"email", "refresh-clock@example.test"})
	var sessionID string
	if err := db.pool.QueryRow(ctx, `SELECT id FROM auth_sessions WHERE refresh_hash=$1`, hash(first.RefreshToken)).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	// 将会话设置为短暂有效，确保刷新事务在过期前开始。
	if _, err := db.pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, sessionID); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM auth_sessions WHERE id=$1 FOR UPDATE`, sessionID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := db.Refresh(ctx, first.RefreshToken)
		result <- err
	}()
	// 等待刷新查询确实被行锁阻塞，再等待数据库实际时钟越过 expires_at。
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, int32(blocker.Conn().PgConn().PID())).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("刷新没有进入会话行锁等待")
		}
		select {
		case err := <-result:
			t.Fatal("刷新在锁释放前结束", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	// 等待数据库实际时钟越过 expires_at，而不是猜测一个固定延时。
	for {
		var expired bool
		if err := db.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM auth_sessions WHERE id=$1`, sessionID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("会话没有按数据库时钟过期")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrInvalid) {
		t.Fatalf("会话已在锁等待期间过期，刷新却返回 %v", err)
	}
}
