package account

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRevisionWritesPreserveFailureResults(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "revision-failures@example.test"}).User.ID
	settings := map[string]json.RawMessage{"general.enable_emoji": json.RawMessage(`true`)}
	phrases := []Phrase{{ID: "first", Text: "收到"}}
	for _, operation := range []struct {
		name      string
		put       func(*Store) (any, error)
		zero, out any
	}{
		{"设置", func(s *Store) (any, error) { return s.PutPreferences(ctx, user, 0, settings) }, Preferences{}, Preferences{Revision: 1, Settings: settings}},
		{"常用语", func(s *Store) (any, error) { return s.PutPhrases(ctx, user, 0, phrases) }, Phrases{}, Phrases{Revision: 1, Phrases: phrases}},
	} {
		for index, stage := range []string{"开启事务", "锁定用户", "条件写入", "提交事务"} {
			t.Run(operation.name+"/"+stage, func(t *testing.T) {
				fault := &statementCancellation{at: index + 1}
				cfg := db.pool.Config()
				cfg.ConnConfig.Tracer = fault
				cfg.MaxConns = 1
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer pool.Close()
				got, err := operation.put(&Store{pool: pool})
				if err == nil {
					t.Fatal("数据库失败未返回错误")
				}
				want := operation.zero
				// 提交前已经构造的结果必须随提交错误一起返回，较早失败则返回零值。
				if stage == "提交事务" {
					want = operation.out
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("失败结果=%+v，期望=%+v，错误=%v", got, want, err)
				}
				if n := clipboardTestCount(t, db, `SELECT (SELECT count(*) FROM user_preferences WHERE user_id=$1)+(SELECT count(*) FROM user_phrases WHERE user_id=$1)`, user); n != 0 {
					t.Fatalf("失败写入未回滚，记录数=%d", n)
				}
			})
		}
	}
}

func TestRevisionWriteRejectsInvalidJSONBeforeWrite(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "revision-invalid-json@example.test"}).User.ID
	got, err := db.PutPreferences(ctx, user, 0, map[string]json.RawMessage{"general.enable_emoji": json.RawMessage(`[`)})
	if err == nil || !reflect.DeepEqual(got, Preferences{}) {
		t.Fatalf("JSON 编码失败的结果=%+v，错误=%v", got, err)
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_preferences WHERE user_id=$1`, user); n != 0 {
		t.Fatalf("JSON 编码失败后仍写入了%d条记录", n)
	}
}
