package account

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type challengeAttemptTrace struct{ queries atomic.Int64 }

func (trace *challengeAttemptTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	return ctx
}

func (*challengeAttemptTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestChallengeAttemptUsesOneRoundTrip(t *testing.T) {
	db := testStore(t)
	id := randomToken()
	challenge := Challenge{IDHash: hash(id), Provider: "email", Subject: "attempt@example.test", Nonce: "synthetic-nonce", CodeHash: hash("synthetic-code"), CodeVerifier: "synthetic-verifier", RedirectURI: "https://example.test/callback"}
	if err := db.PutChallenge(t.Context(), challenge); err != nil {
		t.Fatal(err)
	}
	trace := &challengeAttemptTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	trace.queries.Store(0)
	got, err := (&Store{pool: pool}).Attempt(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	challenge.Attempts = 1
	if got != challenge {
		t.Fatal("挑战返回字段与原值不一致")
	}
	if queries := trace.queries.Load(); queries != 1 {
		t.Fatalf("挑战尝试使用了 %d 次数据库请求，期望 1 次", queries)
	}
}

func TestChallengeConcurrentAttemptsStayBounded(t *testing.T) {
	db := testStore(t)
	id := randomToken()
	if err := db.PutChallenge(t.Context(), Challenge{IDHash: hash(id), Provider: "email"}); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	errs := make(chan error, 12)
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := db.Attempt(t.Context(), id); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrInvalid) {
				errs <- err
			}
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var attempts int
	if err := db.pool.QueryRow(t.Context(), `SELECT attempts FROM auth_challenges WHERE id_hash=$1`, hash(id)).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if successes.Load() != 5 || attempts != 5 {
		t.Fatalf("成功次数 = %d，存储次数 = %d，期望均为 5", successes.Load(), attempts)
	}
}

// 过期判断必须发生在取得行锁之后，不能递增等待期间已经过期的挑战。
func TestChallengeAttemptRejectsExpiryAfterLock(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	id := randomToken()
	if err := db.PutChallenge(ctx, Challenge{IDHash: hash(id), Provider: "email"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE auth_challenges SET expires_at=clock_timestamp()+interval '1 second' WHERE id_hash=$1`, hash(id)); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, `SELECT id_hash FROM auth_challenges WHERE id_hash=$1 FOR UPDATE`, hash(id)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := db.Attempt(ctx, id)
		done <- err
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var expired bool
		if err := db.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM auth_challenges WHERE id_hash=$1`, hash(id)).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("挑战没有按数据库时钟过期")
		case <-ticker.C:
		}
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrInvalid) {
		t.Fatalf("等待期间过期的挑战返回 %v，期望 ErrInvalid", err)
	}
	var attempts int
	if err = db.pool.QueryRow(ctx, `SELECT attempts FROM auth_challenges WHERE id_hash=$1`, hash(id)).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("过期挑战次数 = %d，错误 = %v", attempts, err)
	}
}
