package account

import (
	"errors"
	"testing"
	"time"
)

func TestRateTxUsesWallClockAfterTransactionWait(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	key := "rate-tx-clock-" + randomToken()
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,1,clock_timestamp()+interval '50 milliseconds')`, key); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_sleep(0.2)`); err != nil {
		t.Fatal(err)
	}
	if err = rateTx(ctx, tx, key, 1, time.Minute); err != nil {
		if errors.Is(err, ErrLimited) {
			t.Fatal("限流窗口已过期却仍被拒绝")
		}
		t.Fatal(err)
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("过期窗口未重置：count=%d", count)
	}
}
