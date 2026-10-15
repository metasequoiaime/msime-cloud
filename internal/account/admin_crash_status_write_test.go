package account

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type crashStatusWriteTrace struct{ queries atomic.Int64 }

func (trace *crashStatusWriteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "admin_crash_groups") {
		trace.queries.Add(1)
	}
	return ctx
}

func (*crashStatusWriteTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCrashGroupStatusUsesOneStatement(t *testing.T) {
	db := testStore(t)
	signature := "c0ffee1234567890"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_crash_groups(signature,platform,version,title,status) VALUES($1,'windows','1','合成崩溃','open') ON CONFLICT(signature) DO UPDATE SET status='open'`, signature); err != nil {
		t.Fatal(err)
	}
	trace := &crashStatusWriteTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	result, err := actionCrashGroupStatus(&Service{store: &Store{pool: pool}}, t.Context(), tx, actionRequest{ID: signature, Value: json.RawMessage(`"known"`), Reason: "合成原因"})
	if err != nil {
		t.Fatal(err)
	}
	if got := trace.queries.Load(); got != 1 {
		t.Fatalf("崩溃状态读取和写入用了 %d 条 SQL，期望 1 条", got)
	}
	if result.Affected != 1 || result.Detail["from"] != "open" || result.Detail["to"] != "known" || result.Detail["reason"] != "合成原因" || result.Extra["previous"] != "open" {
		t.Fatalf("状态修改结果 = %+v", result)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = db.pool.QueryRow(t.Context(), `SELECT status FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&status); err != nil || status != "open" {
		t.Fatalf("回滚后的状态 = %q，错误 = %v", status, err)
	}
}

// 等待行锁期间的状态修改应成为撤销目标和审计中的旧状态。
func TestCrashGroupStatusUsesLockedPrevious(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	signature := "c0ffee1234567891"
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title,status) VALUES($1,'windows','1','合成崩溃','open') ON CONFLICT(signature) DO UPDATE SET status='open'`, signature); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, `UPDATE admin_crash_groups SET status='fixed' WHERE signature=$1`, signature); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result actionResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		defer tx.Rollback(context.Background())
		result, err := actionCrashGroupStatus(&Service{store: db}, ctx, tx, actionRequest{ID: signature, Value: json.RawMessage(`"known"`)})
		if err == nil {
			err = tx.Commit(ctx)
		}
		done <- outcome{result, err}
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.result.Detail["from"] != "fixed" || got.result.Extra["previous"] != "fixed" {
		t.Fatalf("等待后的结果 = %+v，错误 = %v", got.result, got.err)
	}
}
