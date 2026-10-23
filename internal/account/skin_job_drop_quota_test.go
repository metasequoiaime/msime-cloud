package account

import (
	"context"
	"testing"
	"time"
)

func skinJobReservationForTest(t *testing.T, db *Store, id string, ttl time.Duration) SkinJobReservation {
	t.Helper()
	reservation, _, err := (&Service{store: db}).CreateSkinJob(t.Context(), id, "refund-owner", 10, 10, 10, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}

func assertSkinJobRefundState(t *testing.T, db *Store, jobs, used int) {
	t.Helper()
	var gotJobs, gotUsed int
	err := db.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM skin_jobs),count FROM auth_rates WHERE key=$1`, "skin-art-day:"+hash("refund-owner")).Scan(&gotJobs, &gotUsed)
	if err != nil || gotJobs != jobs || gotUsed != used {
		t.Fatalf("撤回状态不正确：任务=%d/%d 额度=%d/%d err=%v", gotJobs, jobs, gotUsed, used, err)
	}
}

func TestDropSkinJobRefundsOnlyOnce(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	reservation := skinJobReservationForTest(t, db, skinJobID('a'), time.Minute)
	skinJobReservationForTest(t, db, skinJobID('b'), time.Minute)
	var expires time.Time
	if err := db.pool.QueryRow(t.Context(), `SELECT expires_at FROM auth_rates WHERE key=$1`, "skin-art-day:"+hash("refund-owner")).Scan(&expires); err != nil || !expires.Equal(reservation.quotaExpiresAt) {
		t.Fatal("撤回凭据没有保留实际额度窗口", expires, reservation.quotaExpiresAt, err)
	}
	// 多个副本同时重试同一撤回，也只能退还该任务的一次额度。
	done := make(chan error, 4)
	for range 4 {
		go func() { done <- (&Service{store: db}).DropSkinJob(t.Context(), reservation) }()
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	assertSkinJobRefundState(t, db, 1, 1)
	if err := (&Service{store: db}).DropSkinJob(t.Context(), reservation); err != nil {
		t.Fatal(err)
	}
	assertSkinJobRefundState(t, db, 1, 1)
}

func TestDropSkinJobPreservesNewQuotaWindowAfterLockWait(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	reservation := skinJobReservationForTest(t, db, skinJobID('a'), time.Minute)
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	// 撤回读取旧窗口后等待行锁；解锁时数据库必须重新检查窗口身份。
	if _, err := blocker.Exec(ctx, `UPDATE auth_rates SET expires_at=expires_at+interval '1 day',count=3 WHERE key=$1`, "skin-art-day:"+hash("refund-owner")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- (&Service{store: db}).DropSkinJob(ctx, reservation) }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertSkinJobRefundState(t, db, 0, 3)
}

func TestDropSkinJobPreservesReusedJobID(t *testing.T) {
	for _, changedOwner := range []bool{false, true} {
		name := "同一主体"
		if changedOwner {
			name = "另一主体"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
				t.Fatal(err)
			}
			reservation := skinJobReservationForTest(t, db, skinJobID('a'), time.Minute)
			if _, err := db.pool.Exec(ctx, `DELETE FROM skin_jobs WHERE id=$1`, skinJobID('a')); err != nil {
				t.Fatal(err)
			}
			replacement := skinJobReservationForTest(t, db, skinJobID('a'), 2*time.Minute)
			if changedOwner {
				if _, err := db.pool.Exec(ctx, `UPDATE skin_jobs SET owner='other',expires_at=$2 WHERE id=$1`, skinJobID('a'), reservation.ExpiresAt); err != nil {
					t.Fatal(err)
				}
			}
			if err := (&Service{store: db}).DropSkinJob(ctx, reservation); err != nil {
				t.Fatal(err)
			}
			assertSkinJobRefundState(t, db, 1, 2)
			if !changedOwner {
				if err := (&Service{store: db}).DropSkinJob(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				assertSkinJobRefundState(t, db, 0, 1)
			}
		})
	}
}

func TestDropSkinJobQuotaFailureRollsBackDeletion(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	reservation := skinJobReservationForTest(t, db, skinJobID('a'), time.Minute)
	skinJobReservationForTest(t, db, skinJobID('b'), time.Minute)
	if _, err := db.pool.Exec(ctx, `ALTER TABLE auth_rates ADD CONSTRAINT skin_refund_test_reject CHECK (key NOT LIKE 'skin-art-day:%' OR count>=2) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer db.pool.Exec(ctx, `ALTER TABLE auth_rates DROP CONSTRAINT IF EXISTS skin_refund_test_reject`)
	if err := (&Service{store: db}).DropSkinJob(ctx, reservation); err == nil {
		t.Fatal("退款写入失败未返回错误")
	}
	assertSkinJobRefundState(t, db, 2, 2)
	if _, err := db.pool.Exec(ctx, `ALTER TABLE auth_rates DROP CONSTRAINT skin_refund_test_reject`); err != nil {
		t.Fatal(err)
	}
	if err := (&Service{store: db}).DropSkinJob(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	assertSkinJobRefundState(t, db, 1, 1)
}
