package account

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type positionMutationTrace struct {
	statementCancellation
	batches, writes int
}

func (f *positionMutationTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	f.mu.Lock()
	f.batches++
	f.writes += data.Batch.Len()
	f.mu.Unlock()
	return ctx
}
func (*positionMutationTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (*positionMutationTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

func TestSetCandidatePositionBatchesMutationQueries(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "positions-batch@example.test"}).User.ID
	cfg := db.pool.Config()
	trace := &positionMutationTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}
	var revision int64
	first := CandidatePosition{Context: "batch", Code: "one", Word: "合成词一", Position: 1}
	second := CandidatePosition{Context: "batch", Code: "two", Word: "合成词二", Position: 1}
	moved := second
	moved.Position = 2
	removed := moved
	removed.Position = 0
	for _, tc := range []struct {
		name     string
		position CandidatePosition
		want     []CandidatePosition
	}{
		{"首次固定", first, []CandidatePosition{first}},
		{"替换槽位", second, []CandidatePosition{second}},
		{"移动槽位", moved, []CandidatePosition{moved}},
		{"重复固定", moved, []CandidatePosition{moved}},
		{"清除固定", removed, []CandidatePosition{}},
		{"重复清除", removed, []CandidatePosition{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace.mu.Lock()
			trace.seen, trace.batches, trace.writes = 0, 0, 0
			trace.mu.Unlock()
			next, err := store.SetCandidatePosition(ctx, user, revision, tc.position)
			if err != nil {
				t.Fatal(err)
			}
			if next != revision+1 {
				t.Fatalf("revision=%d，期望 %d", next, revision+1)
			}
			revision = next
			trace.mu.Lock()
			seen, batches, writes := trace.seen, trace.batches, trace.writes
			trace.mu.Unlock()
			expectedWrites := 4
			if tc.position.Position == 0 {
				expectedWrites = 3
			}
			if seen != 4 || batches != 1 || writes != expectedWrites {
				t.Errorf("数据库调用：普通 %d 次、批处理 %d 次、批内 %d 条；期望 4、1、%d", seen, batches, writes, expectedWrites)
			}
			got, _, err := db.CandidatePositions(ctx, user, "batch", 0, 20)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("固定位置=%v，期望 %v：%v", got, tc.want, err)
			}
			changes, _, err := db.DictionaryChanges(ctx, user, revision-1, 20)
			if err != nil || len(changes) != 1 || changes[0].Revision != revision || changes[0].Position == nil || *changes[0].Position != tc.position {
				t.Fatalf("变更记录=%v：%v", changes, err)
			}
		})
	}
}

func TestSetCandidatePositionBatchRollsBackAfterEarlierWrites(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "positions-rollback@example.test"}).User.ID
	original := CandidatePosition{Context: "rollback", Code: "one", Word: "合成词一", Position: 1}
	revision, err := db.SetCandidatePosition(ctx, user, 0, original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `CREATE FUNCTION reject_position_change() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''合成写入失败''; END'; CREATE TRIGGER reject_position_change BEFORE INSERT ON user_dictionary_changes FOR EACH ROW EXECUTE FUNCTION reject_position_change()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.pool.Exec(ctx, `DROP TRIGGER reject_position_change ON user_dictionary_changes; DROP FUNCTION reject_position_change()`); err != nil {
			t.Error(err)
		}
	}()
	for _, p := range []CandidatePosition{
		{Context: "rollback", Code: "two", Word: "合成词二", Position: 1},
		{Context: "rollback", Code: "one", Word: "合成词一"},
	} {
		if _, err := db.SetCandidatePosition(ctx, user, revision, p); err == nil {
			t.Fatal("变更记录失败时未返回错误")
		}
		got, _, err := db.CandidatePositions(ctx, user, "rollback", 0, 20)
		if err != nil || !reflect.DeepEqual(got, []CandidatePosition{original}) {
			t.Fatalf("失败后固定位置=%v：%v", got, err)
		}
		var current, count int64
		if err := db.pool.QueryRow(ctx, `SELECT revision,(SELECT count(*) FROM user_dictionary_changes WHERE user_id=$1) FROM user_dictionary_state WHERE user_id=$1`, user).Scan(&current, &count); err != nil || current != revision || count != 1 {
			t.Fatalf("失败后 revision=%d，变更数=%d：%v", current, count, err)
		}
	}
}
