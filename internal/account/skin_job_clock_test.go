package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFinishSkinJobRejectsExpiryWhileWaitingForLock(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	id := skinJobID('e')
	if _, _, err := a.CreateSkinJob(ctx, id, "owner", 1, 1, 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE skin_jobs SET expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM skin_jobs WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	result := make(chan struct {
		recorded bool
		err      error
	}, 1)
	go func() {
		recorded, err := a.FinishSkinJob(ctx, id, "succeeded", "", []byte(`{"width":1}`))
		result <- struct {
			recorded bool
			err      error
		}{recorded, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, int32(blocker.Conn().PgConn().PID())).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("完成请求没有进入任务行锁等待")
		}
		select {
		case r := <-result:
			t.Fatal("完成请求在锁释放前结束", r.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	for {
		var expired bool
		if err := db.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM skin_jobs WHERE id=$1`, id).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("任务没有按数据库时钟过期")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-result
	if r.err != nil || r.recorded {
		t.Fatalf("已在等待期间过期的任务被接受：recorded=%v err=%v", r.recorded, r.err)
	}
	var state string
	if err := db.pool.QueryRow(ctx, `SELECT state FROM skin_jobs WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Errorf("迟到结果改变了任务状态：%q", state)
	}
	if _, err := a.GetSkinJob(ctx, id, "owner"); !errors.Is(err, ErrSkinJobNotFound) {
		t.Errorf("过期任务仍可读取：%v", err)
	}
}
