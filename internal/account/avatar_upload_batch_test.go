package account

import (
	"context"
	"image/color"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type avatarUploadBatchTrace struct {
	mu           sync.Mutex
	batches      int
	batchQueries int
}

func (t *avatarUploadBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *avatarUploadBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.batchQueries++
	t.mu.Unlock()
}

func (*avatarUploadBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (*avatarUploadBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*avatarUploadBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAvatarUploadBatchesWriteAndProfileResponse(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "avatar-upload-batch@example.test"})
	trace := &avatarUploadBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}, avatars: newMemoryAvatars(), config: Config{Avatars: AvatarConfig{PublicBaseURL: "https://media.example.test"}}}
	mux := http.NewServeMux()
	Mount(mux, a)
	if w := avatarUpload(t, mux, "image/png", encodedImage(t, "png", 8, 8, color.NRGBA{R: 80, G: 120, B: 200, A: 255}), owner.AccessToken, 200); w.Code != 200 {
		t.Fatalf("头像上传失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.batchQueries
	trace.mu.Unlock()
	if batches != 1 || queries != 3 {
		t.Fatalf("头像写入和资料响应使用了 %d 次批量请求、批内 %d 条语句，期望 1、3", batches, queries)
	}
}
