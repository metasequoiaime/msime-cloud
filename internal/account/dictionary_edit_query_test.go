package account

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDictionaryEditsUseOneMutationQuery(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	uid := complete(t, db, Identity{"email", "edit-query@example.test"}).User.ID
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}
	var current DictionaryChange
	for _, action := range []string{"create", "replace", "same_key", "delete"} {
		t.Run(action, func(t *testing.T) {
			entry := &DictionaryEntry{Code: "first", Word: "原词", Weight: 1}
			id, expected := "", int64(0)
			if action != "create" {
				id, expected = current.Replacement.ID, current.Replacement.Revision
				entry.Word, entry.Weight = "替换词", 2
			}
			if action == "delete" {
				entry = nil
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			change, err := store.EditDictionary(ctx, uid, "quick", id, expected, entry)
			if err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			calls := trace.seen
			trace.mu.Unlock()
			if calls != 5 {
				t.Errorf("词库 %s 用了 %d 次数据库调用，期望 5 次", action, calls)
			}
			if change.Revision != expected+1 || (action == "create") != (change.Previous == nil) || (action == "delete") != (change.Replacement == nil) {
				t.Fatalf("词库前后状态或版本不正确：%+v", change)
			}
			if change.Replacement != nil && (change.Replacement.UpdatedAt.IsZero() || (id != "" && change.Replacement.ID != id)) {
				t.Fatalf("替换状态缺少数据库时间或未保留原 ID：%+v", change.Replacement)
			}
			current = change
		})
	}
	changes, _, err := db.DictionaryChanges(ctx, uid, 0, 20)
	if err != nil || len(changes) != 4 || changes[3].Previous == nil || changes[3].Previous.Word != "替换词" || changes[3].Replacement != nil {
		t.Fatal("变更流未保留完整编辑和删除状态", changes, err)
	}
}

