package account

import (
	"context"
	"errors"
	"testing"
)

// 社区删除与发布一样必须在作者行锁内复查封禁；否则封禁事务下架后，迟到的删除会把本应由解封恢复的作品永久删掉。
func TestCommunityDeleteWaitsForConcurrentBan(t *testing.T) {
	for _, tc := range []struct {
		table string
		id    string
		seed  string
	}{
		{"community_skins", "ab334455-1234-1234-1234-000000000001", `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'皮肤','{}')`},
		{"community_candidate_skins", "ab334455-1234-1234-1234-000000000002", `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES($1,$2,'delete-race','候选皮肤','1.0','CC0','manifest','preview.png',repeat('0',64))`},
		{"community_resources", "delete-race-resource", `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'reply','回复','{"prompt":"你好"}')`},
		{"community_plugins", "ab334455-1234-1234-1234-000000000003", `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES($1,$2,'sound','delete-race','插件','1.0','MIT','manifest','zip',repeat('0',64))`},
	} {
		t.Run(tc.table, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "community-delete-ban-" + tc.table + "@example.test"})
			if _, err := db.pool.Exec(ctx, tc.seed, tc.id, user.User.ID); err != nil {
				t.Fatal(err)
			}

			ban, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer ban.Rollback(context.Background())
			if _, err := actionBanUser(&Service{store: db}, ctx, ban, actionRequest{ID: user.User.ID, Reason: "合成封禁原因"}); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				_, err := (&Service{store: db}).deleteOwnedCommunity(ctx, tc.table, tc.id, user.User.ID)
				done <- err
			}()
			waitForAdminDatabaseLock(t, db, ban.Conn().PgConn().PID())
			if err := ban.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrBanned) {
				t.Fatalf("封禁提交后迟到删除未被拒绝：%v", err)
			}
			var exists bool
			if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+tc.table+` WHERE id=$1)`, tc.id).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if !exists {
				t.Fatal("封禁后的作品被迟到删除永久移除")
			}
		})
	}
}
