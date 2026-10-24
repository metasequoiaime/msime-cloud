package account

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNoticeBroadcastFailureAfterCancellationPreservesConnection(t *testing.T) {
	db := testStore(t)
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantPID := conn.Conn().PgConn().PID()
	conn.Release()

	a := &Service{store: &Store{pool: pool}}
	ctx, cancel := context.WithCancel(adminTestContext(t.Context(), "notice-owner"))
	defer cancel()
	a.ConfigureNoticeBroadcaster(&cancelNoticeBroadcaster{cancel: cancel, err: errors.New("synthetic broadcast failure")})
	status, out := noticeAction(t, a, ctx, `{"action":"publish_notice","value":{"title":"合成公告","body":"合成正文","targets":["all"],"channels":["telegram"]}}`)
	if status != http.StatusBadGateway || errorCode(out) != "telegram_failed" || ctx.Err() == nil {
		t.Fatalf("广播失败或取消语义变化：状态=%d，响应=%v，错误=%v", status, out, ctx.Err())
	}

	var gotPID uint32
	if err := pool.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&gotPID); err != nil {
		t.Fatal(err)
	}
	if gotPID != wantPID {
		t.Fatalf("取消后的回滚丢弃了健康连接：原 PID=%d，新 PID=%d", wantPID, gotPID)
	}
}
