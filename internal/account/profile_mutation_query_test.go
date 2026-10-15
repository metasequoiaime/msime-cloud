package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProfileMutationsCombineUserLockAndWrite(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}
	for _, operation := range []string{"update_name", "reset_name", "replace_avatar", "clear_avatar", "avatar_with_me", "delete_user"} {
		for _, state := range []string{"enabled", "banned", "missing"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				uid := randomToken()
				if state != "missing" {
					uid = complete(t, db, Identity{"email", uid + "@example.test"}).User.ID
					if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET display_name='原昵称',avatar_key='avatars/old.jpg',banned_at=CASE WHEN $2 THEN now() END WHERE id=$1`, uid, state == "banned"); err != nil {
						t.Fatal(err)
					}
				}
				trace.mu.Lock()
				trace.seen, trace.statements = 0, nil
				trace.mu.Unlock()
				var key string
				var profile User
				var ids []Identity
				var err error
				switch operation {
				case "update_name":
					err = store.UpdateName(ctx, uid, "新昵称")
				case "reset_name":
					err = store.UpdateName(ctx, uid, "")
				case "replace_avatar":
					key, err = store.SetAvatarKey(ctx, uid, "avatars/new.jpg")
				case "clear_avatar":
					key, err = store.SetAvatarKey(ctx, uid, "")
				case "avatar_with_me":
					key, profile, ids, err = store.SetAvatarKeyAndMe(ctx, uid, "avatars/new.jpg")
				case "delete_user":
					key, err = store.DeleteUserWithAvatarKey(ctx, uid)
				}
				trace.mu.Lock()
				calls := trace.seen
				trace.mu.Unlock()
				wantCalls := 1
				if operation == "avatar_with_me" {
					wantCalls = 3
				}
				if calls != wantCalls {
					t.Errorf("资料修改用了 %d 次数据库调用，期望 %d 次", calls, wantCalls)
				}
				var wantErr error
				if state == "banned" {
					wantErr = ErrBanned
				} else if state == "missing" && operation != "delete_user" {
					wantErr = ErrInvalid
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("资料修改错误=%v，期望 %v", err, wantErr)
				}
				if state == "enabled" && operation != "update_name" && operation != "reset_name" && key != "avatars/old.jpg" {
					t.Fatalf("旧头像键=%q，期望 avatars/old.jpg", key)
				}
				if state == "enabled" && operation == "avatar_with_me" && (profile.ID != uid || profile.avatarKey != "avatars/new.jpg" || profile.DisplayName != "原昵称" || len(ids) != 1) {
					t.Fatalf("头像响应与刚写入的用户资料不一致：%+v，%v", profile, ids)
				}
				var name, avatar string
				err = db.pool.QueryRow(ctx, `SELECT display_name,avatar_key FROM auth_users WHERE id=$1`, uid).Scan(&name, &avatar)
				if state == "missing" || (state == "enabled" && operation == "delete_user") {
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatal("缺失或删除的账号仍然存在", err)
					}
					return
				}
				wantName, wantAvatar := "原昵称", "avatars/old.jpg"
				if state == "enabled" {
					switch operation {
					case "update_name":
						wantName = "新昵称"
					case "reset_name":
						wantName = defaultUserName(uid)
					case "replace_avatar", "avatar_with_me":
						wantAvatar = "avatars/new.jpg"
					case "clear_avatar":
						wantAvatar = ""
					}
				}
				if err != nil || name != wantName || avatar != wantAvatar {
					t.Fatalf("持久化昵称=%q、头像=%q，期望 %q、%q：%v", name, avatar, wantName, wantAvatar, err)
				}
			})
		}
	}
}

func TestAvatarMutationReturnsKeyCommittedDuringLockWait(t *testing.T) {
	for _, operation := range []string{"replace", "with_me", "delete_user"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			uid := complete(t, db, Identity{"email", "avatar-latest@example.test"}).User.ID
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(ctx, `UPDATE auth_users SET avatar_key='avatars/latest.jpg' WHERE id=$1`, uid); err != nil {
				t.Fatal(err)
			}
			type result struct {
				key string
				err error
			}
			done := make(chan result, 1)
			go func() {
				var key string
				var err error
				switch operation {
				case "replace":
					key, err = db.SetAvatarKey(ctx, uid, "avatars/replacement.jpg")
				case "with_me":
					key, _, _, err = db.SetAvatarKeyAndMe(ctx, uid, "avatars/replacement.jpg")
				case "delete_user":
					key, err = db.DeleteUserWithAvatarKey(ctx, uid)
				}
				done <- result{key, err}
			}()
			waitForAdminDatabaseLock(t, db, tx.Conn().PgConn().PID())
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.err != nil || got.key != "avatars/latest.jpg" {
				t.Fatalf("没有返回锁等待期间提交的头像键：%q，%v", got.key, got.err)
			}
		})
	}
}

func TestAvatarProfileReadFailureRollsBackRegistration(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	uid := complete(t, db, Identity{"email", "avatar-profile-read-failure@example.test"}).User.ID
	// PostgreSQL 接受 `infinity`，但资料响应的 `time.Time` 无法扫描该值，令错误发生在头像写入之后。
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET avatar_key='avatars/old.jpg',created_at='infinity' WHERE id=$1`, uid); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.SetAvatarKeyAndMe(ctx, uid, "avatars/new.jpg"); err == nil {
		t.Fatal("资料响应读取失败仍然成功")
	}
	if key, err := db.AvatarKey(ctx, uid); err != nil || key != "avatars/old.jpg" {
		t.Fatalf("资料响应读取失败后未回滚头像登记：%q，%v", key, err)
	}
}
