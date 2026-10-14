package account

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// diagnosticsUploadTrace 只记录快照写入语句，避免鉴权和限流查询干扰计数。
type diagnosticsUploadTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *diagnosticsUploadTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "diagnostic_snapshots") && (strings.Contains(data.SQL, "DELETE") || strings.Contains(data.SQL, "INSERT")) {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (*diagnosticsUploadTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDiagnosticsUploadUsesOneSnapshotWriteRoundTrip(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"anonymous", "diagnostics-upload-batch"})
	cfg := db.pool.Config()
	trace := &diagnosticsUploadTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	mux := http.NewServeMux()
	service := &Service{store: &Store{pool: pool}}
	Mount(mux, service)
	first := apiRequest(t, mux, "POST", diagnosticsPath, diagnosticBody(`{"perf_trace":[{"t_ms":1,"kind":"commit","duration_ms":1}]}`), user.AccessToken, 201)
	var firstResponse struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil || firstResponse.ID == "" {
		t.Fatal("首个快照响应无效", first.Body.String(), err)
	}
	if err := service.RecordDiagnosticAccess(t.Context(), firstResponse.ID, "read", nil, 0, 0); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.queries = 0
	trace.mu.Unlock()
	apiRequest(t, mux, "POST", diagnosticsPath, diagnosticBody(`{"perf_trace":[{"t_ms":2,"kind":"commit","duration_ms":1}]}`), user.AccessToken, 201)
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("诊断快照替换使用了 %d 条写入语句，期望 1 条", queries)
	}
	var snapshots, accesses int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*), (SELECT count(*) FROM diagnostic_accesses) FROM diagnostic_snapshots WHERE user_id=$1`, user.User.ID).Scan(&snapshots, &accesses); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 || accesses != 0 {
		t.Fatalf("替换快照后的数据不正确：快照=%d，访问记录=%d", snapshots, accesses)
	}
}
