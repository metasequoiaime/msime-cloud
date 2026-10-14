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
	// JSON 只传输、解析和展开一次；四张表各写自己的投影，不依赖彼此的执行顺序。
	// 每条变化仍保留独立版本与完整前后状态，任意写入失败均由外层事务回滚。
	_, err = tx.Exec(ctx, `WITH input AS MATERIALIZED (
 SELECT c AS change,c->'replacement' AS entry FROM jsonb_array_elements($2::jsonb) c
), written_entries AS (
 INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision,updated_at)
 SELECT entry->>'id',$1,entry->>'kind',entry->>'code',entry->>'word',(entry->>'weight')::bigint,(entry->>'revision')::bigint,(entry->>'updated_at')::timestamptz
 FROM input
 ON CONFLICT(id) DO UPDATE SET weight=excluded.weight,revision=excluded.revision,updated_at=excluded.updated_at
), written_overlay AS (
 INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted)
 SELECT $1,entry->>'kind',entry->>'code',entry->>'word',entry,false FROM input
 ON CONFLICT(user_id,kind,code,word) DO UPDATE SET entry=excluded.entry,deleted=false
), written_changes AS (
 INSERT INTO user_dictionary_changes(user_id,revision,change)
 SELECT $1,(change->>'revision')::bigint,change FROM input
)
INSERT INTO user_dictionary_state(user_id,revision)
SELECT $1,max((change->>'revision')::bigint) FROM input
ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision`, user, raw)
	return err
}
