package account

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type notificationListBatchTrace struct {
	mu               sync.Mutex
	batches, queries int
}

func (t *notificationListBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}
func (t *notificationListBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}
func (t *notificationListBatchTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
}
func (*notificationListBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (*notificationListBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminNotificationsBatchesListAndUnread(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_notifications,admin_notification_reads,admin_preferences CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_notifications(kind,title,target_page,target_id) VALUES('issue','问题','issues','1')`); err != nil {
		t.Fatal(err)
	}
	trace := &notificationListBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	body := listNotifications(t, a, "member@example.test", "")
	if len(body.Items) != 1 || body.Unread != 1 {
		t.Fatalf("通知列表错误：%+v", body)
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 2 {
		t.Fatalf("通知列表使用了 %d 次批量请求、%d 条查询，期望 1、2", batches, queries)
	}
}
