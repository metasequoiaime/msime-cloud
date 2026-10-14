package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type communityDownloadBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *communityDownloadBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *communityDownloadBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
}

func (*communityDownloadBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}
func (*communityDownloadBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*communityDownloadBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestCommunitySkinDownloadBatchesContentAndAudit(t *testing.T) {
	db := testStore(t)
	reader := complete(t, db, Identity{"email", "community-download-batch@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES('community-download-batch-skin',$1,'下载测试','{"design":true}')`, reader.User.ID); err != nil {
		t.Fatal(err)
	}
	trace := &communityDownloadBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	mux := http.NewServeMux()
	Mount(mux, a)
	if w := apiRequest(t, mux, "POST", "/v1/community/skins/community-download-batch-skin/download", `{}`, reader.AccessToken, 200); w.Code != 200 {
		t.Fatalf("下载请求失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 2 {
		t.Fatalf("下载使用了 %d 次批量请求、%d 条语句，期望 1、2", batches, queries)
	}
}
