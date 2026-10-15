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
	weights := make([]int64, len(entries))
	for i, e := range entries {
		kinds[i], codes[i], words[i] = e.Kind, e.Code, e.Word
		weights[i] = e.Weight
	}
	type key struct{ kind, code, word string }
	existing := make(map[key]*DictionaryEntry, len(entries))
	var count int
	var updatedAt time.Time
	// 匹配词条与汇总共用一次请求；汇总只返回一行，且无新增或版本溢出时跳过容量扫描。
	rows, err := tx.Query(ctx, `WITH wanted AS MATERIALIZED (
 SELECT * FROM unnest($2::text[],$3::text[],$4::text[],$5::bigint[]) AS w(kind,code,word,weight)
), matched AS MATERIALIZED (
 SELECT e.id,e.kind,e.code,e.word,e.weight,e.revision,e.updated_at
FROM user_dictionary_entries e
JOIN wanted USING(kind,code,word)
WHERE e.user_id=$1
), summary AS (
 SELECT count(*) FILTER (WHERE e.id IS NULL) AS added,
 count(*) FILTER (WHERE e.id IS NULL OR e.weight<>w.weight) AS changed
 FROM wanted w LEFT JOIN matched e USING(kind,code,word)
)
SELECT false,id,kind,code,word,weight,revision,updated_at,0::bigint FROM matched
UNION ALL
SELECT true,'','','','',0,0,now(),
 CASE WHEN added>0 AND $6::bigint<=9223372036854775807::bigint-changed
 THEN (SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1) ELSE 0 END
FROM summary`, user, kinds, codes, words, weights, revision)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var summary bool
		var total int
		var e DictionaryEntry
		if err := rows.Scan(&summary, &e.ID, &e.Kind, &e.Code, &e.Word, &e.Weight, &e.Revision, &e.UpdatedAt, &total); err != nil {
			return 0, 0, err
		}
		if summary {
			count, updatedAt = total, e.UpdatedAt
		} else {
			existing[key{e.Kind, e.Code, e.Word}] = &e
		}
	}
	if err = rows.Err(); err != nil {
		return 0, 0, err
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
	if newEntries > 0 && count+newEntries > 100000 {
		return 0, 0, errDictionaryLimit
	}
	for i := range changes {
		changes[i].Replacement.UpdatedAt = updatedAt
	}
	if err = writeDictionaryChanges(ctx, tx, user, changes); err != nil {
		return 0, 0, err
	}
	return revision + int64(len(changes)), len(changes), nil
}
