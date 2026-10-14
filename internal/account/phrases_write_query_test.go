package account

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPutPhrasesCombinesRevisionCheckAndWrite(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "phrases-write@example.test"}).User.ID
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
	first := []Phrase{{ID: "first", Text: "收到，谢谢！\n稍后处理", Group: "工作", Position: 2}}
	next := []Phrase{{ID: "next", Text: "您好", Position: 1}}
	for _, tc := range []struct {
		name     string
		expected int64
		list     []Phrase
		conflict bool
		want     Phrases
	}{
		{"新用户版本冲突", 7, first, true, Phrases{Phrases: []Phrase{}}},
		{"首次同步", 0, first, false, Phrases{Revision: 1, Phrases: first}},
		{"过期版本冲突", 0, []Phrase{}, true, Phrases{Revision: 1, Phrases: first}},
		{"整份替换", 1, next, false, Phrases{Revision: 2, Phrases: next}},
		{"清空列表", 2, []Phrase{}, false, Phrases{Revision: 3, Phrases: []Phrase{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace.mu.Lock()
			trace.queries = 0
			trace.mu.Unlock()
			got, err := s.PutPhrases(ctx, user, tc.expected, tc.list)
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
				t.Errorf("常用语同步用了 %d 次数据库调用，期望事务、用户锁、条件写入、提交或回滚共 4 次", queries)
			}
			stored, err := db.Phrases(ctx, user)
			if err != nil || !reflect.DeepEqual(stored, tc.want) {
				t.Fatalf("保存的常用语=%+v，期望=%+v，错误=%v", stored, tc.want, err)
			}
		})
	}
}
