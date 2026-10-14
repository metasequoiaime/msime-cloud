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
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
func (s *Store) dictionaryEdit(ctx context.Context, tx pgx.Tx, user, kind, id string, expected int64, next *DictionaryEntry) (DictionaryChange, error) {
	var previous *DictionaryEntry
	if id != "" {
		previous = &DictionaryEntry{}
		err := tx.QueryRow(ctx, `SELECT id,kind,code,word,weight,revision,updated_at FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 AND id=$3`, user, kind, id).Scan(&previous.ID, &previous.Kind, &previous.Code, &previous.Word, &previous.Weight, &previous.Revision, &previous.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return DictionaryChange{}, errDataNotFound
		}
		if err != nil {
			return DictionaryChange{}, err
		}
		if previous.Revision != expected {
			return DictionaryChange{}, errRevisionConflict
		}
	}
	if next != nil {
		var duplicate bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 AND code=$3 AND word=$4 AND id<>$5)`, user, kind, next.Code, next.Word, id).Scan(&duplicate); err != nil {
			return DictionaryChange{}, err
		}
		if duplicate {
			return DictionaryChange{}, errDictionaryDuplicate
		}
		if id == "" {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1`, user).Scan(&count); err != nil {
				return DictionaryChange{}, err
			}
			if count >= 100000 {
				return DictionaryChange{}, errDictionaryLimit
			}
			id = randomToken()
		}
	}
	var revision int64
	err := tx.QueryRow(ctx, `INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,1) ON CONFLICT(user_id) DO UPDATE SET revision=user_dictionary_state.revision+1 RETURNING revision`, user).Scan(&revision)
	if err != nil {
		return DictionaryChange{}, err
	}
	change := DictionaryChange{Revision: revision, Previous: previous}
	if next == nil {
		if _, err = tx.Exec(ctx, `DELETE FROM user_dictionary_entries WHERE user_id=$1 AND id=$2`, user, id); err != nil {
			return change, err
		}
	} else {
		replacement := *next
		replacement.ID = id
		replacement.Kind = kind
		replacement.Revision = revision
		err = tx.QueryRow(ctx, `INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(id) DO UPDATE SET code=excluded.code,word=excluded.word,weight=excluded.weight,revision=excluded.revision,updated_at=now() RETURNING updated_at`, id, user, kind, replacement.Code, replacement.Word, replacement.Weight, revision).Scan(&replacement.UpdatedAt)
		if err != nil {
			return change, err
		}
		change.Replacement = &replacement
	}
	for _, state := range []struct {
		entry   *DictionaryEntry
		deleted bool
	}{{change.Previous, true}, {change.Replacement, false}} {
		if state.entry == nil {
			continue
		}
		raw, err := json.Marshal(state.entry)
		if err != nil {
			return change, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted) VALUES($1,$2,$3,$4,$5::jsonb,$6) ON CONFLICT(user_id,kind,code,word) DO UPDATE SET entry=excluded.entry,deleted=excluded.deleted`, user, state.entry.Kind, state.entry.Code, state.entry.Word, raw, state.deleted)
		if err != nil {
			return change, err
		}
	}
	raw, err := json.Marshal(change)
	if err != nil {
		return change, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO user_dictionary_changes(user_id,revision,change) VALUES($1,$2,$3::jsonb)`, user, revision, raw)
	return change, err
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
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return result, more, nil
}

// StreamDictionary exports one database snapshot without retaining the user's entire dictionary in memory.
func (s *Store) StreamDictionary(ctx context.Context, user, kind string, emit func(DictionaryEntry) error) error {
	rows, err := s.pool.Query(ctx, `SELECT id,kind,code,word,weight,revision,updated_at FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 ORDER BY code,word,id`, user, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e DictionaryEntry
		if err = rows.Scan(&e.ID, &e.Kind, &e.Code, &e.Word, &e.Weight, &e.Revision, &e.UpdatedAt); err != nil {
			return err
		}
		if err = emit(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// StreamDictionarySnapshot reads the current overlay under one SQL statement snapshot.
func (s *Store) StreamDictionarySnapshot(ctx context.Context, user string, emit func(json.RawMessage) error) error {
	return streamDictionarySnapshot(ctx, s.pool, user, emit)
}

type snapshotQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func streamDictionarySnapshot(ctx context.Context, q snapshotQuerier, user string, emit func(json.RawMessage) error) error {
	rows, err := q.Query(ctx, `SELECT record FROM (
 SELECT 0 AS category,'' AS kind,'' AS code,'' AS word,jsonb_build_object('snapshot_revision',COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)) AS record
 UNION ALL
 SELECT 1,kind,code,word,jsonb_build_object('previous',CASE WHEN deleted THEN entry ELSE 'null'::jsonb END,'replacement',CASE WHEN deleted THEN 'null'::jsonb ELSE entry END) FROM user_dictionary_overlay WHERE user_id=$1
  UNION ALL SELECT 2,'' AS kind,context AS code,code||word AS word,jsonb_build_object('fixed',jsonb_build_object('context',context,'code',code,'word',word,'position',position)) FROM user_candidate_positions WHERE user_id=$1
 UNION ALL SELECT 3,'',context,code||word,jsonb_build_object('selection',jsonb_build_object('context',context,'code',code,'word',word,'count',count)) FROM user_candidate_selections WHERE user_id=$1
 ) snapshot ORDER BY category,kind,code,word`, user)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if err = emit(raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

// StreamWindowsDictionary matches the Windows manager export selection: pinyin
// includes multi-character upserts, while other kinds export user insertions.
func (s *Store) StreamWindowsDictionary(ctx context.Context, user, kind string, emit func(DictionaryEntry) error) error {
	rows, err := s.pool.Query(ctx, `SELECT entry FROM user_dictionary_overlay WHERE user_id=$1 AND kind=$2 AND NOT deleted AND (($2='pinyin' AND char_length(word)>1) OR ($2<>'pinyin' AND COALESCE(entry->>'user_inserted','true')='true')) ORDER BY code,word`, user, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
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
	return rows.Err()
}
