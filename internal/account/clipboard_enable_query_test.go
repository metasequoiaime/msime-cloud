package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEnableClipboardUsesOneQuery(t *testing.T) {
	for _, state := range []string{"unset", "current_key", "retention", "banned", "missing"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			uid := complete(t, db, Identity{"email", "clipboard-enable@example.test"}).User.ID
			var key string
			if state == "current_key" {
				key, _ = clipboardTestReady(t, db, uid, "enable-current")
				if _, err := db.AddClipboard(ctx, uid, "", clipboardTestUpload(key, "合成历史条目")); err != nil {
					t.Fatal(err)
				}
			}
			if state == "retention" || state == "banned" {
				if err := db.SetClipboardRetention(ctx, uid, 7); err != nil {
					t.Fatal(err)
				}
			}
			var wantErr error
			if state == "banned" {
				if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, uid); err != nil {
					t.Fatal(err)
				}
				wantErr = ErrBanned
			} else if state == "missing" {
				uid = "missing-clipboard-enable"
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
			if err := (&Store{pool: pool}).SetClipboardEnabled(ctx, uid, true); !errors.Is(err, wantErr) {
				t.Fatalf("开启同步返回错误：%v，期望 %v", err, wantErr)
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != 1 {
				t.Errorf("开启同步用了 %d 次数据库调用，期望 1 次", queries)
			}
			settings, err := db.ClipboardSettings(ctx, uid)
			wantDays := 0
			if state == "retention" || state == "banned" {
				wantDays = 7
			}
			if err != nil || settings.Enabled != (wantErr == nil) || settings.RetentionDays != wantDays || (settings.KeyID == nil) != (key == "") || (settings.KeyID != nil && *settings.KeyID != key) {
				if state == "missing" && errors.Is(err, errMissingUser) {
					return
				}
				t.Fatal("开启同步改变了保留期或密钥，或未保留封禁限制", settings, err)
			}
			if key != "" {
				if items, err := db.ListClipboard(ctx, uid, key); err != nil || len(items) != 1 || items[0].Ciphertext != clipboardTestUpload(key, "合成历史条目").Ciphertext {
					t.Fatal("重复开启同步破坏了历史条目", err)
				}
			}
		})
	}
}

func TestEnableClipboardWaitsForConcurrentUserChanges(t *testing.T) {
	for _, change := range []string{"ban", "delete", "create_settings", "replace_key"} {
		t.Run(change, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			uid := complete(t, db, Identity{"email", "clipboard-enable-wait@example.test"}).User.ID
			if change == "replace_key" {
				clipboardTestReady(t, db, uid, "enable-before")
			}
			blocker, err := db.userDataTransaction(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			done := make(chan error, 1)
			go func() { done <- db.SetClipboardEnabled(ctx, uid, true) }()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			var wantErr error
			key := clipboardTestKeyID("enable-after")
			switch change {
			case "ban":
				_, err = actionBanUser(&Service{store: db}, ctx, blocker, actionRequest{ID: uid, Reason: "合成封禁原因"})
				wantErr = ErrBanned
			case "delete":
				_, err = blocker.Exec(ctx, `DELETE FROM auth_users WHERE id=$1`, uid)
				wantErr = pgx.ErrNoRows
			default:
				_, err = blocker.Exec(ctx, `INSERT INTO user_clipboard_settings(user_id,enabled,key_id,retention_days) VALUES($1,false,$2,30)
ON CONFLICT(user_id) DO UPDATE SET enabled=false,key_id=excluded.key_id,retention_days=excluded.retention_days`, uid, key)
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
			if wantErr != nil {
				if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_clipboard_settings WHERE user_id=$1`, uid); n != 0 {
					t.Fatal("封禁或删除后开启同步创建了设置", n)
				}
				return
			}
			settings, err := db.ClipboardSettings(ctx, uid)
			if err != nil || !settings.Enabled || settings.RetentionDays != 30 || settings.KeyID == nil || *settings.KeyID != key {
				t.Fatal("开启同步覆盖了等待期间提交的保留期或密钥", settings, err)
			}
		})
	}
}
