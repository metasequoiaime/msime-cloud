package account

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// countedDictionaryTx 记录真实 PostgreSQL 事务的查询次数，防止恢复逐条往返。
type countedDictionaryTx struct {
	pgx.Tx
	queries int
}

func (tx *countedDictionaryTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.queries++
	return tx.Tx.Query(ctx, sql, args...)
}
func (tx *countedDictionaryTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.queries++
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *countedDictionaryTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.queries++
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestMergeResourceDictionaryBatch(t *testing.T) {
	s := testStore(t)
	user := complete(t, s, Identity{"email", "batch@example.test"}).User.ID
	other := complete(t, s, Identity{"email", "other@example.test"}).User.ID
	ctx := t.Context()
	seed := []SharedWord{{"pinyin", "shared", "共享", 7}, {"quick", "shared", "共享", 10}, {"quick", "keep", "保留", 4}}
	var original DictionaryEntry
	for i, e := range seed {
		change, err := s.EditDictionary(ctx, user, e.Kind, "", 0, &DictionaryEntry{Code: e.Code, Word: e.Word, Weight: e.Weight})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			original = *change.Replacement
		}
	}
	if _, err := s.EditDictionary(ctx, other, "pinyin", "", 0, &DictionaryEntry{Code: "shared", Word: "共享", Weight: 99}); err != nil {
		t.Fatal(err)
	}
	pack := []SharedWord{{"pinyin", "shared", "共享", 20}, seed[1]}
	for i := 0; i < 126; i++ {
		pack = append(pack, SharedWord{"quick", fmt.Sprintf("code-%03d", i), fmt.Sprintf("合成词-%03d", i), int64(i)})
	}
	apply := func(expected int64, changed int, queries int) int64 {
		t.Helper()
		tx, err := s.userDataTransaction(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		counted := &countedDictionaryTx{Tx: tx}
		revision, imported, err := mergeResourceDictionary(ctx, counted, user, expected, pack)
		if err != nil || imported != changed || revision != expected+int64(changed) {
			t.Fatal(revision, imported, err)
		}
		if counted.queries != queries {
			t.Fatalf("词条数 %d：数据库查询 %d 次，期望 %d 次", len(pack), counted.queries, queries)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return revision
	}
	revision := apply(3, 127, 6)
	changes, more, err := s.DictionaryChanges(ctx, user, 3, 200)
	if err != nil || more || len(changes) != 127 {
		t.Fatal(len(changes), more, err)
	}
	ids := map[string]bool{}
	for i, c := range changes {
		if c.Revision != int64(i+4) || c.Replacement == nil || c.Replacement.Revision != c.Revision || c.Replacement.ID == "" || ids[c.Replacement.ID] {
			t.Fatal("独立词条和连续版本", c)
		}
		ids[c.Replacement.ID] = true
		if i == 0 {
			if c.Previous == nil || c.Previous.ID != original.ID || c.Previous.Revision != original.Revision || c.Previous.Weight != original.Weight || !c.Previous.UpdatedAt.Equal(original.UpdatedAt) || c.Replacement.ID != original.ID || c.Replacement.Weight != 20 {
				t.Fatal("更新前后状态", c, original)
			}
		} else if c.Previous != nil {
			t.Fatal("新增词条带有旧状态", c)
		}
	}
	var entries, overlays, mismatches int
	if err = s.pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1),
(SELECT count(*) FROM user_dictionary_overlay WHERE user_id=$1),
(SELECT count(*) FROM user_dictionary_entries e JOIN user_dictionary_overlay o USING(user_id,kind,code,word)
 WHERE e.user_id=$1 AND (o.deleted OR o.entry->>'id'<>e.id OR (o.entry->>'revision')::bigint<>e.revision
 OR (o.entry->>'weight')::bigint<>e.weight OR (o.entry->>'updated_at')::timestamptz<>e.updated_at))`, user).Scan(&entries, &overlays, &mismatches); err != nil || entries != 129 || overlays != 129 || mismatches != 0 {
		t.Fatal(entries, overlays, mismatches, err)
	}
	otherEntries, _, err := s.DictionaryEntries(ctx, other, "pinyin", "", 0, 20)
	if err != nil || len(otherEntries) != 1 || otherEntries[0].Weight != 99 {
		t.Fatal("其他用户被改动", otherEntries, err)
	}
	apply(revision, 0, 1)
	pack = pack[:1]
	pack[0].Weight++
	apply(revision, 1, 6)
}

func TestMergeResourceDictionaryCapacity(t *testing.T) {
	s := testStore(t)
	user := complete(t, s, Identity{"email", "capacity@example.test"}).User.ID
	ctx := t.Context()
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision)
SELECT $1||'-'||n,$1,'quick','code-'||n,'合成词-'||n,1,0 FROM generate_series(1,99999) n`, user); err != nil {
		t.Fatal(err)
	}
	apply := func(pack []SharedWord, wanted error) {
		t.Helper()
		tx, err := s.userDataTransaction(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var current int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)`, user).Scan(&current); err != nil {
			t.Fatal(err)
		}
		_, _, err = mergeResourceDictionary(ctx, tx, user, current, pack)
		if !errors.Is(err, wanted) {
			t.Fatalf("导入错误 %v，期望 %v", err, wanted)
		}
		if err == nil {
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	update := SharedWord{"quick", "code-1", "合成词-1", 5}
	newWord := SharedWord{"quick", "new", "新词", 3}
	apply([]SharedWord{update, newWord, {"quick", "overflow", "超限词", 4}}, errDictionaryLimit)
	var weight int64
	if err := s.pool.QueryRow(ctx, `SELECT weight FROM user_dictionary_entries WHERE user_id=$1 AND code='code-1'`, user).Scan(&weight); err != nil || weight != 1 {
		t.Fatal("超限导入修改了已有词条", weight, err)
	}
	apply([]SharedWord{update, newWord}, nil)
	update.Weight++
	apply([]SharedWord{update}, nil)
	apply([]SharedWord{{"quick", "overflow", "超限词", 4}}, errDictionaryLimit)
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1`, user).Scan(&count); err != nil || count != 100000 {
		t.Fatal(count, err)
	}
}

