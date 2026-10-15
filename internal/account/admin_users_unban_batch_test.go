package account

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestActionUnbanUserBatchesCommunityUpdates(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "unban-content-batch@example.test"})
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,design) VALUES('batch-unban-skin',$1,'Skin','{}')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES('batch-unban-resource',$1,'dictionary','Resource','{"entries":[]}','pending')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	insertCandidateSkin(t, db, "bd334455-1234-4234-8234-000000000020", user.User.ID, "Candidate")
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES('bd334455-1234-4234-8234-000000000021',$1,'sound','batch-unban.sound','Plugin','1.0','MIT','id = "batch-unban.sound"','zip',$2)`, user.User.ID, hash("batch-unban-plugin")); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = actionBanUser(&Service{store: db}, ctx, tx, actionRequest{ID: user.User.ID, Reason: "spam"}); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
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
	s := &Service{store: &Store{pool: pool}}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	result, err := actionUnbanUser(s, ctx, tx, actionRequest{ID: user.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	var updates int
	for _, statement := range trace.statements {
		if strings.Contains(statement, "SET moderation=COALESCE(previous_moderation") {
			updates++
		}
	}
	trace.mu.Unlock()
	if updates != 1 {
		t.Fatalf("解封四张社区表用了 %d 次数据库更新，期望批量写入 1 次", updates)
	}
	if restored, ok := result.Detail["restored"].(int64); !ok || restored != 4 {
		t.Fatalf("解封恢复数量 %v，期望 4", result.Detail["restored"])
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestActionUnbanUserUsesTwoQueriesWithoutContent(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "unban-empty@example.test"})
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now(),ban_reason='合成原因',banned_by='owner@example.test' WHERE id=$1`, user.User.ID); err != nil {
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	result, err := actionUnbanUser(&Service{store: &Store{pool: pool}}, ctx, tx, actionRequest{ID: user.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 2 {
		t.Errorf("无社区内容的解封用了 %d 次数据库调用，期望 2 次（不含外层事务与审计）", queries)
	}
	if result.Affected != 1 || result.Detail["restored"] != int64(0) || result.Extra["restored"] != int64(0) {
		t.Fatal("空内容解封的影响数量不正确")
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var cleared bool
	if err = db.pool.QueryRow(ctx, `SELECT banned_at IS NULL AND ban_reason IS NULL AND banned_by IS NULL FROM auth_users WHERE id=$1`, user.User.ID).Scan(&cleared); err != nil || !cleared {
		t.Fatal("解封没有清空账号封禁状态", err)
	}
}
