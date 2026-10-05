package account

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// resourceApply merges a previewed word pack in one personal-data transaction.
// Unrelated entries survive; identical entries do not create extra changes.
func (a *Service) resourceApply(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		ResourceRevision   *int64 `json:"resource_revision"`
		DictionaryRevision *int64 `json:"dictionary_revision"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.ResourceRevision == nil || *input.ResourceRevision < 1 || input.DictionaryRevision == nil || *input.DictionaryRevision < 0 {
		writeError(w, 400, "revision_required")
		return
	}
	id := r.PathValue("id")
	var kind string
	var version int64
	var raw []byte
	err := a.store.pool.QueryRow(r.Context(), `SELECT kind,revision,content FROM community_resources WHERE id=$1 AND (moderation<>'removed' OR owner_id=$2)`, id, p.UserID).Scan(&kind, &version, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	// 短语包由客户端在本地合并进常用语，服务端不导入。
	if kind == "phrase" {
		writeError(w, 400, "unsupported_kind")
		return
	}
	if kind != "dictionary" {
		writeError(w, 400, "dictionary_resource_required")
		return
	}
	if version != *input.ResourceRevision {
		writeError(w, 409, "resource_revision_conflict")
		return
	}
	var content ResourceContent
	if err = json.Unmarshal(raw, &content); err != nil {
		a.error(w, err)
		return
	}
	content, err = a.validateResource(r.Context(), kind, content)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	tx, err := a.store.userDataTransaction(r.Context(), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// Keep this exact publication alive and unchanged until the import commits.
	err = tx.QueryRow(r.Context(), `SELECT revision FROM community_resources WHERE id=$1 AND (moderation<>'removed' OR owner_id=$2) FOR SHARE`, id, p.UserID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && version != *input.ResourceRevision) {
		writeError(w, 409, "resource_revision_conflict")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	var revision int64
	err = tx.QueryRow(r.Context(), `SELECT COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)`, p.UserID).Scan(&revision)
	if err != nil {
		a.error(w, err)
		return
	}
	if revision != *input.DictionaryRevision {
		writeError(w, 409, "revision_conflict")
		return
	}
	changed := 0
	for _, entry := range content.Entries {
		var existingID string
		var existingRevision, weight int64
		err = tx.QueryRow(r.Context(), `SELECT id,revision,weight FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 AND code=$3 AND word=$4`, p.UserID, entry.Kind, entry.Code, entry.Word).Scan(&existingID, &existingRevision, &weight)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.error(w, err)
			return
		}
		if err == nil && weight == entry.Weight {
			continue
		}
		change, err := a.store.dictionaryEdit(r.Context(), tx, p.UserID, entry.Kind, existingID, existingRevision, &DictionaryEntry{Code: entry.Code, Word: entry.Word, Weight: entry.Weight})
		if err != nil {
			a.dictionaryError(w, err)
			return
		}
		revision = change.Revision
		changed++
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"revision": revision, "imported": changed, "resource_revision": version})
}
