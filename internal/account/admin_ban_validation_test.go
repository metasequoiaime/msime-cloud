package account

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestLockUserForBanAction(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	id := complete(t, db, Identity{"email", "ban-validation@example.test"}).User.ID
	reason := "合成封禁原因"
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET display_name='合成封禁用户',banned_at=now(),ban_reason=$2 WHERE id=$1`, id, reason); err != nil {
		t.Fatal(err)
	}
	var token string
	if err := db.pool.QueryRow(ctx, "SELECT "+banVersionSQL+" FROM auth_users WHERE id=$1", id).Scan(&token); err != nil {
		t.Fatal(err)
	}
	stale := "0" + token[1:]
	if stale == token {
		stale = "1" + token[1:]
	}
	value, _ := json.Marshal(map[string]string{"token": token})
	oldValue, _ := json.Marshal(map[string]string{"token": stale})
	extra, _ := json.Marshal(map[string]any{"token": token, "extra": true})
	for _, tc := range []struct {
		name      string
		id        string
		value     json.RawMessage
		closed    bool
		status    int
		code      string
		wantError error
	}{
		{name: "未携带版本", id: id},
		{name: "当前版本", id: id, value: value},
		{name: "过期版本", id: id, value: oldValue, status: 409, code: "conflict"},
		{name: "无效版本", id: id, value: json.RawMessage(`{"token":"bad"}`), status: 400, code: "invalid_value"},
		{name: "额外字段", id: id, value: extra, status: 400, code: "invalid_value"},
		{name: "空值", id: id, value: json.RawMessage(`null`), status: 400, code: "invalid_value"},
		{name: "用户缺失优先于版本格式", id: "missing-user", value: json.RawMessage(`{"token":"bad"}`), status: 404, code: "not_found"},
		{name: "数据库错误优先于版本格式", id: id, value: json.RawMessage(`{"token":"bad"}`), closed: true, wantError: pgx.ErrTxClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if tc.closed {
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}
			got, err := lockUserForBanAction(ctx, tx, actionRequest{ID: tc.id, Value: tc.value})
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("数据库错误未保留：%v", err)
				}
				return
			}
			if tc.status != 0 {
				var failure *actionError
				if !errors.As(err, &failure) || failure.status != tc.status || failure.code != tc.code {
					t.Fatalf("错误响应不兼容：%v，期望=%d/%s", err, tc.status, tc.code)
				}
				return
			}
			want := userBanSnapshot{Name: "合成封禁用户", Banned: true, Reason: &reason, Token: token}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("封禁快照不兼容：%+v，错误=%v", got, err)
			}
		})
	}
}
