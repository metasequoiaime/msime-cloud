package account

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRotateClipboardKeyCombinesRevocationAndDeviceCheck(t *testing.T) {
	for _, state := range []string{"成功换钥", "封装给已撤销设备", "封装给未登记设备"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "clipboard-rotation-devices@example.test"}).User.ID
			key, device := clipboardTestReady(t, db, user, "rotation-devices-retained")
			revoked, public := clipboardTestDevice("rotation-devices-revoked")
			if err := db.PutClipboardDevice(ctx, user, revoked, public, "待撤销设备"); err != nil {
				t.Fatal(err)
			}
			if err := db.PutClipboardWrap(ctx, user, key, revoked, clipboardTestWrap("rotation-devices-old")); err != nil {
				t.Fatal(err)
			}
			if _, err := db.AddClipboard(ctx, user, "", clipboardTestUpload(key, "换钥前的条目")); err != nil {
				t.Fatal(err)
			}
			beforeKeys, err := db.ClipboardKeys(ctx, user, device)
			if err != nil {
				t.Fatal(err)
			}
			beforeItems, err := db.ListClipboard(ctx, user, key)
			if err != nil {
				t.Fatal(err)
			}
			wrap := clipboardTestWrap("rotation-devices-new")
			change := ClipboardKeyChange{Previous: &key, KeyID: clipboardTestKeyID("rotation-devices-new"), Wraps: []ClipboardWrap{{device, wrap}}, Revoke: []string{revoked}}
			switch state {
			case "封装给已撤销设备":
				change.Wraps = append(change.Wraps, ClipboardWrap{revoked, wrap})
			case "封装给未登记设备":
				missing, _ := clipboardTestDevice("rotation-devices-missing")
				change.Wraps = append(change.Wraps, ClipboardWrap{missing, wrap})
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
			err = (&Store{pool: pool}).RotateClipboardKey(ctx, user, change)
			wantCalls := 6
			if state == "成功换钥" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				wantCalls = 5
				if !errors.Is(err, errClipboardKeyInvalid) {
					t.Fatalf("无效封装未被拒绝：%v", err)
				}
			}
			trace.mu.Lock()
			queries := trace.queries
			trace.mu.Unlock()
			if queries != wantCalls {
				t.Errorf("换钥用了 %d 次数据库调用，期望 %d 次", queries, wantCalls)
			}
			afterKeys, err := db.ClipboardKeys(ctx, user, device)
			if err != nil {
				t.Fatal(err)
			}
			afterItems, err := db.ListClipboard(ctx, user, key)
			if err != nil {
				t.Fatal(err)
			}
			if state != "成功换钥" {
				if !reflect.DeepEqual(afterKeys, beforeKeys) {
					t.Fatal("失败换钥没有回滚设备、密钥编号或封装")
				}
				if !reflect.DeepEqual(afterItems, beforeItems) {
					t.Fatal("失败换钥改变了历史条目")
				}
				return
			}
			if afterKeys.KeyID == nil || *afterKeys.KeyID != change.KeyID || afterKeys.Wrap == nil || *afterKeys.Wrap != wrap || len(afterKeys.Devices) != 1 || afterKeys.Devices[0].DeviceID != device || !afterKeys.Devices[0].Approved || len(afterItems) != 0 {
				t.Fatal("换钥后密钥、封装、设备或历史清理不正确")
			}
		})
	}
}
