package account

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestCreateSkinJobDailyQuotaIsAtomic(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	const workers, daily = 8, 2
	start, done := make(chan struct{}), make(chan error, workers)
	for i := range workers {
		go func() {
			<-start
			_, wait, err := a.CreateSkinJob(ctx, skinJobID(byte('0'+i)), "quota-owner", workers, workers, daily, time.Minute)
			if errors.Is(err, ErrLimited) && (wait < time.Second || wait > 24*time.Hour) {
				t.Errorf("额度超限未给出有效等待时长：%s", wait)
			}
			done <- err
		}()
	}
	close(start)
	created := 0
	for range workers {
		switch err := <-done; {
		case err == nil:
			created++
		case errors.Is(err, ErrLimited):
		default:
			t.Error(err)
		}
	}
	key := "skin-art-day:" + hash("quota-owner")
	var jobs, used int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM skin_jobs),count FROM auth_rates WHERE key=$1`, key).Scan(&jobs, &used); err != nil || jobs != daily || created != daily || used != daily {
		t.Fatalf("并发创建未按每日额度整体提交或回滚：jobs=%d created=%d used=%d err=%v", jobs, created, used, err)
	}
	// 旧窗口过期后可以重新创建，额度超限回滚的任务不会占住后续容量。
	if _, err := db.pool.Exec(ctx, `UPDATE auth_rates SET expires_at=clock_timestamp()-interval '1 second' WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, wait, err := a.CreateSkinJob(ctx, skinJobID('f'), "quota-owner", workers, workers, daily, time.Minute); err != nil || wait != 0 {
		t.Fatal("过期窗口未恢复额度", wait, err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&used); err != nil || used != 1 {
		t.Fatal("新窗口未从一次开始计数", used, err)
	}
}

func TestCreateSkinJobQuotaWriteFailureRollsBackJob(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE auth_rates ADD CONSTRAINT skin_quota_test_reject CHECK (key NOT LIKE 'skin-art-day:%') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer db.pool.Exec(ctx, `ALTER TABLE auth_rates DROP CONSTRAINT skin_quota_test_reject`)
	_, _, err := (&Service{store: db}).CreateSkinJob(ctx, skinJobID('a'), "quota-owner", 1, 1, 10, time.Minute)
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "23514" {
		t.Fatal("未返回额度写入错误", err)
	}
	var jobs int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM skin_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("额度写入失败留下了任务", jobs, err)
	}
}
