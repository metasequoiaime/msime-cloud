package account

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCreateSkinJobUsesBoundedRoundTrips(t *testing.T) {
	for _, ownerKind := range []string{"client", "user"} {
		t.Run(ownerKind, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
				t.Fatal(err)
			}
			owner, wantQueries := "client:synthetic", 5
			if ownerKind == "user" {
				owner = "user:" + complete(t, db, Identity{"email", "skin-create@example.test"}).User.ID
				wantQueries++
			}
			id := skinJobID('a')
			if _, err := db.pool.Exec(ctx, `INSERT INTO skin_jobs(id,owner,expires_at) VALUES($1,$2,now()-interval '1 minute')`, id, owner); err != nil {
				t.Fatal(err)
			}
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if err = pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			a := &Service{store: &Store{pool: pool}}
			expires, err := a.CreateSkinJob(ctx, id, owner, 1, 1, time.Minute)
			if err != nil {
				t.Fatal("清理过期任务后应能复用 ID 和容量", err)
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != wantQueries {
				t.Errorf("创建任务用了 %d 次数据库调用，期望 %d 次（含事务与锁）", queries, wantQueries)
			}
			var stored time.Time
			if err = db.pool.QueryRow(ctx, `SELECT expires_at FROM skin_jobs WHERE id=$1 AND owner=$2 AND state='running' AND NOT cancelled`, id, owner).Scan(&stored); err != nil || !stored.Equal(expires) || !expires.After(time.Now()) {
				t.Fatal("新任务状态或到期时间不正确", err)
			}
		})
	}
}

func TestCreateSkinJobRejectionRollsBackCleanup(t *testing.T) {
	for _, mode := range []string{"owner-cap", "global-cap", "duplicate-id"} {
		t.Run(mode, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
				t.Fatal(err)
			}
			expiredID, activeID := skinJobID('a'), skinJobID('b')
			if _, err := db.pool.Exec(ctx, `INSERT INTO skin_jobs(id,owner,expires_at) VALUES($1,'owner',now()-interval '1 minute'),($2,'owner',now()+interval '1 minute')`, expiredID, activeID); err != nil {
				t.Fatal(err)
			}
			id, owner, ownCap, totalCap := skinJobID('c'), "owner", 1, 2
			switch mode {
			case "global-cap":
				owner, ownCap, totalCap = "other", 2, 1
			case "duplicate-id":
				id, ownCap = activeID, 2
			}
			_, err := (&Service{store: db}).CreateSkinJob(ctx, id, owner, ownCap, totalCap, time.Minute)
			if mode == "duplicate-id" {
				if err == nil || errors.Is(err, ErrSkinJobsBusy) {
					t.Fatal("插入约束失败应保留数据库错误", err)
				}
			} else if !errors.Is(err, ErrSkinJobsBusy) {
				t.Fatal("达到容量上限应拒绝创建", err)
			}
			var count int
			if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM skin_jobs WHERE id IN ($1,$2)`, expiredID, activeID).Scan(&count); err != nil || count != 2 {
				t.Fatal("失败创建未回滚清理操作", count, err)
			}
			if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM skin_jobs`).Scan(&count); err != nil || count != 2 {
				t.Fatal("失败创建留下了新任务", count, err)
			}
		})
	}
}

func TestCreateSkinJobConcurrentCaps(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	const workers = 8
	start, done := make(chan struct{}), make(chan error, workers)
	for i := range workers {
		go func() {
			<-start
			_, err := a.CreateSkinJob(ctx, skinJobID(byte('0'+i)), fmt.Sprintf("owner-%d", i%2), 2, 3, time.Minute)
			done <- err
		}()
	}
	close(start)
	created := 0
	for range workers {
		switch err := <-done; {
		case err == nil:
			created++
		case errors.Is(err, ErrSkinJobsBusy):
		default:
			t.Error(err)
		}
	}
	var count, maxOwn int
	if err := db.pool.QueryRow(ctx, `SELECT sum(n),max(n) FROM (SELECT count(*) AS n FROM skin_jobs GROUP BY owner) counts`).Scan(&count, &maxOwn); err != nil || created != 3 || count != 3 || maxOwn > 2 {
		t.Fatal("并发创建突破共享容量或错误拒绝", created, count, maxOwn, err)
	}
}
