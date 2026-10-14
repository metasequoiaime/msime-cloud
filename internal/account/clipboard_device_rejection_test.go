package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestClipboardRejectApprovedDevicePreservesData(t *testing.T) {
	for _, throughHTTP := range []bool{false, true} {
		name := "store"
		if throughHTTP {
			name = "http"
		}
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ctx := t.Context()
			user := complete(t, s, Identity{"email", "clipboard-rejection@example.test"})
			key, device := clipboardTestReady(t, s, user.User.ID, "approved")
			if _, err := s.AddClipboard(ctx, user.User.ID, "", clipboardTestUpload(key, "需要保留的条目")); err != nil {
				t.Fatal(err)
			}
			before, err := s.ClipboardKeys(ctx, user.User.ID, device)
			if err != nil {
				t.Fatal(err)
			}
			items, err := s.ListClipboard(ctx, user.User.ID, key)
			if err != nil {
				t.Fatal(err)
			}
			if throughHTTP {
				mux := http.NewServeMux()
				Mount(mux, &Service{store: s})
				other := complete(t, s, Identity{"email", "clipboard-rejection-other@example.test"})
				apiRequest(t, mux, "DELETE", "/v1/users/me/clipboard/devices/"+device, "", other.AccessToken, http.StatusNotFound)
				r := httptest.NewRequest("DELETE", "/v1/users/me/clipboard/devices/"+device, nil)
				r.Header.Set("Authorization", "Bearer "+user.AccessToken)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"clipboard_key_rotation_required"`) {
					t.Errorf("拒绝已批准设备应要求换钥：状态=%d，响应=%s", w.Code, w.Body.String())
				}
			} else if err := s.DeleteClipboardDevice(ctx, user.User.ID, device); !errors.Is(err, errClipboardKeyRotationRequired) {
				t.Errorf("拒绝已批准设备应要求换钥：%v", err)
			}
			after, err := s.ClipboardKeys(ctx, user.User.ID, device)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("拒绝请求改变了设备、密钥或封装：之前=%+v，之后=%+v，错误=%v", before, after, err)
			}
			remaining, err := s.ListClipboard(ctx, user.User.ID, key)
			if err != nil || !reflect.DeepEqual(remaining, items) {
				t.Errorf("拒绝请求改变了条目：之前=%+v，之后=%+v，错误=%v", items, remaining, err)
			}
		})
	}
}

func TestClipboardRejectPendingDeviceUsesCurrentKey(t *testing.T) {
	for _, state := range []string{"no_settings", "current_key", "stale_wrap"} {
		t.Run(state, func(t *testing.T) {
			s := testStore(t)
			ctx := t.Context()
			user := complete(t, s, Identity{"email", "clipboard-pending@example.test"})
			if state != "no_settings" {
				clipboardTestReady(t, s, user.User.ID, "retained")
			}
			device, public := clipboardTestDevice("pending")
			if err := s.PutClipboardDevice(ctx, user.User.ID, device, public, "待批准设备"); err != nil {
				t.Fatal(err)
			}
			if state == "stale_wrap" {
				// 旧编号的封装不能把设备算作已持有当前密钥。
				if _, err := s.pool.Exec(ctx, "INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) VALUES($1,$2,$3,$4)", user.User.ID, device, clipboardTestKeyID("old"), clipboardTestWrap("old")); err != nil {
					t.Fatal(err)
				}
			}
			mux := http.NewServeMux()
			Mount(mux, &Service{store: s})
			apiRequest(t, mux, "DELETE", "/v1/users/me/clipboard/devices/"+device, "", user.AccessToken, http.StatusNoContent)
			if n := clipboardTestCount(t, s, "SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1 AND device_id=$2", user.User.ID, device); n != 0 {
				t.Fatal("待批准设备未删除", n)
			}
			if n := clipboardTestCount(t, s, "SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1 AND device_id=$2", user.User.ID, device); n != 0 {
				t.Fatal("待批准设备的旧封装未删除", n)
			}
			if err := s.DeleteClipboardDevice(ctx, user.User.ID, device); !errors.Is(err, errDataNotFound) {
				t.Fatal("重复删除未返回不存在", err)
			}
		})
	}
}

func TestClipboardRejectWaitsForConcurrentApproval(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	user := complete(t, s, Identity{"email", "clipboard-rejection-race@example.test"})
	key, _ := clipboardTestReady(t, s, user.User.ID, "existing")
	device, public := clipboardTestDevice("concurrently-approved")
	if err := s.PutClipboardDevice(ctx, user.User.ID, device, public, "等待批准"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.userDataTransaction(ctx, user.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	wrap := clipboardTestWrap("concurrently-approved")
	if _, err := tx.Exec(ctx, "INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) VALUES($1,$2,$3,$4)", user.User.ID, device, key, wrap); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.DeleteClipboardDevice(ctx, user.User.ID, device) }()
	// 等拒绝请求确实阻塞在用户行锁上，再提交批准事务。
	waitForAdminDatabaseLock(t, s, tx.Conn().PgConn().PID())
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errClipboardKeyRotationRequired) {
		t.Errorf("迟到拒绝未重新检查批准状态：%v", err)
	}
	keys, err := s.ClipboardKeys(ctx, user.User.ID, device)
	if err != nil || keys.Wrap == nil || *keys.Wrap != wrap || len(keys.Devices) != 2 {
		t.Fatalf("并发批准结果被拒绝请求删除：%+v，错误=%v", keys, err)
	}
}
