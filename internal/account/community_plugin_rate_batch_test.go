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

type pluginRateTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *pluginRateTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*pluginRateTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunityPluginRateUsesOneWriteQueryOnOwnPlugin(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"anonymous", "plugin-rate-batch"})
	id := "01234567-1234-4234-8234-012345678901"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES($1,$2,'sound','rate-batch','插件','1.0','MIT',$3,$4,$5)`, id, owner.User.ID, []byte("manifest"), []byte("archive"), strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}

	trace := &pluginRateTrace{}
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
	trace.mu.Lock()
	trace.queries = 0
	trace.mu.Unlock()
	apiRequest(t, mux, "PUT", "/v1/community/plugins/"+id+"/rating", `{"stars":5}`, owner.AccessToken, 403)
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 3 {
		t.Fatalf("给自己的插件评分使用了 %d 条数据库语句，期望路由限流、鉴权和写入各 1 条", queries)
	}
}
