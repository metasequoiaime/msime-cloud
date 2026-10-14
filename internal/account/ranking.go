package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

type CandidateSelection struct {
	Context string `json:"context"`
	Code    string `json:"code"`
	Word    string `json:"word"`
	Count   int    `json:"count"`
}
type RankingAction struct {
	Code         string `json:"code"`
	Word         string `json:"word"`
	Mode         string `json:"mode"`
	LinearStep   int    `json:"linear_step"`
	TriggerCount int    `json:"trigger_count"`
	ForceTop     bool   `json:"force_top"`
}
type RankingResult struct {
	Deleted   *DictionaryEntry   `json:"deleted,omitempty"`
	Updates   []DictionaryEntry  `json:"updates"`
	Selection CandidateSelection `json:"selection"`
	Changed   bool               `json:"changed"`
	Revision  int64              `json:"revision"`
}

func (s *Store) RankCandidate(ctx context.Context, user string, expected int64, cfg engine.Config, query map[string]any, action RankingAction) (RankingResult, error) {
	return s.mutateCandidate(ctx, user, expected, cfg, query, action, "personal_rank")
}

type rankingEntryKey struct {
	kind, code, word string
}

// persistRankingUpdates 一次更新排名产生的所有个人词条和覆盖投影。引擎通常会返回一页候选词，逐条更新会让一次排名动作产生两倍于候选数的数据库往返。
func persistRankingUpdates(ctx context.Context, tx pgx.Tx, user string, revision int64, updates []DictionaryEntry) error {
	if len(updates) == 0 {
		return nil
	}
	updatedAt := time.Now().UTC()
	for i := range updates {
		updates[i].Revision, updates[i].UpdatedAt = revision, updatedAt
	}
	raw, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `WITH input AS MATERIALIZED (
 SELECT value, value->>'kind' AS kind, value->>'code' AS code, value->>'word' AS word
 FROM jsonb_array_elements($2::jsonb) AS items(value)
), updated AS (
 UPDATE user_dictionary_entries e
 SET weight=(i.value->>'weight')::bigint,revision=$3,updated_at=$4
 FROM input i
 WHERE e.user_id=$1 AND e.kind=i.kind AND e.code=i.code AND e.word=i.word
 RETURNING e.id,e.kind,e.code,e.word
), written AS (
 INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted)
 SELECT $1,i.kind,i.code,i.word,
   CASE WHEN u.id IS NULL THEN i.value ELSE jsonb_set(i.value,'{id}',to_jsonb(u.id)) END,
   false
 FROM input i LEFT JOIN updated u ON u.kind=i.kind AND u.code=i.code AND u.word=i.word
 ON CONFLICT(user_id,kind,code,word) DO UPDATE SET entry=excluded.entry,deleted=false
)
SELECT kind,code,word,id FROM updated`, user, raw, revision, updatedAt)
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := make(map[rankingEntryKey]string, len(updates))
	for rows.Next() {
		var key rankingEntryKey
		var id string
		if err = rows.Scan(&key.kind, &key.code, &key.word, &id); err != nil {
			return err
		}
		ids[key] = id
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range updates {
		if id, ok := ids[rankingEntryKey{kind: updates[i].Kind, code: updates[i].Code, word: updates[i].Word}]; ok {
			updates[i].ID = id
		}
	}
	return nil
}

