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

type diagnosticsGetTrace struct {
	mu      sync.Mutex
	queries int
	sql     []string
}

func (t *diagnosticsGetTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.sql = append(t.sql, data.SQL)
	t.mu.Unlock()
	return ctx
}

func (*diagnosticsGetTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDiagnosticsGetUsesOneDataQuery(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"anonymous", "diagnostics-get-batch"})
	id := "0123456789abcdef01234567"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO diagnostic_snapshots(id,user_id,token_hash,token_hint,platform,app_version,sections,content,bytes,expires_at) VALUES($1,$2,$3,'ABCD','android','1.0.0','{perf_trace}','{"perf_trace":[]}',17,now()+interval '1 hour')`, id, user.User.ID, hash("diagnostics-get-batch-token")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO diagnostic_accesses(snapshot_id,tool,arguments,result_count,bytes) VALUES($1,'read_perf_trace','{}',0,0)`, id); err != nil {
		t.Fatal(err)
	}

	trace := &diagnosticsGetTrace{}
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
	trace.sql = nil
	trace.mu.Unlock()
	w := userDataRequest(mux, "GET", diagnosticsPath, user.AccessToken, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"snapshot"`) || !strings.Contains(w.Body.String(), `"accesses"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	trace.mu.Lock()
	queries := trace.queries
	sql := append([]string(nil), trace.sql...)
	trace.mu.Unlock()
	if queries != 3 {
		t.Fatalf("诊断快照读取使用了 %d 条数据库语句，期望路由限流、鉴权和数据各 1 条：%v", queries, sql)
	}
}
