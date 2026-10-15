package account

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminMemberCleanupBatchTrace struct {
	mu      sync.Mutex
	batches int
}

func (t *adminMemberCleanupBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}
func (*adminMemberCleanupBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}
func (*adminMemberCleanupBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}
func (*adminMemberCleanupBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*adminMemberCleanupBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestAdminMemberCleanupBatchesSessionAndTokenDeletes(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_members,admin_sessions,admin_tokens,admin_audit CASCADE`); err != nil {
		t.Fatal(err)
	}
	trace := &adminMemberCleanupBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	body := `{"action":"add","email":"batch-admin@example.test"}`
	r := jsonRequest("POST", "/api/admins", body, "").WithContext(WithAdminActor(t.Context(), "google:owner:owner@example.test"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.AdminMembersHTTP(w, r, []string{"owner@example.test"})
	if w.Code != 200 {
		t.Fatalf("添加成员失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches := trace.batches
	trace.mu.Unlock()
	if batches != 1 {
		t.Fatalf("成员凭据清理使用了 %d 次批量请求，期望 1", batches)
	}
}

func TestAdminMemberAddQueryCount(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_members,admin_sessions,admin_tokens,admin_audit CASCADE`); err != nil {
		t.Fatal(err)
	}
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	a := &Service{store: &Store{pool: pool}}
	r := jsonRequest("POST", "/api/admins", `{"action":"add","email":"count-admin@example.test"}`, "").WithContext(WithAdminActor(t.Context(), "google:owner:owner@example.test"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.AdminMembersHTTP(w, r, []string{"owner@example.test"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 6 {
		t.Errorf("添加成员使用了 %d 次数据库调用，期望 6 次（含事务、锁、凭据清理与审计）", queries)
	}
}

func TestAdminMemberConcurrentAddsRespectLimit(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_members,admin_sessions,admin_tokens,admin_audit CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email) SELECT 'existing-'||n||'@example.test' FROM generate_series(1,99) n`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	const workers = 8
	start, done := make(chan struct{}), make(chan *httptest.ResponseRecorder, workers)
	for i := range workers {
		go func() {
			<-start
			body := fmt.Sprintf(`{"action":"add","email":"concurrent-%d@example.test"}`, i)
			r := jsonRequest("POST", "/api/admins", body, "").WithContext(WithAdminActor(ctx, "google:owner:owner@example.test"))
			w := httptest.NewRecorder()
			a.AdminMembersHTTP(w, r, []string{"owner@example.test"})
			done <- w
		}()
	}
	close(start)
	added := 0
	for range workers {
		w := <-done
		if w.Code == 200 {
			added++
		} else if w.Code != 409 {
			t.Error("并发添加返回了非容量错误", w.Code)
		}
	}
	var count, audits int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_members),(SELECT count(*) FROM admin_audit WHERE action='admin_add')`).Scan(&count, &audits); err != nil || added != 1 || count != 100 || audits != 1 {
		t.Fatal("并发添加突破人数上限或写入了多余审计", added, count, audits, err)
	}
}
