package account

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// mergeResourceDictionary 批量合并已经校验、去重的词包。调用方必须持有
// userDataTransaction 的用户锁并核对词库版本，使预加载与写入期间没有并发编辑。
func mergeResourceDictionary(ctx context.Context, tx pgx.Tx, user string, revision int64, entries []SharedWord) (int64, int, error) {
	kinds, codes, words := make([]string, len(entries)), make([]string, len(entries)), make([]string, len(entries))
	for i, e := range entries {
		kinds[i], codes[i], words[i] = e.Kind, e.Code, e.Word
	}
	type key struct{ kind, code, word string }
	existing := make(map[key]*DictionaryEntry, len(entries))
	rows, err := tx.Query(ctx, `SELECT e.id,e.kind,e.code,e.word,e.weight,e.revision,e.updated_at
FROM user_dictionary_entries e
JOIN unnest($2::text[],$3::text[],$4::text[]) AS wanted(kind,code,word)
 ON wanted.kind=e.kind AND wanted.code=e.code AND wanted.word=e.word
WHERE e.user_id=$1`, user, kinds, codes, words)
	if err != nil {
		return 0, 0, err
	}
	previous, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*DictionaryEntry, error) {
		e := new(DictionaryEntry)
		err := row.Scan(&e.ID, &e.Kind, &e.Code, &e.Word, &e.Weight, &e.Revision, &e.UpdatedAt)
		return e, err
	})
	if err != nil {
		return 0, 0, err
	}
	for _, e := range previous {
		existing[key{e.Kind, e.Code, e.Word}] = e
	}
	changes := make([]DictionaryChange, 0, len(entries))
	for _, e := range entries {
		old := existing[key{e.Kind, e.Code, e.Word}]
		if old != nil && old.Weight == e.Weight {
			continue
		}
		if revision > math.MaxInt64-int64(len(changes))-1 {
			return 0, 0, errors.New("dictionary revision overflow")
		}
		next := &DictionaryEntry{Kind: e.Kind, Code: e.Code, Word: e.Word, Weight: e.Weight, Revision: revision + int64(len(changes)) + 1}
		if old != nil {
			next.ID = old.ID
		} else {
			next.ID = randomToken()
		}
		changes = append(changes, DictionaryChange{Revision: next.Revision, Previous: old, Replacement: next})
	}
	if len(changes) == 0 {
		return revision, 0, nil
	}
	newEntries := len(entries) - len(existing)
	var updatedAt time.Time
	if newEntries > 0 {
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*),now() FROM user_dictionary_entries WHERE user_id=$1`, user).Scan(&count, &updatedAt); err != nil {
			return 0, 0, err
		}
		if count+newEntries > 100000 {
			return 0, 0, errDictionaryLimit
		}
	} else if err = tx.QueryRow(ctx, `SELECT now()`).Scan(&updatedAt); err != nil {
		return 0, 0, err
	}
	for i := range changes {
		changes[i].Replacement.UpdatedAt = updatedAt
	}
	raw, err := json.Marshal(changes)
	if err != nil {
		return 0, 0, err
	}
	// 每条变化保留独立版本和完整前后状态；四次批量写入与单条编辑生成相同的同步记录。
	queries := []string{
		`INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision,updated_at)
SELECT e->>'id',$1,e->>'kind',e->>'code',e->>'word',(e->>'weight')::bigint,(e->>'revision')::bigint,(e->>'updated_at')::timestamptz
FROM jsonb_array_elements($2::jsonb) c CROSS JOIN LATERAL (SELECT c->'replacement' AS e) r
ON CONFLICT(id) DO UPDATE SET weight=excluded.weight,revision=excluded.revision,updated_at=excluded.updated_at`,
		`INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted)
SELECT $1,e->>'kind',e->>'code',e->>'word',e,false
FROM jsonb_array_elements($2::jsonb) c CROSS JOIN LATERAL (SELECT c->'replacement' AS e) r
ON CONFLICT(user_id,kind,code,word) DO UPDATE SET entry=excluded.entry,deleted=false`,
		`INSERT INTO user_dictionary_changes(user_id,revision,change)
SELECT $1,(c->>'revision')::bigint,c FROM jsonb_array_elements($2::jsonb) c`,
		`INSERT INTO user_dictionary_state(user_id,revision)
SELECT $1,max((c->>'revision')::bigint) FROM jsonb_array_elements($2::jsonb) c
ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision`,
	}
	for _, query := range queries {
		if _, err = tx.Exec(ctx, query, user, raw); err != nil {
			return 0, 0, err
		}
	}
	return revision + int64(len(changes)), len(changes), nil
}
