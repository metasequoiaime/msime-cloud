package account

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type clipboardUploadBatchTrace struct {
	clipboardDisableBatchTrace
	batchStatements int
}

func (t *clipboardUploadBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.batchStatements++
	t.mu.Unlock()
}

func TestAddClipboardBatchesOrderedWrites(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "clipboard-upload-batch@example.test"}).User.ID
	key, _ := clipboardTestReady(t, db, user, "upload-batch")
	upload := clipboardTestUpload(key, "new")
	if _, err := db.pool.Exec(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence,pinned)
 SELECT 'existing-'||n,$1,'',md5(n::text),$2,$3,n,n=1 FROM generate_series(1,50) n`, user, upload.Ciphertext, key); err != nil {
		t.Fatal(err)
	}
	// 旧密钥条目复用本次去重标签；先删旧条目，才能分配新的 ID 和连续序号。
	if _, err := db.pool.Exec(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence)
 VALUES('stale-upload',$1,'',$2,$3,$4,1000)`, user, upload.Dedup, upload.Ciphertext, clipboardTestKeyID("stale")); err != nil {
		t.Fatal(err)
	}
	trace := &clipboardUploadBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &Store{pool: pool}
	item, err := s.AddClipboard(ctx, user, "测试设备", upload)
	if err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	batches, queries, batchStatements := trace.batches, trace.queries, trace.batchStatements
	trace.mu.Unlock()
	if batches != 1 || queries != 7 || batchStatements != 3 {
		t.Fatalf("上传剪贴板使用了 %d 次批量请求、共 %d 条语句、批内 %d 条语句，期望 1、7、3", batches, queries, batchStatements)
	}
	if item.ID == "stale-upload" || item.Ciphertext != upload.Ciphertext || item.KeyID != key || item.Device != "测试设备" || item.Pinned {
		t.Fatalf("上传结果不正确：%+v", item)
	}
	items, err := s.ListClipboard(ctx, user, key)
	if err != nil || len(items) != 50 || items[0].ID != "existing-1" || !items[0].Pinned || items[1].ID != item.ID {
		t.Fatalf("容量清理或置顶顺序不正确：%+v %v", items, err)
	}
	var sequence int64
	if err := db.pool.QueryRow(ctx, `SELECT sequence FROM user_clipboard WHERE id=$1`, item.ID).Scan(&sequence); err != nil || sequence != 51 {
		t.Fatalf("旧密钥清理应先于写入：sequence=%d err=%v", sequence, err)
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_clipboard WHERE user_id=$1 AND id IN ('existing-2','stale-upload')`, user); n != 0 {
		t.Fatalf("旧密钥或最早的未置顶条目未被清理：%d", n)
	}
}
