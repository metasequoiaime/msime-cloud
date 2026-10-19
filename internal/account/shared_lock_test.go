package account

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Two services on separate pools stand in for two replicas: the second waits while the first holds the lock, gets ErrLockBusy once its wait runs out, takes the lock as soon as the first releases it, and a waiter whose request is cancelled stops at once.
func TestWaitLockAcrossPools(t *testing.T) {
	first := &Service{store: testStore(t)}
	other, err := Open(context.Background(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	second := &Service{store: other}
	ctx := context.Background()
	name := "test:" + t.Name() + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)

	release, err := first.WaitLock(ctx, name, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release = sync.OnceFunc(release)
	defer release()
	started := time.Now()
	if _, err = second.WaitLock(ctx, name, 300*time.Millisecond); !errors.Is(err, ErrLockBusy) {
		t.Fatal("second replica took a held lock", err)
	}
	if waited := time.Since(started); waited < 300*time.Millisecond {
		t.Fatal("gave up before the wait ran out", waited)
	}
	// 超时可能销毁连接，pgx 会异步移除池统计；等待回收完成，仍要求等待者不占用连接。
	drainCtx, drainCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer drainCancel()
	for other.pool.Stat().AcquiredConns() != 0 {
		select {
		case <-drainCtx.Done():
			t.Fatal("等待者未归还连接", other.pool.Stat().AcquiredConns())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = second.WaitLock(cancelled, name, 5*time.Second); err == nil {
		t.Fatal("a cancelled waiter took the lock")
	}

	// Release only after the waiter has failed at least one attempt, so the poll-then-acquire path is what runs: every attempt checks a connection out, and a second checkout means the first attempt found the lock held.
	attempts := other.pool.Stat().AcquireCount()
	got := make(chan error, 1)
	go func() {
		release, err := second.WaitLock(ctx, name, 5*time.Second)
		if err == nil {
			release()
		}
		got <- err
	}()
	for other.pool.Stat().AcquireCount() < attempts+2 {
		select {
		case err = <-got:
			t.Fatal("waiter finished while the lock was held", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	release()
	if err = <-got; err != nil {
		t.Fatal("waiter did not get the released lock", err)
	}
	// Released on both sides: the first takes it again without waiting, and the pool holds no connection afterwards.
	release, err = first.WaitLock(ctx, name, 0)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if acquired := first.store.pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatal("release kept the connection", acquired)
	}
}

// The lock holder's session gets server-side keepalives, so a holder whose node vanishes does not keep the lock for hours. A one-connection pool makes the connection inspected afterwards the one that held the lock.
func TestWaitLockSetsServerKeepalives(t *testing.T) {
	testStore(t)
	cfg, err := pgxpool.ParseConfig(os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	var tcp bool
	if err = pool.QueryRow(ctx, `SELECT inet_server_addr() IS NOT NULL`).Scan(&tcp); err != nil {
		t.Fatal(err)
	}
	if !tcp {
		t.Skip("keepalives only apply to TCP connections")
	}
	release, err := (&Service{store: &Store{pool: pool}}).WaitLock(ctx, "test:"+t.Name()+":"+strconv.FormatInt(time.Now().UnixNano(), 10), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release()
	var idle, interval, count string
	if err = pool.QueryRow(ctx, `SELECT current_setting('tcp_keepalives_idle'),current_setting('tcp_keepalives_interval'),current_setting('tcp_keepalives_count')`).Scan(&idle, &interval, &count); err != nil {
		t.Fatal(err)
	}
	if idle != "30" || interval != "10" || count != "3" {
		t.Fatal("lock holder without server keepalives", idle, interval, count)
	}
}