func TestDictionaryEditRollsBackEveryTableFailure(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	uid := complete(t, db, Identity{"email", "edit-rollback@example.test"}).User.ID
	created, err := db.EditDictionary(ctx, uid, "quick", "", 0, &DictionaryEntry{Code: "old", Word: "旧词", Weight: 1})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var raw string
		if err := db.pool.QueryRow(ctx, `SELECT jsonb_build_array(
(SELECT jsonb_agg(e ORDER BY id) FROM user_dictionary_entries e WHERE user_id=$1),
(SELECT jsonb_agg(o ORDER BY kind,code,word) FROM user_dictionary_overlay o WHERE user_id=$1),
(SELECT jsonb_agg(c ORDER BY revision) FROM user_dictionary_changes c WHERE user_id=$1),
(SELECT revision FROM user_dictionary_state WHERE user_id=$1))::text`, uid).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	for _, tc := range []struct {
		table    string
		suppress bool
	}{
		{"user_dictionary_state", false},
		{"user_dictionary_entries", false},
		{"user_dictionary_overlay", false},
		{"user_dictionary_changes", false},
		{"user_dictionary_state", true},
		{"user_dictionary_entries", true},
	} {
		t.Run(tc.table, func(t *testing.T) {
			tx, err := db.userDataTransaction(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			query := `ALTER TABLE ` + pgx.Identifier{tc.table}.Sanitize() + ` ADD CONSTRAINT dictionary_edit_failure CHECK (false) NOT VALID`
			if tc.suppress {
				query = `CREATE FUNCTION skip_dictionary_edit() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END';
CREATE TRIGGER skip_dictionary_edit BEFORE INSERT OR UPDATE ON ` + pgx.Identifier{tc.table}.Sanitize() + ` FOR EACH ROW EXECUTE FUNCTION skip_dictionary_edit()`
			}
			if _, err := tx.Exec(ctx, query); err != nil {
				t.Fatal(err)
			}
			_, err = db.dictionaryEdit(ctx, tx, uid, "quick", created.Replacement.ID, 1, &DictionaryEntry{Code: "new", Word: "新词", Weight: 2})
			var pgerr *pgconn.PgError
			if tc.suppress && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("数据库没有返回写入结果时仍然成功", err)
			} else if !tc.suppress && (!errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != "dictionary_edit_failure") {
				t.Fatal("未返回指定表的约束错误", err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if after := snapshot(); after != before {
				t.Fatal("编辑失败后词条、覆盖层、变更记录或版本未完整回滚", after)
			}
		})
	}
}

func TestDictionaryEditKeepsValidationPriorityAndCapacity(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	uid := complete(t, db, Identity{"email", "edit-capacity@example.test"}).User.ID
	created, err := db.EditDictionary(ctx, uid, "quick", "", 0, &DictionaryEntry{Code: "kept", Word: "保留词", Weight: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision)
SELECT $1||'-'||n,$1,'pinyin','code-'||n,'合成词-'||n,1,0 FROM generate_series(1,99999) n`, uid); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, kind, id string
		expected       int64
		next           DictionaryEntry
		want           error
	}{
		{"missing", "quick", "missing", 1, DictionaryEntry{Code: "kept", Word: "保留词", Weight: 1}, errDataNotFound},
		{"wrong_kind", "english", created.Replacement.ID, 1, DictionaryEntry{Code: "kept", Word: "保留词", Weight: 1}, errDataNotFound},
		{"revision", "quick", created.Replacement.ID, 0, DictionaryEntry{Code: "kept", Word: "保留词", Weight: 1}, errRevisionConflict},
		{"revision_before_duplicate", "pinyin", uid + "-1", -1, DictionaryEntry{Code: "code-2", Word: "合成词-2", Weight: 1}, errRevisionConflict},
		{"update_duplicate", "pinyin", uid + "-1", 0, DictionaryEntry{Code: "code-2", Word: "合成词-2", Weight: 1}, errDictionaryDuplicate},
		{"duplicate", "quick", "", 0, DictionaryEntry{Code: "kept", Word: "保留词", Weight: 1}, errDictionaryDuplicate},
		{"capacity_across_kinds", "english", "", 0, DictionaryEntry{Code: "fresh", Word: "新词", Weight: 1}, errDictionaryLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.EditDictionary(ctx, uid, tc.kind, tc.id, tc.expected, &tc.next); !errors.Is(err, tc.want) {
				t.Fatalf("验证错误=%v，期望 %v", err, tc.want)
			}
		})
	}
	changes, _, err := db.DictionaryChanges(ctx, uid, 0, 20)
	if err != nil || len(changes) != 1 || changes[0].Revision != 1 {
		t.Fatal("失败的验证推进了版本或留下变更", changes, err)
	}
	replaced, err := db.EditDictionary(ctx, uid, "quick", created.Replacement.ID, 1, &DictionaryEntry{Code: "kept", Word: "保留词", Weight: 2})
	if err != nil || replaced.Revision != 2 {
		t.Fatal("容量已满时无法编辑已有词条", replaced, err)
	}
	if _, err := db.EditDictionary(ctx, uid, "quick", created.Replacement.ID, 2, nil); err != nil {
		t.Fatal("容量已满时无法删除词条", err)
	}
	added, err := db.EditDictionary(ctx, uid, "quick", "", 0, &DictionaryEntry{Code: "freed", Word: "腾出位置", Weight: 1})
	if err != nil || added.Revision != 4 {
		t.Fatal("删除后无法用释放的容量创建词条", added, err)
	}
	edited, err := db.EditDictionary(ctx, uid, "pinyin", uid+"-1", 0, &DictionaryEntry{Code: "code-1", Word: "合成词-1", Weight: 2})
	if err != nil || edited.Revision != 5 || edited.Previous == nil || edited.Previous.Revision != 0 {
		t.Fatal("校验应使用词条版本，新版本应从用户当前全局版本递增", edited, err)
	}
	other := complete(t, db, Identity{"email", "edit-isolated@example.test"}).User.ID
	if _, err := db.EditDictionary(ctx, other, "quick", "", 0, &DictionaryEntry{Code: "kept", Word: "保留词", Weight: 1}); err != nil {
		t.Fatal("另一账号的容量或查重影响了当前账号", err)
	}
}
