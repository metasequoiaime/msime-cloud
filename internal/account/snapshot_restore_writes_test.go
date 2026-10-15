package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func restoreWriteSnapshot() []byte {
	return signedSnapshot(
		`{"type":"header","format":"msime-dictionary-snapshot","version":1,"revision":4}`,
		`{"type":"entry","data":{"id":"source-entry","kind":"wubi98","code":"abcd","word":"恢复新词","weight":10,"revision":1,"updated_at":"2026-09-08T00:00:00Z"}}`,
		`{"type":"overlay","deleted":false,"data":{"id":"source-entry","kind":"wubi98","code":"abcd","word":"恢复新词","weight":10,"revision":1,"updated_at":"2026-09-08T00:00:00Z"}}`,
		`{"type":"overlay","deleted":true,"data":{"id":"","kind":"pinyin","code":"shan","word":"删","weight":0,"revision":2,"updated_at":"2026-09-08T00:00:00Z","user_inserted":false}}`,
		`{"type":"overlay","deleted":false,"data":{"id":"","kind":"pinyin","code":"ji","word":"基","weight":20,"revision":3,"updated_at":"2026-09-08T00:00:00Z","user_inserted":false}}`,
		`{"type":"position","data":{"context":"abcd","code":"abcd","word":"恢复新词","position":2}}`,
		`{"type":"selection","data":{"context":"abcd","code":"abcd","word":"恢复新词","count":3}}`,
	)
}

