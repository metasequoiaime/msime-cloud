package account

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type communityReportTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *communityReportTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "community_reports") || strings.Contains(data.SQL, "community_skins") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (*communityReportTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunityReportVisibilityAndInsertUseOneDatabaseStatement(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "community-report-batch@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES('community-report-batch-skin',$1,'举报测试','{}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &communityReportTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/community/reports", Route(a, "POST /v1/community/reports", (*Service).CommunityReport))
	apiRequest(t, mux, "POST", "/v1/community/reports", `{"kind":"skins","item_id":"community-report-batch-skin","reason":"重复内容"}`, owner.AccessToken, 201)
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("举报可见性检查和写入使用了 %d 条相关数据库语句，期望 1 条", queries)
	}
}
