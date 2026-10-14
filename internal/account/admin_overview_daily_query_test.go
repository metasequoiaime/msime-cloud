package account

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type overviewQueryTrace struct {
	queries chan string
}

func (trace *overviewQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "'daily'") {
		trace.queries <- data.SQL
	}
	return ctx
}

func (*overviewQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminOverviewDailyUsesGroupedScans(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events,admin_crash_groups,admin_service_daily,admin_incidents CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id,created_at) VALUES('overview-daily-user',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_events(id,kind,platform,version,created_at) VALUES
 ('overview-daily-download','download','windows','1',now()),('overview-daily-crash','crash','windows','1',now())`); err != nil {
		t.Fatal(err)
	}
	trace := &overviewQueryTrace{queries: make(chan string, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	if body := getOverview(t, a, "?days=7"); len(body.Daily) != 7 {
		t.Fatalf("每日序列长度错误：%d", len(body.Daily))
	}
	var query string
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到总览查询")
	}
	if !strings.Contains(query, "user_daily") || !strings.Contains(query, "event_daily") {
		t.Fatalf("每日序列没有使用分组聚合：%s", query)
	}
}
