package account

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type CandidatePosition struct {
	Context  string `json:"context"`
	Code     string `json:"code"`
	Word     string `json:"word"`
	Position int    `json:"position"`
}

func (s *Store) SetCandidatePosition(ctx context.Context, user string, expected int64, p CandidatePosition) (int64, error) {
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)`, user).Scan(&revision); err != nil {
		return 0, err
	}
	if revision != expected {
		return 0, errRevisionConflict
	}
	batch := &pgx.Batch{}
	if p.Position > 0 {
		batch.Queue(`DELETE FROM user_candidate_positions WHERE user_id=$1 AND context=$2 AND position=$3`, user, p.Context, p.Position)
		batch.Queue(`INSERT INTO user_candidate_positions(user_id,context,code,word,position) VALUES($1,$2,$3,$4,$5) ON CONFLICT(user_id,context,code,word) DO UPDATE SET position=excluded.position`, user, p.Context, p.Code, p.Word, p.Position)
	} else {
		batch.Queue(`DELETE FROM user_candidate_positions WHERE user_id=$1 AND context=$2 AND code=$3 AND word=$4`, user, p.Context, p.Code, p.Word)
	}
	revision++
	batch.Queue(`INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision`, user, revision)
	raw, err := json.Marshal(DictionaryChange{Revision: revision, Position: &p})
	if err != nil {
		return 0, err
	}
	batch.Queue(`INSERT INTO user_dictionary_changes(user_id,revision,change) VALUES($1,$2,$3::jsonb)`, user, revision, raw)
	// 槽位删除必须先于写入；批量发送保持执行顺序，任一语句失败仍由外层事务回滚。
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return 0, err
	}
	return revision, tx.Commit(ctx)
}
func (s *Store) CandidatePositions(ctx context.Context, user, contextKey string, offset, limit int) ([]CandidatePosition, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT context,code,word,position FROM user_candidate_positions WHERE user_id=$1 AND ($2='' OR context=$2) ORDER BY context,position LIMIT $3 OFFSET $4`, user, contextKey, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []CandidatePosition{}
	for rows.Next() {
		var p CandidatePosition
		if err = rows.Scan(&p.Context, &p.Code, &p.Word, &p.Position); err != nil {
			return nil, false, err
		}
		out = append(out, p)
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
func validPositionText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n\t")
}
func (a *Service) candidatePositions(w http.ResponseWriter, r *http.Request) {
	user, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if r.Method == "GET" {
		query := r.URL.Query()
		offset, limit, ok := dictionaryPage(query)
		key := query.Get("context")
		if !ok || (key != "" && !validPositionText(key, 512)) {
			writeError(w, 400, "invalid_position_query")
			return
		}
		positions, more, err := a.store.CandidatePositions(r.Context(), user.UserID, key, offset, limit)
		if err != nil {
			a.dictionaryError(w, err)
			return
		}
		write(w, 200, map[string]any{"positions": positions, "has_more": more, "offset": offset})
		return
	}
	var input struct {
		Revision *int64 `json:"revision"`
		Context  string `json:"context"`
		Code     string `json:"code"`
		Word     string `json:"word"`
		Position *int   `json:"position"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.Revision == nil || *input.Revision < 0 || !validPositionText(input.Context, 512) || !validPositionText(input.Code, 512) || !validPositionText(input.Word, 2048) || len(input.Context)+len(input.Code)+len(input.Word) > 2048 {
		writeError(w, 400, "invalid_candidate_position")
		return
	}
	position := 0
	if r.Method == "PUT" {
		if input.Position == nil || *input.Position < 1 || *input.Position > 5 {
			writeError(w, 400, "invalid_candidate_position")
			return
		}
		position = *input.Position
	} else if input.Position != nil {
		writeError(w, 400, "invalid_candidate_position")
		return
	}
	revision, err := a.store.SetCandidatePosition(r.Context(), user.UserID, *input.Revision, CandidatePosition{input.Context, input.Code, input.Word, position})
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, map[string]any{"revision": revision})
}
