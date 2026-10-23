package account

import (
	"context"
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
			owner, wantQueries := "client:synthetic", 4
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
			// 固定一个连接，验证创建任务时实际持锁连接的保活设置。
			cfg.MaxConns = 1
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
			reservation, _, err := a.CreateSkinJob(ctx, id, owner, 1, 1, 10, time.Minute)
			if err != nil {
				t.Fatal("清理过期任务后应能复用 ID 和容量", err)
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != wantQueries {
				t.Errorf("创建任务用了 %d 次数据库调用，期望 %d 次（含事务与锁）", queries, wantQueries)
			}
			var keepalives bool
			if err := pool.QueryRow(ctx, `SELECT inet_server_addr() IS NULL OR (current_setting('tcp_keepalives_idle')='30' AND current_setting('tcp_keepalives_interval')='10' AND current_setting('tcp_keepalives_count')='3')`).Scan(&keepalives); err != nil || !keepalives {
				t.Fatal("持锁连接缺少服务端保活设置", keepalives, err)
			}
			var stored time.Time
			if err = db.pool.QueryRow(ctx, `SELECT expires_at FROM skin_jobs WHERE id=$1 AND owner=$2 AND state='running' AND NOT cancelled`, id, owner).Scan(&stored); err != nil || !stored.Equal(reservation.ExpiresAt) || !reservation.ExpiresAt.After(time.Now()) {
				t.Fatal("新任务状态或到期时间不正确", err)
			}
		})
	}
}

func TestCreateSkinJobUsesCapacityCommittedDuringLockWait(t *testing.T) {
	for _, userOwner := range []bool{false, true} {
		name := "配置客户端"
		if userOwner {
			name = "用户账号"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
				t.Fatal(err)
			}
			owner := "client:synthetic"
			if userOwner {
				owner = "user:" + complete(t, db, Identity{"email", "skin-capacity@example.test"}).User.ID
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, skinJobsLock); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := (&Service{store: db}).CreateSkinJob(ctx, skinJobID('a'), owner, 1, 1, 10, time.Minute)
				done <- err
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if _, err := blocker.Exec(ctx, `INSERT INTO skin_jobs(id,owner,expires_at) VALUES($1,'other',clock_timestamp()+interval '1 minute')`, skinJobID('b')); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrSkinJobsBusy) {
				t.Fatal("未按锁等待期间提交的容量拒绝新任务", err)
			}
			var count int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM skin_jobs`).Scan(&count); err != nil || count != 1 {
				t.Fatal("锁等待后创建任务突破共享容量", count, err)
			}
		})
	}
}

func TestCreateSkinJobRejectsInvalidOwnerBeforeGlobalLock(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "已封禁"
		if missing {
			name = "不存在"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			uid := randomToken()
			want := ErrInvalid
			if !missing {
				uid = complete(t, db, Identity{"email", "skin-banned@example.test"}).User.ID
				want = ErrBanned
				if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=clock_timestamp() WHERE id=$1`, uid); err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, skinJobsLock); err != nil {
				t.Fatal(err)
			}
			if _, _, err := (&Service{store: db}).CreateSkinJob(ctx, skinJobID('c'), "user:"+uid, 1, 1, 10, time.Minute); !errors.Is(err, want) {
				t.Fatal("无效账号未在全局锁之前被拒绝", err)
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
			_, _, err := (&Service{store: db}).CreateSkinJob(ctx, id, owner, ownCap, totalCap, 10, time.Minute)
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
			_, _, err := a.CreateSkinJob(ctx, skinJobID(byte('0'+i)), fmt.Sprintf("owner-%d", i%2), 2, 3, 10, time.Minute)
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
