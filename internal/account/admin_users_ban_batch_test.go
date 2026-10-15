package account

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestActionBanUserBatchesCommunityUpdates(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "ban-content-batch@example.test"})
	if _, err := db.pool.Exec(ctx, `UPDATE auth_sessions SET revoked=true WHERE user_id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	complete(t, db, Identity{"email", "ban-content-batch@example.test"})
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,design) VALUES('batch-ban-skin',$1,'Skin','{}')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES('batch-ban-resource',$1,'dictionary','Resource','{"entries":[]}','pending')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	insertCandidateSkin(t, db, "bd334455-1234-4234-8234-000000000010", user.User.ID, "Candidate")
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES('bd334455-1234-4234-8234-000000000011',$1,'sound','batch.sound','Plugin','1.0','MIT','id = "batch.sound"','zip',$2)`, user.User.ID, hash("batch-ban-plugin")); err != nil {
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	result, err := actionBanUser(&Service{store: &Store{pool: pool}}, ctx, tx, actionRequest{ID: user.User.ID, Reason: "spam"})
	if err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.seen
	var updates int
	for _, statement := range trace.statements {
		if strings.Contains(statement, "UPDATE community_") {
			updates++
		}
	}
	trace.mu.Unlock()
	if queries != 2 {
		t.Errorf("封禁用了 %d 次数据库调用，期望 2 次（不含外层事务与审计）", queries)
	}
	if updates != 1 {
		t.Fatalf("封禁四张社区表用了 %d 次数据库更新，期望批量写入 1 次", updates)
	}
	if removed, ok := result.Detail["removed"].(int64); !ok || removed != 4 {
		t.Fatalf("封禁移除数量 %v，期望 4", result.Detail["removed"])
	}
	if sessions, ok := result.Detail["sessions"].(int64); !ok || sessions != 1 || result.Extra["sessions"] != sessions {
		t.Fatal("撤销会话数量包含了已撤销的会话", result.Detail["sessions"])
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var removed int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM community_skins WHERE owner_id=$1 AND moderation='removed'`, user.User.ID).Scan(&removed); err != nil || removed != 1 {
		t.Fatalf("皮肤封禁状态 %d，期望 1：%v", removed, err)
	}
}
