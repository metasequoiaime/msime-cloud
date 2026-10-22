package account

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type DictionaryEntry struct {
	UserInserted *bool     `json:"user_inserted,omitempty"`
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Code         string    `json:"code"`
	Word         string    `json:"word"`
	Weight       int64     `json:"weight"`
	Revision     int64     `json:"revision"`
	UpdatedAt    time.Time `json:"updated_at"`
}
type DictionaryChange struct {
	Reset       bool                `json:"reset,omitempty"`
	Ranking     []DictionaryEntry   `json:"ranking,omitempty"`
	Selection   *CandidateSelection `json:"selection,omitempty"`
	Position    *CandidatePosition  `json:"position,omitempty"`
	Revision    int64               `json:"revision"`
	Previous    *DictionaryEntry    `json:"previous"`
	Replacement *DictionaryEntry    `json:"replacement"`
}

var errDictionaryDuplicate = errors.New("dictionary_duplicate")
var errDictionaryLimit = errors.New("dictionary_limit")

func (s *Store) DictionaryEntries(ctx context.Context, user, kind, search string, offset, limit int) ([]DictionaryEntry, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,kind,code,word,weight,revision,updated_at FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 AND ($3='' OR strpos(lower(code),lower($3))>0 OR strpos(lower(word),lower($3))>0) ORDER BY code,word,id LIMIT $4 OFFSET $5`, user, kind, search, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []DictionaryEntry{}
	for rows.Next() {
		var e DictionaryEntry
		if err = rows.Scan(&e.ID, &e.Kind, &e.Code, &e.Word, &e.Weight, &e.Revision, &e.UpdatedAt); err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		exists, err := s.userExists(ctx, user)
		if err != nil {
			return nil, false, err
		}
		if !exists {
			return nil, false, errMissingUser
		}
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
func (s *Store) dictionaryEdit(ctx context.Context, tx pgx.Tx, user, kind, id string, expected int64, next *DictionaryEntry) (DictionaryChange, error) {
	entryID := id
	var code, word string
	var weight int64
	if next != nil {
		code, word, weight = next.Code, next.Word, next.Weight
		if entryID == "" {
			entryID = randomToken()
		}
	}
	var outcome string
	var previous *DictionaryEntry
	var revision *int64
	var updatedAt *time.Time
	// 调用方已经持有用户行锁；校验按原错误优先级执行，仅通过后推进版本并修改词条。
	// 前后状态通过物化读取和 RETURNING 传递，不从同一语句的旧快照重读写入结果。
	err := tx.QueryRow(ctx, `WITH previous AS MATERIALIZED (
 SELECT id,kind,code,word,weight,revision,updated_at FROM user_dictionary_entries
 WHERE user_id=$1 AND kind=$2 AND id=$3
), validation AS MATERIALIZED (
 SELECT CASE
 WHEN $3<>'' AND NOT EXISTS(SELECT 1 FROM previous) THEN 'missing'
 WHEN $3<>'' AND (SELECT revision FROM previous)<>$5 THEN 'conflict'
 WHEN NOT $6 THEN 'ok'
 WHEN EXISTS(SELECT 1 FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 AND code=$7 AND word=$8 AND id<>$3) THEN 'duplicate'
 WHEN $3='' THEN CASE WHEN (SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1)>=100000 THEN 'limit' ELSE 'ok' END
 ELSE 'ok' END AS outcome
), version AS (
 INSERT INTO user_dictionary_state(user_id,revision)
 SELECT $1,1 FROM validation WHERE outcome='ok'
 ON CONFLICT(user_id) DO UPDATE SET revision=user_dictionary_state.revision+1
 RETURNING revision
), removed AS (
 DELETE FROM user_dictionary_entries USING version
 WHERE user_id=$1 AND id=$3 AND NOT $6
), written AS (
 INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision)
 SELECT $4,$1,$2,$7,$8,$9,revision FROM version WHERE $6
 ON CONFLICT(id) DO UPDATE SET code=excluded.code,word=excluded.word,weight=excluded.weight,revision=excluded.revision,updated_at=now()
 RETURNING updated_at
)
SELECT outcome,(SELECT row_to_json(p) FROM previous p),(SELECT revision FROM version),(SELECT updated_at FROM written)
FROM validation`, user, kind, id, entryID, expected, next != nil, code, word, weight).Scan(&outcome, &previous, &revision, &updatedAt)
	if err != nil {
		return DictionaryChange{}, err
	}
	switch outcome {
	case "missing":
		return DictionaryChange{}, errDataNotFound
	case "conflict":
		return DictionaryChange{}, errRevisionConflict
	case "duplicate":
		return DictionaryChange{}, errDictionaryDuplicate
	case "limit":
		return DictionaryChange{}, errDictionaryLimit
	}
	if revision == nil {
		return DictionaryChange{}, pgx.ErrNoRows
	}
	change := DictionaryChange{Revision: *revision, Previous: previous}
	if next != nil {
		if updatedAt == nil {
			return change, pgx.ErrNoRows
		}
		replacement := *next
		replacement.ID, replacement.Kind = entryID, kind
		replacement.Revision, replacement.UpdatedAt = *revision, *updatedAt
		change.Replacement = &replacement
	}
	return change, writeDictionaryOverlayAndChange(ctx, tx, user, change)
}

// writeDictionaryOverlayAndChange 一次写入词库覆盖层和变更记录，避免替换词的前后两条覆盖层各占一次往返。
func writeDictionaryOverlayAndChange(ctx context.Context, tx pgx.Tx, user string, change DictionaryChange) error {
	query, args, err := dictionaryOverlayAndChangeQuery(user, change)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, query, args...)
	return err
}

func dictionaryOverlayAndChangeQuery(user string, change DictionaryChange) (string, []any, error) {
	type overlayRow struct {
		Kind    string          `json:"kind"`
		Code    string          `json:"code"`
		Word    string          `json:"word"`
		Entry   json.RawMessage `json:"entry"`
		Deleted bool            `json:"deleted"`
	}
	overlays := make([]overlayRow, 0, 2)
	indices := map[string]int{}
	for _, state := range []struct {
		entry   *DictionaryEntry
		deleted bool
	}{{change.Previous, true}, {change.Replacement, false}} {
		if state.entry == nil {
			continue
		}
		raw, err := json.Marshal(state.entry)
		if err != nil {
			return "", nil, err
		}
		row := overlayRow{Kind: state.entry.Kind, Code: state.entry.Code, Word: state.entry.Word, Entry: raw, Deleted: state.deleted}
		key := row.Kind + "\x00" + row.Code + "\x00" + row.Word
		if i, ok := indices[key]; ok {
			overlays[i] = row
		} else {
			indices[key] = len(overlays)
			overlays = append(overlays, row)
		}
	}
	overlayJSON, err := json.Marshal(overlays)
	if err != nil {
		return "", nil, err
	}
	changeJSON, err := json.Marshal(change)
	if err != nil {
		return "", nil, err
	}
	return `WITH overlay AS (
 INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted)
 SELECT $1,kind,code,word,entry,deleted FROM jsonb_to_recordset($2::jsonb) AS v(kind text,code text,word text,entry jsonb,deleted boolean)
 ON CONFLICT(user_id,kind,code,word) DO UPDATE SET entry=excluded.entry,deleted=excluded.deleted
 RETURNING 1
), change AS (
 INSERT INTO user_dictionary_changes(user_id,revision,change)
 SELECT $1,$3,$4::jsonb FROM (SELECT count(*) FROM overlay) dependency
 RETURNING 1
)
SELECT 1 FROM change`, []any{user, overlayJSON, change.Revision, changeJSON}, nil
}
func (s *Store) EditDictionary(ctx context.Context, user, kind, id string, expected int64, next *DictionaryEntry) (DictionaryChange, error) {
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		return DictionaryChange{}, err
	}
	defer tx.Rollback(ctx)
	change, err := s.dictionaryEdit(ctx, tx, user, kind, id, expected, next)
	if err != nil {
		return change, err
	}
	return change, tx.Commit(ctx)
}
func (s *Store) ImportDictionary(ctx context.Context, user, kind string, entries []DictionaryEntry) (int64, error) {
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	revision, err := importDictionaryBatch(ctx, tx, user, kind, entries)
	if err != nil {
		return 0, err
	}
	return revision, tx.Commit(ctx)
}
func (s *Store) DictionaryChanges(ctx context.Context, user string, after int64, limit int) ([]DictionaryChange, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT change FROM user_dictionary_changes WHERE user_id=$1 AND revision>$2 ORDER BY revision LIMIT $3`, user, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := []DictionaryChange{}
	for rows.Next() {
		var raw []byte
		var c DictionaryChange
		if err = rows.Scan(&raw); err != nil {
			return nil, false, err
		}
		if err = json.Unmarshal(raw, &c); err != nil {
			return nil, false, err
		}
		result = append(result, c)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	if len(result) == 0 {
		exists, err := s.userExists(ctx, user)
		if err != nil {
			return nil, false, err
		}
		if !exists {
			return nil, false, errMissingUser
		}
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return result, more, nil
}

// StreamDictionary exports one database snapshot without retaining the user's entire dictionary in memory.
func (s *Store) StreamDictionary(ctx context.Context, user, kind string, emit func(DictionaryEntry) error) error {
	if exists, err := s.userExists(ctx, user); err != nil {
		return err
	} else if !exists {
		return errMissingUser
	}
	rows, err := s.pool.Query(ctx, `SELECT id,kind,code,word,weight,revision,updated_at FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 ORDER BY code,word,id`, user, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	emitted := false
	for rows.Next() {
		emitted = true
		var e DictionaryEntry
		if err = rows.Scan(&e.ID, &e.Kind, &e.Code, &e.Word, &e.Weight, &e.Revision, &e.UpdatedAt); err != nil {
			return err
		}
		if err = emit(e); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !emitted {
		exists, err := s.userExists(ctx, user)
		if err != nil {
			return err
		}
		if !exists {
			return errMissingUser
		}
	}
	return nil
}

// StreamDictionarySnapshot 用一条 SQL 读取全量覆盖快照。
func (s *Store) StreamDictionarySnapshot(ctx context.Context, user string, emit func(json.RawMessage) error) error {
	return streamDictionarySnapshot(ctx, s.pool, user, emit)
}

type snapshotQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// streamDictionaryQuerySnapshot 只回放查询依赖的词库，保留全局版本与全部候选状态。
func streamDictionaryQuerySnapshot(ctx context.Context, q snapshotQuerier, user string, query map[string]any, emit func(json.RawMessage) error) error {
	kind := ""
	switch query["operation"] {
	case "dictionary":
		if value, ok := query["kind"].(string); ok && dictionaryKind(value) {
			kind = value
		}
	case "english", "quick":
		kind = query["operation"].(string)
	case "jianpin":
		kind = "pinyin"
	case "candidates":
		switch query["scheme"] {
		case nil, "pinyin", "shuangpin":
			kind = "pinyin"
		case "wubi", "wubi98":
			kind = query["scheme"].(string)
		}
	}
	// 未识别的内部查询继续提供全量快照，由引擎按原规则处理。
	return streamDictionarySnapshotKind(ctx, q, user, kind, emit)
}

func streamDictionarySnapshot(ctx context.Context, q snapshotQuerier, user string, emit func(json.RawMessage) error) error {
	return streamDictionarySnapshotKind(ctx, q, user, "", emit)
}

func streamDictionarySnapshotKind(ctx context.Context, q snapshotQuerier, user, kind string, emit func(json.RawMessage) error) error {
	filter := ""
	args := []any{user}
	if kind != "" {
		filter = " AND kind=$2"
		args = append(args, kind)
	}
	rows, err := q.Query(ctx, `SELECT record FROM (
 SELECT 0 AS category,'' AS kind,'' AS code,'' AS word,jsonb_build_object('snapshot_revision',COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)) AS record FROM auth_users WHERE id=$1
 UNION ALL
 SELECT 1,kind,code,word,jsonb_build_object('previous',CASE WHEN deleted THEN entry ELSE 'null'::jsonb END,'replacement',CASE WHEN deleted THEN 'null'::jsonb ELSE entry END) FROM user_dictionary_overlay WHERE user_id=$1`+filter+`
  UNION ALL SELECT 2,'' AS kind,context AS code,code||word AS word,jsonb_build_object('fixed',jsonb_build_object('context',context,'code',code,'word',word,'position',position)) FROM user_candidate_positions WHERE user_id=$1
 UNION ALL SELECT 3,'',context,code||word,jsonb_build_object('selection',jsonb_build_object('context',context,'code',code,'word',word,'count',count)) FROM user_candidate_selections WHERE user_id=$1
 ) snapshot ORDER BY category,kind,code,word`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := false
	for rows.Next() {
		seen = true
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if err = emit(raw); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !seen {
		return errMissingUser
	}
	return nil
}

// StreamWindowsDictionary matches the Windows manager export selection: pinyin
// includes multi-character upserts, while other kinds export user insertions.
func (s *Store) StreamWindowsDictionary(ctx context.Context, user, kind string, emit func(DictionaryEntry) error) error {
	if exists, err := s.userExists(ctx, user); err != nil {
		return err
	} else if !exists {
		return errMissingUser
	}
	rows, err := s.pool.Query(ctx, `SELECT entry FROM user_dictionary_overlay WHERE user_id=$1 AND kind=$2 AND NOT deleted AND (($2='pinyin' AND char_length(word)>1) OR ($2<>'pinyin' AND COALESCE(entry->>'user_inserted','true')='true')) ORDER BY code,word`, user, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	emitted := false
	for rows.Next() {
		emitted = true
		var raw []byte
		var entry DictionaryEntry
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &entry); err != nil {
			return err
		}
		if err = emit(entry); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !emitted {
		exists, err := s.userExists(ctx, user)
		if err != nil {
			return err
		}
		if !exists {
			return errMissingUser
		}
	}
	return nil
}
