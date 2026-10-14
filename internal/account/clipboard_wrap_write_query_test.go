package account

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPutClipboardWrapCombinesKeyCheckAndWrite(t *testing.T) {
	for _, state := range []string{"无设置", "未设密钥", "错误密钥", "设备不存在", "其他用户设备", "首次批准", "替换封装"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "clipboard-wrap-query@example.test"}).User.ID
			key := clipboardTestKeyID("wrap-query")
			if state != "无设置" && state != "未设密钥" {
				key, _ = clipboardTestReady(t, db, user, "wrap-query-retained")
			} else if state == "未设密钥" {
				if err := db.SetClipboardEnabled(ctx, user, true); err != nil {
					t.Fatal(err)
				}
			}
			owner := user
			if state == "其他用户设备" {
				owner = complete(t, db, Identity{"email", "clipboard-wrap-other@example.test"}).User.ID
			}
			device, public := clipboardTestDevice("wrap-query-pending")
			if state != "设备不存在" {
				if err := db.PutClipboardDevice(ctx, owner, device, public, "待批准设备"); err != nil {
					t.Fatal(err)
				}
			}
			wantWrap := ""
			if state == "错误密钥" || state == "替换封装" || state == "其他用户设备" {
				wantWrap = clipboardTestWrap("wrap-query-old")
				if _, err := db.pool.Exec(ctx, "INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) VALUES($1,$2,$3,$4)", owner, device, key, wantWrap); err != nil {
					t.Fatal(err)
				}
			}
			var wantErr error
			switch state {
			case "无设置", "未设密钥", "错误密钥":
				wantErr = errClipboardKeyConflict
			case "设备不存在", "其他用户设备":
				wantErr = errDataNotFound
			default:
				wantWrap = clipboardTestWrap("wrap-query-new")
			}
			wantKey := key
			if wantWrap == "" {
				wantKey = ""
			}
			if state == "错误密钥" {
				key = clipboardTestKeyID("wrap-query-wrong")
			}
			trace := &clipboardDevicePutTrace{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			err = (&Store{pool: pool}).PutClipboardWrap(ctx, user, key, device, clipboardTestWrap("wrap-query-new"))
			if !errors.Is(err, wantErr) {
				t.Fatalf("写入错误=%v，期望=%v", err, wantErr)
			}
			trace.mu.Lock()
			queries := trace.queries
			trace.mu.Unlock()
			if queries != 4 {
				t.Errorf("写入封装用了 %d 次数据库调用，期望事务、用户锁、条件写入、提交或回滚共 4 次", queries)
			}
			var storedKey, storedWrap string
			if err := db.pool.QueryRow(ctx, "SELECT COALESCE(max(key_id),''),COALESCE(max(wrap),'') FROM user_clipboard_wraps WHERE user_id=$1 AND device_id=$2", owner, device).Scan(&storedKey, &storedWrap); err != nil {
				t.Fatal(err)
			}
			if storedKey != wantKey || storedWrap != wantWrap {
				t.Fatal("保存的密钥编号或封装与预期不符")
			}
			if owner != user && clipboardTestCount(t, db, "SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1 AND device_id=$2", user, device) != 0 {
				t.Error("给其他用户的设备写入了封装")
			}
		})
	}
}