func TestMergeResourceDictionaryRevisionOverflow(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	user := complete(t, s, Identity{"email", "revision@example.test"}).User.ID
	word := SharedWord{"quick", "old", "原词", 1}
	if _, err := s.EditDictionary(ctx, user, word.Kind, "", 0, &DictionaryEntry{Code: word.Code, Word: word.Word, Weight: word.Weight}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE user_dictionary_state SET revision=$2 WHERE user_id=$1`, user, int64(math.MaxInt64-1)); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var raw string
		if err := s.pool.QueryRow(ctx, `SELECT jsonb_build_array(
(SELECT jsonb_agg(e ORDER BY id) FROM user_dictionary_entries e WHERE user_id=$1),
(SELECT jsonb_agg(o ORDER BY kind,code,word) FROM user_dictionary_overlay o WHERE user_id=$1),
(SELECT jsonb_agg(c ORDER BY revision) FROM user_dictionary_changes c WHERE user_id=$1),
(SELECT revision FROM user_dictionary_state WHERE user_id=$1))::text`, user).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	apply := func(current int64, pack []SharedWord, fail bool) {
		t.Helper()
		before := snapshot()
		tx, err := s.userDataTransaction(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, _, err = mergeResourceDictionary(ctx, tx, user, current, pack)
		if fail {
			if err == nil {
				t.Fatal("版本溢出仍然成功")
			}
			if err = tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if after := snapshot(); after != before {
				t.Fatal("版本溢出修改了词库、覆盖、日志或版本", after)
			}
		} else if err != nil {
			t.Fatal(err)
		} else if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	word.Weight++
	apply(math.MaxInt64-1, []SharedWord{word, {"quick", "new", "新词", 1}}, true)
	apply(math.MaxInt64-1, []SharedWord{word}, false)
	apply(math.MaxInt64, []SharedWord{word}, false)
	word.Weight++
	apply(math.MaxInt64, []SharedWord{word}, true)
}
