package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCrashBackfillUsesBoundedDatabaseCalls(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: &Store{pool: pool}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	created := start.Add(time.Hour)
	for _, tc := range []struct {
		name, message string
		seed          bool
		calls, groups int
	}{
		{name: "有待回填的崩溃", message: "合成崩溃", seed: true, calls: 2, groups: 1},
		{name: "没有待回填的崩溃", calls: 1},
		{name: "崩溃无法分组", message: " \n\t", seed: true, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCrashTables(t, &Service{store: db})
			if tc.seed {
				if _, err := db.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,message,created_at) VALUES('synthetic-backfill-event','crash','ios','1',$1,$2)`, tc.message, created); err != nil {
					t.Fatal(err)
				}
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			more, lastTime, lastID, err := a.backfillCrashBatch(ctx, start, "before")
			if err != nil || more {
				t.Fatal(more, err)
			}
			trace.mu.Lock()
			calls := trace.seen
			trace.mu.Unlock()
			if calls != tc.calls {
				t.Errorf("崩溃回填用了 %d 次数据库调用，期望 %d 次", calls, tc.calls)
			}
			wantTime, wantID := start, "before"
			if tc.seed {
				wantTime, wantID = created, "synthetic-backfill-event"
			}
			if !lastTime.Equal(wantTime) || lastID != wantID {
				t.Fatal("回填游标不正确", lastTime, lastID)
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_events WHERE signature IS NOT NULL`); count != tc.groups {
				t.Fatal("事件签名数量不正确", count)
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_crash_groups`); count != tc.groups {
				t.Fatal("崩溃分组数量不正确", count)
			}
		})
	}
}

func TestCrashBackfillRollsBackFailedWrites(t *testing.T) {
	for _, table := range []string{"admin_events", "admin_crash_groups"} {
		t.Run(table, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx := t.Context()
			resetCrashTables(t, a)
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			created := start.Add(time.Hour)
			if _, err := db.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,message,created_at) VALUES('synthetic-backfill-event','crash','ios','1','合成崩溃',$1)`, created); err != nil {
				t.Fatal(err)
			}
			check := `signature IS NULL`
			if table == "admin_crash_groups" {
				check = `version<>'1'`
			}
			if _, err := db.pool.Exec(ctx, `ALTER TABLE `+table+` ADD CONSTRAINT crash_backfill_test_reject CHECK(`+check+`) NOT VALID`); err != nil {
				t.Fatal(err)
			}
			drop := `ALTER TABLE ` + table + ` DROP CONSTRAINT IF EXISTS crash_backfill_test_reject`
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), drop); err != nil {
					t.Error(err)
				}
			})
			more, lastTime, lastID, err := a.backfillCrashBatch(ctx, start, "before")
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "23514" || more || !lastTime.Equal(start) || lastID != "before" {
				t.Fatal("回填失败未保留原游标或约束错误", more, lastTime, lastID, err)
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_events WHERE signature IS NOT NULL`); count != 0 {
				t.Fatal("回填失败未回滚事件签名", count)
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_crash_groups`); count != 0 {
				t.Fatal("回填失败仍写入了分组", count)
			}
			if _, err := db.pool.Exec(ctx, drop); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := a.backfillCrashBatch(ctx, start, "before"); err != nil {
				t.Fatal("回填失败后无法重试", err)
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_events WHERE signature IS NOT NULL`); count != 1 {
				t.Fatal("重试未写入签名", count)
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_crash_groups`); count != 1 {
				t.Fatal("重试未写入分组", count)
			}
		})
	}
}

func TestCrashBackfillKeepsNewerGroupCommittedDuringLockWait(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	created := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	first, latest := created.Add(-time.Hour), created.Add(time.Hour)
	signature := crashSignature("ios", "合成崩溃", "")
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,message,created_at) VALUES('synthetic-backfill-event','crash','ios','old','合成崩溃',$1)`, created); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title,status,issue_url,first_seen,last_seen) VALUES($1,'ios','base','合成崩溃','fixed','https://example.test/issue/1',$2,$2)`, signature, first); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM admin_crash_groups WHERE signature=$1 FOR UPDATE`, signature); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := a.backfillCrashBatch(ctx, time.Time{}, "")
		done <- err
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if _, err := blocker.Exec(ctx, `UPDATE admin_crash_groups SET version='new',last_seen=$2 WHERE signature=$1`, signature, latest); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err := db.pool.QueryRow(ctx, `SELECT version='new' AND first_seen=$2 AND last_seen=$3 AND status='fixed' AND issue_url='https://example.test/issue/1' FROM admin_crash_groups WHERE signature=$1`, signature, first, latest).Scan(&preserved); err != nil || !preserved {
		t.Fatal("回填覆盖了等待期间提交的较新分组信息", preserved, err)
	}
	var signed bool
	if err := db.pool.QueryRow(ctx, `SELECT signature=$1 FROM admin_events WHERE id='synthetic-backfill-event'`, signature).Scan(&signed); err != nil || !signed {
		t.Fatal("回填未写入事件签名", signed, err)
	}
}
