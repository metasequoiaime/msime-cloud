package account

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type clipboardDevicePutTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *clipboardDevicePutTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*clipboardDevicePutTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPutClipboardDeviceEvictionUsesOneWriteRoundTrip(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "clipboard-device-batch@example.test"}).User.ID
	for i := 0; i < clipboardDeviceLimit; i++ {
		if _, err := db.pool.Exec(ctx, `INSERT INTO user_clipboard_devices(user_id,device_id,public_key,name,created_at) VALUES($1,$2,$3,$4,$5)`, user, fmt.Sprintf("%032x", i), "key", fmt.Sprintf("设备 %d", i), time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	cfg := db.pool.Config()
	trace := &clipboardDevicePutTrace{}
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
	trace.queries = 0
	trace.mu.Unlock()
	store := &Store{pool: pool}
	newDevice := fmt.Sprintf("%032x", clipboardDeviceLimit)
	if err = store.PutClipboardDevice(ctx, user, newDevice, "new-key", "新设备"); err != nil {
		t.Fatal(err)
	}

	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 5 {
		t.Fatalf("满额登记设备应使用事务、用户锁、更新和合并写入共 5 条语句，实际为 %d", queries)
	}
	var count int
	if err = db.pool.QueryRow(ctx, "SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1", user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != clipboardDeviceLimit {
		t.Fatalf("淘汰后设备数=%d，期望 %d", count, clipboardDeviceLimit)
	}
	var exists bool
	if err = db.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM user_clipboard_devices WHERE user_id=$1 AND device_id=$2)", user, newDevice).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("新设备未登记")
	}
}
