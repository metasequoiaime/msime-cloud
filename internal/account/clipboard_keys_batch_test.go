package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type clipboardKeysBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *clipboardKeysBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *clipboardKeysBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
}

func (*clipboardKeysBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (t *clipboardKeysBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*clipboardKeysBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestClipboardKeysUsesOneSnapshotQuery(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "clipboard-keys-batch@example.test"}).User.ID
	key, device := clipboardTestReady(t, db, user, "batch-keys")
	if _, err := db.pool.Exec(ctx, `UPDATE user_clipboard_devices SET last_seen='2020-01-01' WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &statementCancellation{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	store := &Store{pool: pool}
	keys, err := store.ClipboardKeys(ctx, user, device)
	if err != nil {
		t.Fatal(err)
	}
	if !keys.Enabled || keys.KeyID == nil || *keys.KeyID != key || keys.Wrap == nil || len(keys.Devices) != 1 {
		t.Fatalf("剪贴板密钥读取结果=%+v", keys)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("剪贴板密钥快照读取使用了 %d 次数据库调用，期望 1 次", queries)
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1 AND device_id=$2 AND last_seen>'2020-01-01'`, user, device); n != 1 {
		t.Fatal("读取密钥未刷新请求设备的访问时间", n)
	}
}
