package account

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRotateClipboardKeyBatchesReplacementWrites(t *testing.T) {
	for _, failInsert := range []bool{false, true} {
		name := "成功换钥"
		if failInsert {
			name = "末条写入失败回滚"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "clipboard-rotation-batch@example.test"}).User.ID
			key, device := clipboardTestReady(t, db, user, "rotation-batch")
			revoked, public := clipboardTestDevice("revoked-batch")
			if err := db.PutClipboardDevice(ctx, user, revoked, public, "待撤销设备"); err != nil {
				t.Fatal(err)
			}
			if err := db.PutClipboardWrap(ctx, user, key, revoked, clipboardTestWrap("revoked-batch")); err != nil {
				t.Fatal(err)
			}
			if _, err := db.AddClipboard(ctx, user, "测试设备", clipboardTestUpload(key, "需要清空的条目")); err != nil {
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
			if failInsert {
				if _, err := db.pool.Exec(ctx, `CREATE FUNCTION reject_rotation_wrap() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''合成封装写入失败''; END';
 CREATE TRIGGER reject_rotation_wrap BEFORE INSERT ON user_clipboard_wraps FOR EACH ROW EXECUTE FUNCTION reject_rotation_wrap()`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := db.pool.Exec(context.Background(), `DROP TRIGGER reject_rotation_wrap ON user_clipboard_wraps; DROP FUNCTION reject_rotation_wrap()`); err != nil {
						t.Error(err)
					}
				})
			}
			trace := &clipboardKeysBatchTrace{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			next := clipboardTestKeyID("next-batch")
			wrap := clipboardTestWrap("next-batch")
			err = (&Store{pool: pool}).RotateClipboardKey(ctx, user, ClipboardKeyChange{Previous: &key, KeyID: next, Wraps: []ClipboardWrap{{device, wrap}}, Revoke: []string{revoked}})
			if failInsert && err == nil || !failInsert && err != nil {
				t.Fatalf("换钥结果与故障注入不符：%v", err)
			}
			trace.mu.Lock()
			batches, queries := trace.batches, trace.queries
			trace.mu.Unlock()
			if !failInsert && (batches != 1 || queries != 4) {
				t.Errorf("换钥写入使用了 %d 次批量请求、批内 %d 条语句，期望 1、4", batches, queries)
			}
			afterKeys, err := db.ClipboardKeys(ctx, user, device)
			if err != nil {
				t.Fatal(err)
			}
			afterItems, err := db.ListClipboard(ctx, user, key)
			if err != nil {
				t.Fatal(err)
			}
			if failInsert {
				if !reflect.DeepEqual(beforeKeys, afterKeys) || !reflect.DeepEqual(beforeItems, afterItems) {
					t.Fatal("失败换钥没有回滚设备撤销、历史清理、密钥编号或旧封装")
				}
				return
			}
			if !afterKeys.Enabled || afterKeys.KeyID == nil || *afterKeys.KeyID != next || afterKeys.Wrap == nil || *afterKeys.Wrap != wrap || len(afterKeys.Devices) != 1 || afterKeys.Devices[0].DeviceID != device || !afterKeys.Devices[0].Approved {
				t.Fatalf("新密钥、封装或设备不正确：%+v", afterKeys)
			}
			if n := clipboardTestCount(t, db, `SELECT (SELECT count(*) FROM user_clipboard WHERE user_id=$1)+(SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1 AND key_id<>$2)`, user, next); n != 0 {
				t.Fatalf("换钥留下了历史条目或旧封装：%d", n)
			}
		})
	}
}
