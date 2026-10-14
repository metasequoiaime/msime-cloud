package account

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// importDictionaryBatch 在用户事务锁内导入已规范化的词条。重复词条让整批失败，
// 与社区词包的覆盖合并不同；容量、时间戳和当前版本只读取一次。
func importDictionaryBatch(ctx context.Context, tx pgx.Tx, user, kind string, entries []DictionaryEntry) (int64, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	codes, words := make([]string, len(entries)), make([]string, len(entries))
	type key struct{ code, word string }
	seen := make(map[key]bool, len(entries))
	for i, e := range entries {
		k := key{e.Code, e.Word}
		if seen[k] {
			return 0, errDictionaryDuplicate
		}
		seen[k] = true
		codes[i], words[i] = e.Code, e.Word
	}
	var duplicate bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM user_dictionary_entries e
JOIN unnest($3::text[],$4::text[]) AS wanted(code,word) ON wanted.code=e.code AND wanted.word=e.word
WHERE e.user_id=$1 AND e.kind=$2)`, user, kind, codes, words).Scan(&duplicate); err != nil {
		return 0, err
	}
	if duplicate {
		return 0, errDictionaryDuplicate
	}
	var count int
	var revision int64
	var updatedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT count(*),COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0),now()
FROM user_dictionary_entries WHERE user_id=$1`, user).Scan(&count, &revision, &updatedAt); err != nil {
		return 0, err
	}
	if count+len(entries) > 100000 {
		return 0, errDictionaryLimit
	}
	if revision > math.MaxInt64-int64(len(entries)) {
		return 0, errors.New("dictionary revision overflow")
	}
	changes := make([]DictionaryChange, len(entries))
	for i, e := range entries {
		e.ID, e.Kind, e.Revision, e.UpdatedAt = randomToken(), kind, revision+int64(i)+1, updatedAt
		changes[i] = DictionaryChange{Revision: e.Revision, Replacement: &e}
	}
	if err := writeDictionaryChanges(ctx, tx, user, changes); err != nil {
		return 0, err
	}
	return revision + int64(len(entries)), nil
}

// writeDictionaryChanges 批量写入新词或相同编码、正文的权重更新，调用方持有用户锁。
func writeDictionaryChanges(ctx context.Context, tx pgx.Tx, user string, changes []DictionaryChange) error {
	raw, err := json.Marshal(changes)
	if err != nil {
		return err
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
			return err
		}
	}
	return nil
}
