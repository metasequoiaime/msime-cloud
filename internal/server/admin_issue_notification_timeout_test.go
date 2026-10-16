package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestAdminIssueNotificationLockWaitIsBoundedAndRetryable(t *testing.T) {
	s, _, conn, schema := issuesFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	baseline := time.Now()
	s.rememberIssues(ctx, nil, true, true, baseline)
	rows := []issueRow{
		{Repo: issuesWinRepo, Number: 13, Title: "合成问题一", State: "new", CreatedAt: baseline.Add(time.Second)},
		{Repo: issuesWinRepo, Number: 14, Title: "合成问题二", State: "new", CreatedAt: baseline.Add(time.Second)},
	}
	blocker, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	for _, row := range rows {
		key := issueRef{Repo: row.Repo, N: row.Number}.key()
		if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin_notifications:'||$1||':'||$2,0))`, account.NotifyIssue, key); err != nil {
			t.Fatal(err)
		}
	}
	detached, cancelParent := context.WithCancel(ctx)
	cancelParent()
	done := make(chan struct{})
	go func() {
		s.rememberIssues(detached, rows, true, true, baseline.Add(2*time.Second))
		close(done)
	}()
	defer func() {
		_ = blocker.Rollback(context.Background())
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("解除通知锁后调用仍未退出")
		}
	}()
	// 先确认真实查询已阻塞在本事务持有的通知锁上，再检查整批通知的等待上限。
	for {
		var waiting bool
		if err := blocker.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)))`, conn.PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-done:
			t.Fatal("父请求取消后未尝试写入通知")
		case <-ctx.Done():
			t.Fatal("未观察到通知查询等待锁")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// 两条通知共用有限预算；五秒保护期限用于捕获无期限等待或逐条重新计时。
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("通知锁一直未释放时，整批通知未在五秒内结束")
	}
	if got := notificationRows(t, conn, schema); len(got) != 0 {
		t.Fatalf("超时通知不应写入：%v", got)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// 超时后取消预占；后续即使父请求已取消，仍应补写并保持去重。
	for range 2 {
		s.rememberIssues(detached, rows, true, true, baseline.Add(3*time.Second))
	}
	want := []string{"issue issues " + issuesWinRepo + "#13", "issue issues " + issuesWinRepo + "#14"}
	if got := notificationRows(t, conn, schema); !slices.Equal(got, want) {
		t.Fatalf("超时后通知无法重试或重复写入：%v", got)
	}
}
