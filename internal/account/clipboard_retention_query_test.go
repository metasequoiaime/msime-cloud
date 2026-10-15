package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestClipboardRetentionUsesOneQuery(t *testing.T) {
	for _, state := range []string{"unset", "enabled", "disabled", "banned", "banned_unset", "missing"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "retention-query@example.test"})
			uid := user.User.ID
			var key string
			if state == "enabled" || state == "banned" {
				key, _ = clipboardTestReady(t, db, uid, "retention-query")
			}
			if state == "disabled" {
				if err := db.SetClipboardEnabled(ctx, uid, false); err != nil {
					t.Fatal(err)
				}
			}
			var wantErr error
			if state == "banned" || state == "banned_unset" {
				if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, uid); err != nil {
					t.Fatal(err)
				}
				wantErr = ErrBanned
			} else if state == "missing" {
				uid = "missing-retention-user"
				wantErr = pgx.ErrNoRows
			}
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if err := pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			if err := (&Store{pool: pool}).SetClipboardRetention(ctx, uid, 7); !errors.Is(err, wantErr) {
				t.Fatalf("保留期设置返回错误：%v，期望 %v", err, wantErr)
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != 1 {
				t.Errorf("设置保留期使用了 %d 次数据库调用，期望 1 次", queries)
			}
			settings, err := db.ClipboardSettings(ctx, uid)
			wantDays := 7
			if wantErr != nil {
				wantDays = 0
			}
			if err != nil || settings.RetentionDays != wantDays || settings.Enabled != (key != "") || (settings.KeyID == nil) != (key == "") || (settings.KeyID != nil && *settings.KeyID != key) {
				t.Fatal("保留期、同步开关或密钥不正确", settings, err)
			}
			if state == "missing" && clipboardTestCount(t, db, `SELECT count(*) FROM user_clipboard_settings WHERE user_id=$1`, uid) != 0 {
				t.Fatal("不存在的用户被写入了剪贴板设置")
			}
		})
	}
}

func TestClipboardRetentionWaitsForConcurrentUserChanges(t *testing.T) {
	for _, change := range []string{"ban", "delete", "create_settings", "replace_key"} {
		t.Run(change, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			user := complete(t, db, Identity{"email", "retention-wait@example.test"})
			uid := user.User.ID
			if change != "create_settings" {
				clipboardTestReady(t, db, uid, "retention-before")
				if err := db.SetClipboardRetention(ctx, uid, 1); err != nil {
					t.Fatal(err)
				}
			}
			before, err := db.ClipboardSettings(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := db.userDataTransaction(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			done := make(chan error, 1)
			go func() { done <- db.SetClipboardRetention(ctx, uid, 7) }()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			var wantErr error
			key := clipboardTestKeyID("retention-after")
			switch change {
			case "ban":
				_, err = actionBanUser(&Service{store: db}, ctx, blocker, actionRequest{ID: uid, Reason: "合成封禁原因"})
				wantErr = ErrBanned
			case "delete":
				_, err = blocker.Exec(ctx, `DELETE FROM auth_users WHERE id=$1`, uid)
				wantErr = pgx.ErrNoRows
			default:
				// 在保留期写入已经等待用户锁后创建或替换设置，验证冲突更新保留最新密钥。
				_, err = blocker.Exec(ctx, `INSERT INTO user_clipboard_settings(user_id,enabled,key_id,retention_days) VALUES($1,true,$2,30)
ON CONFLICT(user_id) DO UPDATE SET enabled=true,key_id=excluded.key_id,retention_days=excluded.retention_days`, uid, key)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, wantErr) {
				t.Fatalf("锁等待后返回错误：%v，期望 %v", err, wantErr)
			}
			after, err := db.ClipboardSettings(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "ban":
				if after.RetentionDays != before.RetentionDays || after.Enabled != before.Enabled || after.KeyID == nil || before.KeyID == nil || *after.KeyID != *before.KeyID {
					t.Fatal("封禁后保留期请求改变了原设置", after)
				}
			case "delete":
				if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_clipboard_settings WHERE user_id=$1`, uid); n != 0 {
					t.Fatal("删除用户后保留期请求重建了设置", n)
				}
			default:
				if !after.Enabled || after.KeyID == nil || *after.KeyID != key || after.RetentionDays != 7 {
					t.Fatal("保留期写入覆盖了等待期间提交的同步开关或密钥", after)
				}
			}
		})
	}
}
