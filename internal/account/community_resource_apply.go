package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

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
	var createdAt time.Time
	err := a.store.pool.QueryRow(r.Context(), `SELECT kind,revision,content,created_at FROM community_resources WHERE id=$1 AND (moderation<>'removed' OR owner_id=$2)`, id, p.UserID).Scan(&kind, &version, &raw, &createdAt)
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
	// 锁定资源后同时读取个人词库版本，避免事务内为两个版本检查分别往返。
	// 正常发布的版本不会重用；仍防御管理写入重置版本后在本次请求内替换资源行。
	// 同时核对 kind、created_at 和 JSONB 内容，防止旧快照被导入新作品。
	var currentKind string
	var currentCreatedAt time.Time
	var currentRaw []byte
	var revision int64
	var resourceExists bool
	err = tx.QueryRow(r.Context(), `WITH resource AS MATERIALIZED (
 SELECT kind,revision,created_at,content FROM community_resources
 WHERE id=$1 AND (moderation<>'removed' OR owner_id=$2) FOR SHARE
)
SELECT COALESCE((SELECT kind FROM resource),''),
 COALESCE((SELECT revision FROM resource),0),
 COALESCE((SELECT created_at FROM resource),'epoch'::timestamptz),
 COALESCE((SELECT content FROM resource),'{}'::jsonb),
 COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$2),0),
 EXISTS(SELECT 1 FROM resource)`, id, p.UserID).Scan(&currentKind, &version, &currentCreatedAt, &currentRaw, &revision, &resourceExists)
	if err != nil {
		a.error(w, err)
		return
	}
	if !resourceExists || currentKind != kind || version != *input.ResourceRevision || !currentCreatedAt.Equal(createdAt) || !bytes.Equal(currentRaw, raw) {
		writeError(w, 409, "resource_revision_conflict")
		return
	}
	if revision != *input.DictionaryRevision {
		writeError(w, 409, "revision_conflict")
		return
	}
	revision, changed, err := mergeResourceDictionary(r.Context(), tx, p.UserID, revision, content.Entries)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"revision": revision, "imported": changed, "resource_revision": version})
}
