package account

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestImportDictionaryBatchQueriesAndChanges(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "import-batch@example.test"}).User.ID
	if _, err := db.EditDictionary(ctx, user, "quick", "", 0, &DictionaryEntry{Code: "keep", Word: "保留", Weight: 1}); err != nil {
		t.Fatal(err)
	}
	other := complete(t, db, Identity{"email", "import-other@example.test"}).User.ID
	if _, err := db.EditDictionary(ctx, other, "quick", "", 0, &DictionaryEntry{Code: "code-000", Word: "合成词-000", Weight: 99}); err != nil {
		t.Fatal(err)
	}
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	store := &Store{pool: pool}
	entries := make([]DictionaryEntry, 500)
	for i := range entries {
		entries[i] = DictionaryEntry{Code: fmt.Sprintf("code-%03d", i), Word: fmt.Sprintf("合成词-%03d", i), Weight: int64(i)}
	}
	revision, err := store.ImportDictionary(ctx, user, "quick", entries)
	if err != nil || revision != 501 {
		t.Fatal(revision, err)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 6 {
		t.Fatalf("500 条导入用了 %d 次数据库调用，期望 6 次（含事务和用户锁）", queries)
	}
	t.Logf("500 条导入：%d 次数据库调用（含事务和用户锁）", queries)
	changes, more, err := db.DictionaryChanges(ctx, user, 1, 501)
	if err != nil || more || len(changes) != 500 {
		t.Fatal(len(changes), more, err)
	}
	ids := map[string]bool{}
	for i, change := range changes {
		e := change.Replacement
		if change.Previous != nil || e == nil || e.ID == "" || ids[e.ID] || change.Revision != int64(i+2) || e.Revision != change.Revision || e.Kind != "quick" || e.Code != entries[i].Code || e.Word != entries[i].Word || e.Weight != entries[i].Weight {
			t.Fatal("导入日志与输入顺序不一致", i, change)
		}
		ids[e.ID] = true
	}
	var count, invalid int
	if err = db.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE o.entry->>'id'<>e.id OR (o.entry->>'weight')::bigint<>e.weight OR (o.entry->>'revision')::bigint<>e.revision OR (o.entry->>'updated_at')::timestamptz<>e.updated_at OR o.deleted)
FROM user_dictionary_entries e JOIN user_dictionary_overlay o USING(user_id,kind,code,word) WHERE e.user_id=$1`, user).Scan(&count, &invalid); err != nil || count != 501 || invalid != 0 {
		t.Fatal(count, invalid, err)
	}
	if revision, err = store.ImportDictionary(ctx, user, "quick", nil); err != nil || revision != 0 {
		t.Fatal("空导入行为改变", revision, err)
	}
}

func TestImportDictionaryBatchRejectsDuplicatesAndRollsBack(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "import-errors@example.test"}).User.ID
	old := DictionaryEntry{Code: "old", Word: "原词", Weight: 1}
	if _, err := db.EditDictionary(ctx, user, "quick", "", 0, &old); err != nil {
		t.Fatal(err)
	}
	next := DictionaryEntry{Code: "new", Word: "新词", Weight: 2}
	for name, entries := range map[string][]DictionaryEntry{
		"词库里已有": {next, old},
		"导入内重复": {next, next},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ImportDictionary(ctx, user, "quick", entries); !errors.Is(err, errDictionaryDuplicate) {
				t.Fatal(err)
			}
		})
	}
	invalid := next
	invalid.Code, invalid.Weight = "invalid", -1
	if _, err := db.ImportDictionary(ctx, user, "quick", []DictionaryEntry{next, invalid}); err == nil {
		t.Fatal("数据库约束失败仍然成功")
	}
	entries, _, err := db.DictionaryEntries(ctx, user, "quick", "", 0, 20)
	changes, _, changeErr := db.DictionaryChanges(ctx, user, 0, 20)
	if err != nil || changeErr != nil || len(entries) != 1 || len(changes) != 1 || entries[0].Code != old.Code || changes[0].Revision != 1 {
		t.Fatal("失败导入留下部分词条或日志", entries, changes, err, changeErr)
	}
}

func TestImportDictionaryBatchCapacityAndRevision(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "import-capacity@example.test"}).User.ID
	if _, err := db.pool.Exec(ctx, `INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision)
SELECT $1||'-'||n,$1,'quick','code-'||n,'合成词-'||n,1,0 FROM generate_series(1,99999) n`, user); err != nil {
		t.Fatal(err)
	}
	pack := []DictionaryEntry{{Code: "new", Word: "新词", Weight: 1}, {Code: "overflow", Word: "超限词", Weight: 2}}
	if _, err := db.ImportDictionary(ctx, user, "quick", pack); !errors.Is(err, errDictionaryLimit) {
		t.Fatal(err)
	}
	if revision, err := db.ImportDictionary(ctx, user, "quick", pack[:1]); err != nil || revision != 1 {
		t.Fatal(revision, err)
	}
	if _, err := db.ImportDictionary(ctx, user, "quick", pack[1:]); !errors.Is(err, errDictionaryLimit) {
		t.Fatal(err)
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1`, user).Scan(&count); err != nil || count != 100000 {
		t.Fatal(count, err)
	}
	// 用另一账号核对批量版本计算；恰好到上限可以写，溢出必须整批回滚。
	other := complete(t, db, Identity{"email", "import-revision@example.test"}).User.ID
	if _, err := db.pool.Exec(ctx, `INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,$2)`, other, int64(math.MaxInt64-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportDictionary(ctx, other, "quick", pack); err == nil {
		t.Fatal("批量版本溢出仍然成功")
	}
	if revision, err := db.ImportDictionary(ctx, other, "quick", pack[:1]); err != nil || revision != math.MaxInt64 {
		t.Fatal(revision, err)
	}
	if _, err := db.ImportDictionary(ctx, other, "quick", pack[1:]); err == nil {
		t.Fatal("最大版本仍然导入新词")
	}
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1`, other).Scan(&count); err != nil || count != 1 {
		t.Fatal("溢出导入留下词条", count, err)
	}
}
