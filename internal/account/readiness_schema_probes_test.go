package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReadyCombinesConsecutiveSchemaProbes(t *testing.T) {
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
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	if err := (&Store{pool: pool}).Ready(ctx); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	calls := trace.seen
	trace.mu.Unlock()
	if calls != 8 {
		t.Fatalf("启动探测用了 %d 次数据库调用，期望 8 次", calls)
	}
}

func TestReadyRejectsMissingColumnsAcrossCombinedSchemaProbes(t *testing.T) {
	db := testStore(t)
	for _, tc := range []struct{ table, column string }{
		{"feedback", "diagnostics"},
		{"qq_faq_candidates", "claimed_at"},
		{"qq_bad_answers", "state"},
		{"skin_jobs", "heartbeat_at"},
		{"community_skins", "category"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			ctx := t.Context()
			table := pgx.Identifier{tc.table}.Sanitize()
			column := pgx.Identifier{tc.column}.Sanitize()
			if _, err := db.pool.Exec(ctx, `ALTER TABLE `+table+` RENAME COLUMN `+column+` TO readiness_missing_column`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE `+table+` RENAME COLUMN readiness_missing_column TO `+column); err != nil {
					t.Error(err)
				}
			})
			var failure *pgconn.PgError
			if err := db.Ready(ctx); !errors.As(err, &failure) || failure.Code != "42703" {
				t.Fatal("连续探测中的缺失列未被发现", err)
			}
		})
	}
	if err := db.Ready(t.Context()); err != nil {
		t.Fatal("恢复列后仍不能通过启动探测", err)
	}
}

func TestReadyCombinedProbesWaitForMigrationBeforeTableLocks(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	migration, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer migration.Rollback(context.Background())
	if _, err := migration.Exec(ctx, `SELECT pg_advisory_xact_lock(8372419); LOCK TABLE qq_bad_answers IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- db.Ready(ctx) }()
	waitForAdminDatabaseLock(t, db, migration.Conn().PgConn().PID())
	// 迁移可能接着修改前面的表；探测等待迁移时不能已持有这些表的锁，否则会形成反向锁等待。
	var holdingEarlierTable bool
	if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='feedback'::regclass AND granted AND $1=ANY(pg_blocking_pids(pid)))`, int32(migration.Conn().PgConn().PID())).Scan(&holdingEarlierTable); err != nil {
		t.Fatal(err)
	}
	if holdingEarlierTable {
		t.Error("连续探测在等待迁移前已持有前序表锁")
	}
	if err := migration.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("迁移提交后启动探测失败", err)
	}
}
