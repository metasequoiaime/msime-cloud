package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

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
	for i := range result.Updates {
		e := &result.Updates[i]
		e.Revision = result.Revision
		e.UpdatedAt = time.Now().UTC()
		// Existing personal entries retain their stable ID; ranked base entries stay in the overlay only.
		rows, err := tx.Query(ctx, `UPDATE user_dictionary_entries SET weight=$5,revision=$6,updated_at=$7 WHERE user_id=$1 AND kind=$2 AND code=$3 AND word=$4 RETURNING id`, user, e.Kind, e.Code, e.Word, e.Weight, e.Revision, e.UpdatedAt)
		if err != nil {
			return result, err
		}
		if rows.Next() {
			if err = rows.Scan(&e.ID); err != nil {
				rows.Close()
				return result, err
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return result, err
		}
		data, err := json.Marshal(e)
		if err != nil {
			return result, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted) VALUES($1,$2,$3,$4,$5::jsonb,false) ON CONFLICT(user_id,kind,code,word) DO UPDATE SET entry=excluded.entry,deleted=false`, user, e.Kind, e.Code, e.Word, data); err != nil {
			return result, err
		}
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
