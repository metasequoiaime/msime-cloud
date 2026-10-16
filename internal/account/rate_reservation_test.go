package account

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRateReservationCapacityRefundAndWindow(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := t.Context()
	const scope, subject = "synthetic-reservation", "member"
	first, wait, err := a.ReserveRateLimit(ctx, scope, subject, 2, time.Hour)
	if err != nil || wait != 0 || first == nil {
		t.Fatal(wait, err)
	}
	second, _, err := a.ReserveRateLimit(ctx, scope, subject, 2, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		release, wait, err := a.ReserveRateLimit(ctx, scope, subject, 2, time.Hour)
		if !errors.Is(err, ErrLimited) || release != nil || wait < 59*time.Minute || wait > time.Hour {
			t.Fatal("拒绝结果或剩余窗口错误", release == nil, wait, err)
		}
	}
	key := scope + ":" + hash(subject)
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&count); err != nil || count != 2 {
		t.Fatal("拒绝请求改变了已预留计数", count, err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := first(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if err := db.pool.QueryRow(ctx, `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&count); err != nil || count != 1 {
		t.Fatal("同一预留重复退款", count, err)
	}
	third, _, err := a.ReserveRateLimit(ctx, scope, subject, 2, time.Hour)
	if err != nil {
		t.Fatal("退款后不能重试", err)
	}
	if err := first(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.ReserveRateLimit(ctx, scope, subject, 2, time.Hour); !errors.Is(err, ErrLimited) {
		t.Fatal("重复退款影响其他预留", err)
	}
	if db.pool.Stat().AcquiredConns() != 0 {
		t.Fatal("预留持有期间占用了连接")
	}
	if _, err := db.pool.Exec(ctx, `UPDATE auth_rates SET expires_at=clock_timestamp()-interval '1 second' WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	current, _, err := a.ReserveRateLimit(ctx, scope, subject, 2, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := second(ctx); err != nil {
		t.Fatal(err)
	}
	if err := third(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&count); err != nil || count != 1 {
		t.Fatal("旧窗口退款影响新窗口", count, err)
	}
	if err := current(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRateReservationConcurrentPoolsAndLegacyRows(t *testing.T) {
	db := testStore(t)
	other, err := Open(t.Context(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	services := []*Service{{store: db}, {store: other}}
	const scope, subject = "synthetic-reservation", "concurrent"
	const capacity = 3
	claimed := make(chan func(context.Context) error, 20)
	var workers sync.WaitGroup
	for i := range cap(claimed) {
		workers.Go(func() {
			release, _, err := services[i%2].ReserveRateLimit(t.Context(), scope, subject, capacity, time.Hour)
			if err == nil {
				claimed <- release
			} else if !errors.Is(err, ErrLimited) {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if len(claimed) != capacity {
		t.Fatal("跨副本预留超过或不足上限", len(claimed))
	}
	close(claimed)
	for release := range claimed {
		if err := release(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	key := scope + ":" + hash(subject)
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_rates SET count=$2 WHERE key=$1`, key, capacity+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := services[0].ReserveRateLimit(t.Context(), scope, subject, capacity, time.Hour); !errors.Is(err, ErrLimited) {
		t.Fatal("旧计数被错误放开", err)
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_rates SET expires_at=clock_timestamp()-interval '1 second' WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := services[1].ReserveRateLimit(t.Context(), scope, subject, capacity, time.Hour); err != nil {
		t.Fatal("过期的旧计数未重开窗口", err)
	}
}

func TestRateReservationValidationAndCanceledRefund(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	for _, test := range []struct {
		limit  int
		window time.Duration
	}{{0, time.Hour}, {-1, time.Hour}, {1, 0}, {1, -time.Second}} {
		if release, _, err := a.ReserveRateLimit(t.Context(), "synthetic-reservation", "invalid", test.limit, test.window); !errors.Is(err, ErrInvalid) || release != nil {
			t.Fatal("非法额度预留被接受", test, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if release, _, err := a.ReserveRateLimit(ctx, "synthetic-reservation", "canceled", 2, time.Hour); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatal("已取消的预留被接受", err)
	}
	release, _, err := a.ReserveRateLimit(t.Context(), "synthetic-reservation", "refund-canceled", 2, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// 退款只尝试一次，包括结果不确定的失败；重试不能误退同窗口其他预留。
	if err := release(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := release(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal("退款重复执行", err)
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key=$1`, "synthetic-reservation:"+hash("refund-canceled")).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestRateReservationWindowStartsAfterLockWait(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const scope, subject = "synthetic-reservation", "locked"
	key := scope + ":" + hash(subject)
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,3,clock_timestamp()-interval '1 second')`, key); err != nil {
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
	done := make(chan error, 1)
	go func() { _, _, err := a.ReserveRateLimit(ctx, scope, subject, 3, time.Minute); done <- err }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	var unlockedAt time.Time
	if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&unlockedAt); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var until time.Time
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count,expires_at FROM auth_rates WHERE key=$1`, key).Scan(&count, &until); err != nil || count != 1 || until.Before(unlockedAt.Add(time.Minute)) {
		t.Fatal("新窗口未从解除锁后的数据库时间开始", count, until, unlockedAt, err)
	}
}

// reservationReadHook 在拒绝预留与查询等待时间之间插入窗口变化，避免依赖 goroutine 调度。
type reservationReadHook struct {
	once  sync.Once
	after func(context.Context)
}

func (h *reservationReadHook) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT count,expires_at,clock_timestamp() FROM auth_rates") {
		h.once.Do(func() { h.after(ctx) })
	}
	return ctx
}
func (*reservationReadHook) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRateReservationWaitAfterConcurrentRefundOrCleanup(t *testing.T) {
	for _, scenario := range []string{"refund", "cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			const scope, subject = "synthetic-reservation", "wait-race"
			refund, _, err := a.ReserveRateLimit(t.Context(), scope, subject, 2, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := a.ReserveRateLimit(t.Context(), scope, subject, 2, time.Hour); err != nil {
				t.Fatal(err)
			}
			hook := &reservationReadHook{after: func(ctx context.Context) {
				if scenario == "refund" {
					if err := refund(ctx); err != nil {
						t.Fatal(err)
					}
				} else if _, err := db.pool.Exec(ctx, `DELETE FROM auth_rates WHERE key=$1`, scope+":"+hash(subject)); err != nil {
					t.Fatal(err)
				}
			}}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = hook
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			traced := &Service{store: &Store{pool: pool}}
			if release, wait, err := traced.ReserveRateLimit(t.Context(), scope, subject, 2, time.Hour); !errors.Is(err, ErrLimited) || release != nil || wait != time.Second {
				t.Fatal("名额已出现时不应提示等待整个旧窗口", wait, err)
			}
			if _, _, err := a.ReserveRateLimit(t.Context(), scope, subject, 2, time.Hour); err != nil {
				t.Fatal("并发退款或清理后未恢复空位", err)
			}
		})
	}
}
