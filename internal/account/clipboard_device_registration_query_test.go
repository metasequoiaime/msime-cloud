package account

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPutClipboardDeviceCombinesExistingAndNewRegistration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		count    int
		existing bool
		approved bool
		wantErr  error
	}{
		{"首次登记", 0, false, false, nil},
		{"设备改名", 1, true, false, nil},
		{"满额已批准时拒绝新设备", clipboardDeviceLimit, false, true, errClipboardDeviceLimit},
		{"满额已批准时改名", clipboardDeviceLimit, true, true, nil},
		{"满额待批准时改名", clipboardDeviceLimit, true, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "clipboard-registration@example.test"}).User.ID
			oldTime := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			if _, err := db.pool.Exec(ctx, `INSERT INTO user_clipboard_devices(user_id,device_id,public_key,name,created_at,last_seen)
 SELECT $1,lpad(to_hex(i),32,'0'),'original-key','原名',$2,$2 FROM generate_series(0,$3::integer-1) AS s(i)`, user, oldTime, tc.count); err != nil {
				t.Fatal(err)
			}
			if tc.approved {
				key := clipboardTestKeyID("registration-current")
				if _, err := db.pool.Exec(ctx, "INSERT INTO user_clipboard_settings(user_id,enabled,key_id) VALUES($1,true,$2)", user, key); err != nil {
					t.Fatal(err)
				}
				if _, err := db.pool.Exec(ctx, "INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) SELECT user_id,device_id,$2,$3 FROM user_clipboard_devices WHERE user_id=$1", user, key, clipboardTestWrap("registration-current")); err != nil {
					t.Fatal(err)
				}
			}
			device := fmt.Sprintf("%032x", tc.count)
			if tc.existing {
				device = fmt.Sprintf("%032x", 0)
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
			err = (&Store{pool: pool}).PutClipboardDevice(ctx, user, device, "new-key", "更新名称")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("登记错误=%v，期望=%v", err, tc.wantErr)
			}
			trace.mu.Lock()
			queries := trace.queries
			trace.mu.Unlock()
			if queries != 4 {
				t.Errorf("登记设备用了 %d 次数据库调用，期望事务、用户锁、条件写入、提交或回滚共 4 次", queries)
			}
			wantCount := tc.count
			if !tc.existing && tc.wantErr == nil {
				wantCount++
			}
			if n := clipboardTestCount(t, db, "SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1", user); n != wantCount {
				t.Fatalf("设备数=%d，期望=%d", n, wantCount)
			}
			if tc.approved && clipboardTestCount(t, db, "SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1", user) != tc.count {
				t.Fatal("登记请求改变了已批准设备的封装")
			}
			if tc.wantErr != nil {
				if n := clipboardTestCount(t, db, "SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1 AND public_key='original-key' AND name='原名' AND created_at=$2 AND last_seen=$2", user, oldTime); n != tc.count {
					t.Fatal("失败登记改变了已有设备")
				}
				return
			}
			var public, name string
			var created, seen time.Time
			if err := db.pool.QueryRow(ctx, "SELECT public_key,name,created_at,last_seen FROM user_clipboard_devices WHERE user_id=$1 AND device_id=$2", user, device).Scan(&public, &name, &created, &seen); err != nil {
				t.Fatal(err)
			}
			if name != "更新名称" || !seen.After(oldTime) {
				t.Fatal("设备名称或活跃时间未更新")
			}
			if tc.existing {
				if public != "original-key" || !created.Equal(oldTime) {
					t.Fatal("改名改变了设备公钥或登记时间")
				}
			} else if public != "new-key" {
				t.Fatal("新设备公钥未保存")
			}
		})
	}
}
