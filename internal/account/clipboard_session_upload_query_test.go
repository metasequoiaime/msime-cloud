package account

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAddClipboardSessionCombinesSessionDeviceAndStateRead(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	tokens := complete(t, db, Identity{"email", "clipboard-session-upload@example.test"})
	principal, err := db.Authenticate(ctx, tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := clipboardTestReady(t, db, principal.UserID, "session-upload")
	trace := &clipboardUploadBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	item, err := (&Store{pool: pool}).AddClipboardSession(ctx, principal.UserID, principal.SessionID, clipboardTestUpload(key, "session-upload-item"))
	if err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries, batchStatements := trace.queries, trace.batchStatements
	trace.mu.Unlock()
	if queries != 7 || batchStatements != 3 {
		t.Fatalf("会话上传用了 %d 次数据库调用、批内 %d 条语句，期望 7、3", queries, batchStatements)
	}
	if item.KeyID != key || item.Device != "" {
		t.Fatalf("会话设备或密钥不正确：%+v", item)
	}
}
