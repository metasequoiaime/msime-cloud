package account

import (
	"context"
	"testing"
	"time"
)

func TestClipboardKeysSnapshotDuringRotation(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	user := complete(t, db, Identity{"email", "keys-snapshot@example.test"})
	uid := user.User.ID
	oldKey, first := clipboardTestReady(t, db, uid, "snapshot-first")
	second, public := clipboardTestDevice("snapshot-second")
	if err := db.PutClipboardDevice(ctx, uid, second, public, "待批准设备"); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.userDataTransaction(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `UPDATE user_clipboard_devices SET last_seen=last_seen WHERE user_id=$1 AND device_id=$2`, uid, first); err != nil {
		t.Fatal(err)
	}
	type result struct {
		keys ClipboardKeys
		err  error
	}
	done := make(chan result, 1)
	go func() {
		keys, err := db.ClipboardKeys(ctx, uid, first)
		done <- result{keys, err}
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	newKey := clipboardTestKeyID("snapshot-new")
	if _, err := blocker.Exec(ctx, `UPDATE user_clipboard_settings SET key_id=$2 WHERE user_id=$1`, uid, newKey); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `DELETE FROM user_clipboard_wraps WHERE user_id=$1`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) VALUES($1,$2,$3,$4)`, uid, second, newKey, clipboardTestWrap("snapshot-new")); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || !got.keys.Enabled || got.keys.KeyID == nil || len(got.keys.Devices) != 2 {
		t.Fatal("并发换钥期间读取密钥失败", got.err)
	}
	isOld := *got.keys.KeyID == oldKey
	if !isOld && *got.keys.KeyID != newKey {
		t.Fatal("返回了未知密钥编号")
	}
	if isOld {
		if got.keys.Wrap == nil || *got.keys.Wrap != clipboardTestWrap("snapshot-first") {
			t.Fatal("旧密钥编号未匹配旧封装")
		}
	} else if got.keys.Wrap != nil {
		t.Fatal("新密钥仍返回请求设备的旧封装")
	}
	for _, device := range got.keys.Devices {
		if device.DeviceID != first && device.DeviceID != second {
			t.Fatal("返回了不属于用户的设备")
		}
		if device.Approved != ((device.DeviceID == first) == isOld) {
			t.Fatal("设备批准状态与返回的密钥不属于同一快照")
		}
	}
}
