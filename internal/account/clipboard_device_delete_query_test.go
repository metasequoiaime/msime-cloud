package account

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDeleteClipboardDeviceCombinesApprovalCheckAndDelete(t *testing.T) {
	for _, state := range []string{"无设置", "当前密钥", "旧封装", "已批准", "不存在", "其他用户"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "clipboard-delete-query@example.test"}).User.ID
			owner := user
			if state == "其他用户" {
				owner = complete(t, db, Identity{"email", "clipboard-delete-other@example.test"}).User.ID
			}
			var key string
			if state != "无设置" {
				key, _ = clipboardTestReady(t, db, owner, "delete-query-retained")
			}
			device, public := clipboardTestDevice("delete-query-pending")
			if state != "不存在" {
				if err := db.PutClipboardDevice(ctx, owner, device, public, "待批准设备"); err != nil {
					t.Fatal(err)
				}
			}
			if state == "已批准" || state == "旧封装" {
				if state == "旧封装" {
					key = clipboardTestKeyID("delete-query-old")
				}
				if _, err := db.pool.Exec(ctx, "INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) VALUES($1,$2,$3,$4)", owner, device, key, clipboardTestWrap("delete-query")); err != nil {
					t.Fatal(err)
				}
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
			err = (&Store{pool: pool}).DeleteClipboardDevice(ctx, user, device)
			var wantErr error
			switch state {
			case "已批准":
				wantErr = errClipboardKeyRotationRequired
			case "不存在", "其他用户":
				wantErr = errDataNotFound
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("删除错误=%v，期望=%v", err, wantErr)
			}
			trace.mu.Lock()
			queries := trace.queries
			trace.mu.Unlock()
			if queries != 4 {
				t.Errorf("删除设备用了 %d 次数据库调用，期望事务、用户锁、条件删除、提交或回滚共 4 次", queries)
			}
			wantDevice, wantWrap := 0, 0
			if state == "已批准" {
				wantDevice, wantWrap = 1, 1
			} else if state == "其他用户" {
				wantDevice = 1
			}
			if n := clipboardTestCount(t, db, "SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1 AND device_id=$2", owner, device); n != wantDevice {
				t.Errorf("设备数=%d，期望=%d", n, wantDevice)
			}
			if n := clipboardTestCount(t, db, "SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1 AND device_id=$2", owner, device); n != wantWrap {
				t.Errorf("封装数=%d，期望=%d", n, wantWrap)
			}
			if state != "无设置" && clipboardTestCount(t, db, "SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1", owner) != 1+wantDevice {
				t.Error("删除请求改变了保留设备")
			}
		})
	}
}
