package account

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type autoIncidentTrace struct {
	mu      sync.Mutex
	queries []string
}

func (t *autoIncidentTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.TrimSpace(data.SQL)
	if strings.Contains(sql, "admin_incidents") || strings.Contains(sql, "admin_notifications") {
		t.mu.Lock()
		t.queries = append(t.queries, sql)
		t.mu.Unlock()
	}
	return ctx
}

func (*autoIncidentTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// 自动事件和通知写入应在同一条数据修改语句中完成。
func TestOpenAutoIncidentBatchesNotificationWrite(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_notification_reads,admin_incidents,admin_notifications`); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &autoIncidentTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	a := &Service{store: &Store{pool: pool}}
	opened, err := a.OpenAutoIncident(t.Context(), "chat", "响应变慢", "最近 5 分钟 P95 4.2s")
	if err != nil || !opened {
		t.Fatalf("open = %v, %v", opened, err)
	}

	trace.mu.Lock()
	queries := append([]string(nil), trace.queries...)
	trace.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("自动事件和通知写入使用了 %d 条相关 SQL，期望 1：%v", len(queries), queries)
	}
	if !strings.Contains(queries[0], "WITH incident AS") || !strings.Contains(queries[0], "admin_notifications") {
		t.Fatalf("相关 SQL 未合并事件和通知写入：%s", queries[0])
	}
}