func (s *Store) mutateCandidate(ctx context.Context, user string, expected int64, cfg engine.Config, query map[string]any, action RankingAction, operation string) (RankingResult, error) {
	var result RankingResult
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)`, user).Scan(&revision); err != nil {
		return result, err
	}
	if revision != expected {
		return result, errRevisionConflict
	}
	raw, err := cfg.QuerySnapshot(ctx, map[string]any{"operation": operation, "query": query, "action": action}, func(ctx context.Context, w io.Writer) error {
		return streamDictionaryQuerySnapshot(ctx, tx, user, query, func(raw json.RawMessage) error {
			if _, err := w.Write(raw); err != nil {
				return err
			}
			_, err := w.Write([]byte{'\n'})
			return err
		})
	})
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	result.Revision = revision + 1
	if err = persistRankingUpdates(ctx, tx, user, result.Revision, result.Updates); err != nil {
		return result, err
	}
	var selection *CandidateSelection
	if result.Deleted != nil {
		e := result.Deleted
		e.Revision = result.Revision
		e.UpdatedAt = time.Now().UTC()
		if _, err = tx.Exec(ctx, `DELETE FROM user_dictionary_entries WHERE user_id=$1 AND kind=$2 AND code=$3 AND word=$4`, user, e.Kind, e.Code, e.Word); err != nil {
			return result, err
		}
		data, err := json.Marshal(e)
		if err != nil {
			return result, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted) VALUES($1,$2,$3,$4,$5::jsonb,true) ON CONFLICT(user_id,kind,code,word) DO UPDATE SET entry=excluded.entry,deleted=true`, user, e.Kind, e.Code, e.Word, data); err != nil {
			return result, err
		}
	} else {
		selection = &result.Selection
		p := result.Selection
		if p.Count == 0 {
			_, err = tx.Exec(ctx, `DELETE FROM user_candidate_selections WHERE user_id=$1 AND context=$2 AND code=$3 AND word=$4`, user, p.Context, p.Code, p.Word)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO user_candidate_selections(user_id,context,code,word,count) VALUES($1,$2,$3,$4,$5) ON CONFLICT(user_id,context,code,word) DO UPDATE SET count=excluded.count`, user, p.Context, p.Code, p.Word, p.Count)
		}
		if err != nil {
			return result, err
		}

	}
	if _, err = tx.Exec(ctx, `INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision`, user, result.Revision); err != nil {
		return result, err
	}
	data, err := json.Marshal(DictionaryChange{Revision: result.Revision, Ranking: result.Updates, Selection: selection, Previous: result.Deleted})
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO user_dictionary_changes(user_id,revision,change) VALUES($1,$2,$3::jsonb)`, user, result.Revision, data); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
func (a *Service) candidateRanking(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		Revision *int64        `json:"revision"`
		Query    PersonalQuery `json:"query"`
		Action   RankingAction `json:"action"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.Revision == nil || *v.Revision < 0 || !validPositionText(v.Action.Code, 512) || !validPositionText(v.Action.Word, 2048) || len(v.Action.Code)+len(v.Action.Word) > 1536 {
		writeError(w, 400, "invalid_ranking_action")
		return
	}
	if v.Action.Mode == "" {
		v.Action.Mode = "pin"
	}
	if v.Action.LinearStep == 0 {
		v.Action.LinearStep = 1
	}
	if v.Action.TriggerCount == 0 {
		v.Action.TriggerCount = 1
	}
	switch v.Action.Mode {
	case "disabled", "pin", "halve", "linear", "promote":
	default:
		writeError(w, 400, "invalid_ranking_mode")
		return
	}
	if v.Action.LinearStep < 1 || v.Action.LinearStep > 100 || v.Action.TriggerCount < 1 || v.Action.TriggerCount > 10 || v.Query.Kind == "quick" {
		writeError(w, 400, "invalid_ranking_action")
		return
	}
	query, ok := preparePersonalQuery(w, v.Query)
	if !ok {
		return
	}
	result, err := a.store.RankCandidate(r.Context(), p.UserID, *v.Revision, a.engine, query, v.Action)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, result)
}

func (a *Service) candidateDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		Revision *int64        `json:"revision"`
		Query    PersonalQuery `json:"query"`
		Code     string        `json:"code"`
		Word     string        `json:"word"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.Revision == nil || *v.Revision < 0 || !validPositionText(v.Code, 512) || !validPositionText(v.Word, 2048) || len(v.Code)+len(v.Word) > 1536 || v.Query.Kind == "quick" {
		writeError(w, 400, "invalid_candidate_removal")
		return
	}
	query, ok := preparePersonalQuery(w, v.Query)
	if !ok {
		return
	}
	result, err := a.store.mutateCandidate(r.Context(), p.UserID, *v.Revision, a.engine, query, RankingAction{Code: v.Code, Word: v.Word}, "personal_delete")
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, DictionaryChange{Revision: result.Revision, Previous: result.Deleted})
}