func fullSnapshotForRestoreTest(t *testing.T, db *Store, user string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := db.streamFullDictionarySnapshotWithFooter(t.Context(), user, func(raw json.RawMessage) error {
		_, err := out.Write(append(raw, '\n'))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRestoreDictionarySnapshotWritesDependentStateInOrder(t *testing.T) {
	cfg := fakeRestoreEngine(t)
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "restore-writes@example.test"})
	other := complete(t, db, Identity{"email", "restore-unrelated@example.test"})
	for _, uid := range []string{user.User.ID, other.User.ID} {
		if _, err := db.EditDictionary(ctx, uid, "quick", "", 0, &DictionaryEntry{Code: "old", Word: "旧词", Weight: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(ctx, `INSERT INTO user_candidate_positions(user_id,context,code,word,position) VALUES($1,'old','old','旧词',1);
INSERT INTO user_candidate_selections(user_id,context,code,word,count) VALUES($1,'old','old','旧词',1)`, pgx.QueryExecModeSimpleProtocol, uid); err != nil {
			t.Fatal(err)
		}
	}
	beforeOther := fullSnapshotForRestoreTest(t, db, other.User.ID)
	revision, err := db.RestoreDictionarySnapshot(ctx, user.User.ID, 1, bytes.NewReader(restoreWriteSnapshot()), cfg)
	if err != nil || revision != 9 {
		t.Fatalf("恢复完整快照失败，revision=%d：%v", revision, err)
	}
	entries, _, err := db.DictionaryEntries(ctx, user.User.ID, "wubi98", "", 0, 10)
	if err != nil || len(entries) != 1 || entries[0].ID == "source-entry" || entries[0].Word != "恢复新词" || entries[0].Revision != 5 {
		t.Fatalf("恢复后的词条或新 ID、版本不正确：%+v，%v", entries, err)
	}
	var overlay DictionaryEntry
	if err := db.pool.QueryRow(ctx, `SELECT entry FROM user_dictionary_overlay WHERE user_id=$1 AND kind='wubi98'`, user.User.ID).Scan(&overlay); err != nil {
		t.Fatal(err)
	}
	if overlay.ID != entries[0].ID || overlay.Revision != entries[0].Revision || !overlay.UpdatedAt.Equal(entries[0].UpdatedAt) {
		t.Fatalf("覆盖记录未使用刚写入词条的 ID、版本和时间：%+v，%+v", overlay, entries[0])
	}
	positions, _, err := db.CandidatePositions(ctx, user.User.ID, "", 0, 10)
	if err != nil || len(positions) != 1 || positions[0].Context != "abcd" || positions[0].Position != 2 {
		t.Fatalf("候选位置未被替换：%+v，%v", positions, err)
	}
	changes, _, err := db.DictionaryChanges(ctx, user.User.ID, 1, 20)
	if err != nil || len(changes) != 6 || !changes[0].Reset || changes[0].Revision != 2 {
		t.Fatalf("恢复变更或重置边界不正确：%+v，%v", changes, err)
	}
	if changes[1].Replacement == nil || changes[1].Replacement.ID != entries[0].ID || changes[2].Previous == nil || changes[2].Replacement != nil || changes[3].Replacement == nil || changes[3].Replacement.ID != "" || changes[4].Position == nil || changes[5].Selection == nil || changes[5].Selection.Count != 3 {
		t.Fatalf("恢复变更未保留新 ID、删除、基础排名、位置或选择次数：%+v", changes)
	}
	after := fullSnapshotForRestoreTest(t, db, user.User.ID)
	if strings.Contains(string(after), "旧词") || !strings.Contains(string(after), `"count": 3`) {
		t.Fatalf("恢复后仍有旧词或缺少选择次数：%s", after)
	}
	if !bytes.Equal(beforeOther, fullSnapshotForRestoreTest(t, db, other.User.ID)) {
		t.Fatal("恢复修改了其他用户的数据")
	}
}

func TestRestoreDictionarySnapshotWriteFailureRollsBack(t *testing.T) {
	cfg := fakeRestoreEngine(t)
	db := testStore(t)
	for _, table := range []string{"user_candidate_positions", "user_dictionary_state"} {
		t.Run(table, func(t *testing.T) {
			ctx := t.Context()
			user := complete(t, db, Identity{"email", table + "@example.test"})
			if _, err := db.EditDictionary(ctx, user.User.ID, "quick", "", 0, &DictionaryEntry{Code: "old", Word: "旧词", Weight: 1}); err != nil {
				t.Fatal(err)
			}
			before := fullSnapshotForRestoreTest(t, db, user.User.ID)
			beforeChanges, _, err := db.DictionaryChanges(ctx, user.User.ID, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			quoted := pgx.Identifier{table}.Sanitize()
			if _, err := db.pool.Exec(ctx, `CREATE FUNCTION reject_restore_write() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''restore write failure''; END';
CREATE TRIGGER reject_restore_write BEFORE INSERT OR UPDATE ON `+quoted+` FOR EACH ROW EXECUTE FUNCTION reject_restore_write()`); err != nil {
				t.Fatal(err)
			}
			_, restoreErr := db.RestoreDictionarySnapshot(ctx, user.User.ID, 1, bytes.NewReader(restoreWriteSnapshot()), cfg)
			if _, err := db.pool.Exec(ctx, `DROP TRIGGER reject_restore_write ON `+quoted+`; DROP FUNCTION reject_restore_write()`); err != nil {
				t.Fatal(err)
			}
			var pgerr *pgconn.PgError
			if !errors.As(restoreErr, &pgerr) || pgerr.Code != "P0001" {
				t.Fatal("未返回注入的数据库写入错误", restoreErr)
			}
			afterChanges, _, err := db.DictionaryChanges(ctx, user.User.ID, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			beforeJSON, _ := json.Marshal(beforeChanges)
			afterJSON, _ := json.Marshal(afterChanges)
			if !bytes.Equal(before, fullSnapshotForRestoreTest(t, db, user.User.ID)) || !bytes.Equal(beforeJSON, afterJSON) {
				t.Fatal("中途失败后词库、覆盖层、候选状态、变更或版本未完整回滚")
			}
			if _, err := db.RestoreDictionarySnapshot(ctx, user.User.ID, 1, bytes.NewReader(restoreWriteSnapshot()), cfg); err != nil {
				t.Fatal("回滚后无法重新恢复快照", err)
			}
		})
	}
}
