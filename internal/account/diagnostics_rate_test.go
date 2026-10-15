package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticSnapshotRate(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "diagnostics-rate@example.test"})
	id := newDiagnosticID()
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO diagnostic_snapshots(id,user_id,token_hash,token_hint,platform,app_version,sections,content,bytes,expires_at) VALUES($1,$2,'synthetic-hash','hint','android','1.0.0',ARRAY['perf_trace'],'{}',2,clock_timestamp()+interval '1 hour')`, id, user.User.ID); err != nil {
		t.Fatal(err)
	}
	key := "mcp-snapshot:" + hash(id)
	const limit = 3
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() { results <- a.RateDiagnosticSnapshot(t.Context(), id, limit, time.Minute) })
	}
	wg.Wait()
	close(results)
	allowed := 0
	for err := range results {
		if err == nil {
			allowed++
		} else if !errors.Is(err, ErrLimited) {
			t.Fatal(err)
		}
	}
	if allowed != limit {
		t.Fatalf("并发请求放行 %d 次，期望 %d 次", allowed, limit)
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_rates SET expires_at=clock_timestamp()-interval '1 second' WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if err := a.RateDiagnosticSnapshot(t.Context(), id, limit, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{
		`UPDATE diagnostic_snapshots SET expires_at=clock_timestamp()-interval '1 second'`,
		`UPDATE diagnostic_snapshots SET expires_at=clock_timestamp()+interval '1 hour'; UPDATE auth_users SET banned_at=clock_timestamp()`,
		`DELETE FROM diagnostic_snapshots`,
	} {
		if _, err := db.pool.Exec(t.Context(), mutation); err != nil {
			t.Fatal(err)
		}
		if err := a.RateDiagnosticSnapshot(t.Context(), id, limit, time.Minute); !errors.Is(err, ErrInvalid) {
			t.Fatal("无效快照未拒绝", err)
		}
		var count int
		if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&count); err != nil || count != 1 {
			t.Fatal("无效快照改变了计数", count, err)
		}
	}
	if err := a.RateDiagnosticSnapshot(t.Context(), "invalid", limit, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.RateDiagnosticSnapshot(ctx, id, limit, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
