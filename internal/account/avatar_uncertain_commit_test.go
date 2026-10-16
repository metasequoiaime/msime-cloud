package account

import (
	"bytes"
	"image/color"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// avatarCommitResponseLoss 丢弃数据库完成 COMMIT 后的首个响应，模拟写入成功但客户端无法确认结果。
type avatarCommitResponseLoss struct {
	reader  io.Reader
	writer  io.Writer
	pending atomic.Bool
	lost    *atomic.Bool
}

func (c *avatarCommitResponseLoss) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("commit\x00")) {
		c.pending.Store(true)
	}
	return c.writer.Write(p)
}

func (c *avatarCommitResponseLoss) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if n > 0 && c.pending.Swap(false) && c.lost.CompareAndSwap(false, true) {
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestAvatarUploadPreservesObjectWhenCommitResponseIsLost(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "avatar-uncertain@example.test"})
	storage := newMemoryAvatars()
	cfg := db.pool.Config()
	frontend := cfg.ConnConfig.BuildFrontend
	var lost atomic.Bool
	// 在 PostgreSQL 协议层丢弃响应，同时支持 TCP、Unix 套接字和 TLS 测试连接。
	cfg.ConnConfig.BuildFrontend = func(reader io.Reader, writer io.Writer) *pgproto3.Frontend {
		connection := &avatarCommitResponseLoss{reader: reader, writer: writer, lost: &lost}
		return frontend(connection, connection)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}, avatars: storage, config: Config{Avatars: AvatarConfig{PublicBaseURL: "https://media.example.test"}}}
	mux := http.NewServeMux()
	Mount(mux, a)
	avatarUpload(t, mux, "image/png", encodedImage(t, "png", 8, 8, color.White), user.AccessToken, http.StatusServiceUnavailable)
	if !lost.Load() {
		t.Fatal("没有模拟丢失提交响应")
	}
	key, err := db.AvatarKey(ctx, user.User.ID)
	if err != nil || key == "" {
		t.Fatalf("没有保留已提交的头像登记：%q，%v", key, err)
	}
	storage.mu.Lock()
	_, exists := storage.objects[key]
	storage.mu.Unlock()
	if !exists {
		t.Fatalf("提交响应丢失后删除了数据库仍在引用的头像对象 %q", key)
	}
}

// 提交前读取资料失败会回滚，仍应清理这次新上传的无引用对象。
func TestAvatarUploadCleansObjectWhenProfileReadRollsBack(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "avatar-rollback@example.test"})
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET avatar_key='avatars/old.jpg',created_at='infinity' WHERE id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	storage := newMemoryAvatars()
	if err := storage.Put(ctx, "avatars/old.jpg", []byte("old image"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db, avatars: storage}
	mux := http.NewServeMux()
	Mount(mux, a)
	avatarUpload(t, mux, "image/png", encodedImage(t, "png", 8, 8, color.White), user.AccessToken, http.StatusServiceUnavailable)
	if keys := storage.keys(); len(keys) != 1 || keys[0] != "avatars/old.jpg" {
		t.Fatalf("回滚后未只保留原头像：%v", keys)
	}
	if key, err := db.AvatarKey(ctx, user.User.ID); err != nil || key != "avatars/old.jpg" {
		t.Fatalf("回滚后头像登记变化：%q，%v", key, err)
	}
}
