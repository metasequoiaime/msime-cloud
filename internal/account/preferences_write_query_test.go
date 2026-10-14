package account

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPutPreferencesCombinesRevisionCheckAndWrite(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "preferences-write@example.test"}).User.ID
	trace := &clipboardDevicePutTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &Store{pool: pool}
	first := map[string]json.RawMessage{"general.enable_emoji": json.RawMessage(`true`)}
	for _, tc := range []struct {
		name     string
		expected int64
		settings map[string]json.RawMessage
		conflict bool
		want     Preferences
	}{
		{"新用户版本冲突", 7, first, true, Preferences{Settings: map[string]json.RawMessage{}}},
		{"首次同步", 0, first, false, Preferences{Revision: 1, Settings: first}},
		{"过期版本冲突", 0, map[string]json.RawMessage{}, true, Preferences{Revision: 1, Settings: first}},
		{"覆盖为空设置", 1, map[string]json.RawMessage{}, false, Preferences{Revision: 2, Settings: map[string]json.RawMessage{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace.mu.Lock()
			trace.queries = 0
			trace.mu.Unlock()
			got, err := s.PutPreferences(ctx, user, tc.expected, tc.settings)
			if tc.conflict {
				if !errors.Is(err, errRevisionConflict) {
					t.Fatalf("版本冲突未被拒绝：%v", err)
				}
			} else if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("同步结果=%+v，期望=%+v，错误=%v", got, tc.want, err)
			}
			trace.mu.Lock()
			queries := trace.queries
			trace.mu.Unlock()
			if queries != 4 {
				t.Errorf("设置同步用了 %d 次数据库调用，期望事务、用户锁、条件写入、提交或回滚共 4 次", queries)
			}
			stored, err := db.Preferences(ctx, user)
			if err != nil || !reflect.DeepEqual(stored, tc.want) {
				t.Fatalf("保存的设置=%+v，期望=%+v，错误=%v", stored, tc.want, err)
			}
		})
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_preferences WHERE user_id=$1`, user); n != 1 {
		t.Fatalf("设置记录数=%d，期望 1", n)
	}
}
