package account

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type telemetryLookupKey struct{}

type telemetryLookupBarrier struct {
	arrived chan struct{}
	release <-chan struct{}
}

func (*telemetryLookupBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT EXISTS(SELECT 1 FROM admin_events") {
		return context.WithValue(ctx, telemetryLookupKey{}, true)
	}
	return ctx
}

func (b *telemetryLookupBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(telemetryLookupKey{}) != true || data.Err != nil {
		return
	}
	// 两个请求都确认 `id` 不存在后再允许写入，固定复现跨副本并发重试。
	select {
	case b.arrived <- struct{}{}:
	case <-ctx.Done():
		return
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
}

func TestTelemetryConcurrentRetriesGroupOnlyInsertedEvent(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	resetCrashTables(t, &Service{store: db})
	release := make(chan struct{})
	trace := &telemetryLookupBarrier{arrived: make(chan struct{}, 2), release: release}
	cfg := db.pool.Config()
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, body := range []string{
		`{"id":"telemetry-concurrent-001","kind":"crash","platform":"ios","version":"1","message":"合成崩溃甲"}`,
		`{"id":"telemetry-concurrent-001","kind":"crash","platform":"windows","version":"2","message":"合成崩溃乙"}`,
	} {
		go func() {
			w := httptest.NewRecorder()
			a.Telemetry(w, jsonRequest("POST", TelemetryPath, body, "").WithContext(ctx))
			results <- w
		}()
	}
	for range 2 {
		select {
		case <-trace.arrived:
		case <-ctx.Done():
			t.Fatal("并发请求未同时完成重试检查", ctx.Err())
		}
	}
	close(release)
	for range 2 {
		select {
		case w := <-results:
			if w.Code != 202 {
				t.Fatal(w.Code, w.Body.String())
			}
		case <-ctx.Done():
			t.Fatal("并发遥测没有结束", ctx.Err())
		}
	}
	if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_events`); count != 1 {
		t.Fatal("并发重试产生了多条事件", count)
	}
	if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_crash_groups`); count != 1 {
		t.Fatal("未写入的重试事件仍产生了分组", count)
	}
	var matched bool
	if err := db.pool.QueryRow(ctx, `SELECT g.platform=e.platform AND g.version=e.version AND g.title=e.message
FROM admin_events e JOIN admin_crash_groups g USING(signature)`).Scan(&matched); err != nil || !matched {
		t.Fatal("分组不属于成功写入的事件", matched, err)
	}
}
