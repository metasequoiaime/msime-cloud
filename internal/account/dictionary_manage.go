package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func (s *Store) EditManagedDictionary(ctx context.Context, user string, expected int64, previous DictionaryEntry, next *DictionaryEntry, cfg engine.Config) (DictionaryChange, error) {
	var change DictionaryChange
	tx, current, err := s.userDataRevisionTransaction(ctx, user, expected)
	if err != nil {
		return change, err
	}
	defer tx.Rollback(ctx)
	lookup := func(e DictionaryEntry) (*DictionaryEntry, error) {
		query := map[string]any{"operation": "dictionary", "kind": e.Kind, "text": e.Code, "word": e.Word, "exact": true, "limit": 1}
		raw, err := cfg.QuerySnapshot(ctx, map[string]any{"operation": "personal_query", "query": query}, dictionaryQuerySnapshot(tx, user, query))
		if err != nil {
			return nil, err
		}
		var out struct {
			Entries []DictionaryEntry `json:"entries"`
		}
		if err = json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		if len(out.Entries) == 0 {
			return nil, nil
		}
		return &out.Entries[0], nil
	}
	old, err := lookup(previous)
	if err != nil {
		return change, err
	}
	if old == nil {
		return change, errDataNotFound
	}
	if next != nil && (old.Code != next.Code || old.Word != next.Word) {
		duplicate, err := lookup(*next)
		if err != nil {
			return change, err
		}
		if duplicate != nil {
			return change, errDictionaryDuplicate
		}
	}
	err = tx.QueryRow(ctx, `SELECT id FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 AND code=$3 AND word=$4`, user, old.Kind, old.Code, old.Word).Scan(&old.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return change, err
	}
	inserted := err == nil
	old.UserInserted = &inserted
	change.Revision = current + 1
	old.Revision = change.Revision
	old.UpdatedAt = time.Now().UTC()
	change.Previous = old
	batch := &pgx.Batch{}
	if inserted {
		batch.Queue(`DELETE FROM user_dictionary_entries WHERE user_id=$1 AND id=$2`, user, old.ID)
	}
	if next != nil {
		replacement := *next
		replacement.ID = old.ID
		replacement.UserInserted = &inserted
		replacement.Revision = change.Revision
		replacement.UpdatedAt = old.UpdatedAt
		change.Replacement = &replacement
		if inserted {
			batch.Queue(`INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, replacement.ID, user, replacement.Kind, replacement.Code, replacement.Word, replacement.Weight, replacement.Revision, replacement.UpdatedAt)
		}
	}
	batch.Queue(`INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision`, user, change.Revision)
	query, args, err := dictionaryOverlayAndChangeQuery(user, change)
	if err != nil {
		return change, err
	}
	batch.Queue(query, args...)
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return change, err
	}
	return change, tx.Commit(ctx)
}

func (a *Service) dictionaryManage(w http.ResponseWriter, r *http.Request) {
	user, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !dictionaryKind(kind) {
		writeError(w, 404, "unknown_dictionary_kind")
		return
	}
	var input struct {
		Revision *int64 `json:"revision"`
		Previous *struct {
			Code string `json:"code"`
			Word string `json:"word"`
		} `json:"previous"`
		Replacement json.RawMessage `json:"replacement"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.Revision == nil || *input.Revision < 0 || input.Previous == nil || input.Replacement == nil || !validPositionText(input.Previous.Code, 512) || !validPositionText(input.Previous.Word, 2048) {
		writeError(w, 400, "invalid_dictionary_edit")
		return
	}
	previous := DictionaryEntry{Kind: kind, Code: input.Previous.Code, Word: input.Previous.Word}
	var next *DictionaryEntry
	if string(input.Replacement) != "null" {
		var value struct {
			Code   string `json:"code"`
			Word   string `json:"word"`
			Weight *int64 `json:"weight"`
		}
		if err := strictSnapshotJSON(input.Replacement, &value); err != nil || value.Weight == nil {
			writeError(w, 400, "invalid_dictionary_edit")
			return
		}
		entries, err := a.validateDictionary(r.Context(), kind, []DictionaryEntry{{Code: value.Code, Word: value.Word, Weight: *value.Weight}})
		if err != nil {
			a.dictionaryError(w, err)
			return
		}
		next = &entries[0]
	}
	change, err := a.store.EditManagedDictionary(r.Context(), user.UserID, *input.Revision, previous, next, a.engine)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, change)
}
