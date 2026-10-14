package account

import (
	"context"
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
	if err = writeDictionaryChanges(ctx, tx, user, changes); err != nil {
		return 0, 0, err
	}
	return revision + int64(len(changes)), len(changes), nil
}
