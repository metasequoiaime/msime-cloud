package account

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestWriteDictionaryChangesRollsBackEveryTableFailure(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	user := complete(t, s, Identity{"email", "write-errors@example.test"}).User.ID
	change, err := s.EditDictionary(ctx, user, "quick", "", 0, &DictionaryEntry{Code: "old", Word: "原词", Weight: 1})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var raw string
		err := s.pool.QueryRow(ctx, `SELECT jsonb_build_array(
(SELECT jsonb_agg(e ORDER BY id) FROM user_dictionary_entries e WHERE user_id=$1),
(SELECT jsonb_agg(o ORDER BY kind,code,word) FROM user_dictionary_overlay o WHERE user_id=$1),
(SELECT jsonb_agg(c ORDER BY revision) FROM user_dictionary_changes c WHERE user_id=$1),
(SELECT revision FROM user_dictionary_state WHERE user_id=$1))::text`, user).Scan(&raw)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	for _, table := range []string{"user_dictionary_entries", "user_dictionary_overlay", "user_dictionary_changes", "user_dictionary_state"} {
		t.Run(table, func(t *testing.T) {
			tx, err := s.userDataTransaction(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			// 约束只存在于本次事务，既有数据不扫描；回滚也会撤销测试约束。
			if _, err = tx.Exec(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT dictionary_write_test_failure CHECK (false) NOT VALID"); err != nil {
				t.Fatal(err)
			}
			next := *change.Replacement
			next.Weight, next.Revision = 2, 2
			fresh := DictionaryEntry{ID: randomToken(), Kind: "quick", Code: "new", Word: "新词", Weight: 3, Revision: 3, UpdatedAt: next.UpdatedAt}
			err = writeDictionaryChanges(ctx, tx, user, []DictionaryChange{
				{Revision: 2, Previous: change.Replacement, Replacement: &next},
				{Revision: 3, Replacement: &fresh},
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "dictionary_write_test_failure" {
				t.Fatalf("未返回目标表约束错误：%v", err)
			}
			if err = tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if after := snapshot(); after != before {
				t.Fatal("失败写入修改了词条、覆盖、日志或版本", after)
			}
		})
	}
}

func BenchmarkWriteDictionaryChanges(b *testing.B) {
	dsn := os.Getenv("MSIME_TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("设置 MSIME_TEST_DATABASE_URL 运行 PostgreSQL 基准")
	}
	if !strings.Contains(dsn, "msime_auth_test") {
		b.Fatal("仅允许使用一次性测试数据库")
	}
	s, err := Open(b.Context(), dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(b.Context()); err != nil {
		b.Fatal(err)
	}
	for _, count := range []int{1, 500, 5000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			ctx := b.Context()
			// 所有数据在单一事务内回滚，每次写入后回到相同的空词库。
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				b.Fatal(err)
			}
			defer tx.Rollback(ctx)
			user := randomToken()
			if _, err = tx.Exec(ctx, `INSERT INTO auth_users(id) VALUES($1)`, user); err != nil {
				b.Fatal(err)
			}
			changes := make([]DictionaryChange, count)
			now := time.Now().UTC()
			for i := range changes {
				e := &DictionaryEntry{ID: randomToken(), Kind: "quick", Code: fmt.Sprintf("code-%d", i), Word: fmt.Sprintf("合成词-%d", i), Weight: int64(i), Revision: int64(i + 1), UpdatedAt: now}
				changes[i] = DictionaryChange{Revision: e.Revision, Replacement: e}
			}
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				if _, err := tx.Exec(ctx, "SAVEPOINT dictionary_write_benchmark"); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := writeDictionaryChanges(ctx, tx, user, changes); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT dictionary_write_benchmark"); err != nil {
					b.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT dictionary_write_benchmark"); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}
