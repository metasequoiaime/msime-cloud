package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSharedSkinJobShutdownDuringCreationPreservesDailyQuota(t *testing.T) {
	var upstreamCalls atomic.Int64
	a, b, admin, schema := sharedSkinReplicas(t, 4, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	blocker, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('skin_jobs',0))"); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- call(b, "POST", "/v1/skins/jobs", `{"prompt":"shutdown during creation"}`)
	}()
	// 确认请求已通过首次关闭检查并进入创建事务，再关闭副本，避免按时间猜测交错位置。
	waitFor(t, "创建事务没有等待测试持有的任务锁", func() bool {
		var waiting bool
		if err := blocker.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND $1::integer=ANY(pg_blocking_pids(pid)))`, admin.PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting
	})
	b.Close()
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-done:
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("关闭后创建返回 %d：%s", response.Code, response.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("关闭后的创建请求没有结束")
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("未启动的任务访问上游 %d 次", calls)
	}
	if rows := skinJobRows(t, admin, schema); rows != 0 {
		t.Fatalf("关闭后留下 %d 个未启动任务", rows)
	}
	var used int
	rates := pgx.Identifier{schema, "auth_rates"}.Sanitize()
	if err := admin.QueryRow(ctx, "SELECT COALESCE(sum(count),0) FROM "+rates+" WHERE key LIKE 'skin-art-day:%'").Scan(&used); err != nil || used != 0 {
		t.Fatalf("未启动任务消耗了每日额度：used=%d want=0 err=%v", used, err)
	}
	if response := call(a, "POST", "/v1/skins/jobs", `{"prompt":"retry on another replica"}`); response.Code != http.StatusAccepted {
		t.Fatalf("另一副本不能接受重试：%d %s", response.Code, response.Body.String())
	}
}
