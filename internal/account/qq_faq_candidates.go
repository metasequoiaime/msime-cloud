package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// QQFAQCandidateWindow 是文档候选的有效期：更早的不再列出，并由 Store.Prune 删除。
const QQFAQCandidateWindow = 30 * 24 * time.Hour

// ErrQQFAQCandidateNotFound 表示候选不存在、已经处理或者过期。
var ErrQQFAQCandidateNotFound = errors.New("qq faq candidate not found")

// QQFAQCandidate 是一条文档候选。
type QQFAQCandidate struct {
	ID        int64
	Question  string
	Answer    string
	CreatedAt time.Time
}

// AddQQFAQCandidate 记下一条文档候选。
func (a *Service) AddQQFAQCandidate(ctx context.Context, question, answer string) (int64, error) {
	var id int64
	err := a.store.pool.QueryRow(ctx, `INSERT INTO qq_faq_candidates(question,answer) VALUES($1,$2) RETURNING id`, question, answer).Scan(&id)
	return id, err
}

// QQFAQCandidate 读一条有效期内、还没处理的候选。
func (a *Service) QQFAQCandidate(ctx context.Context, id int64) (QQFAQCandidate, error) {
	var c QQFAQCandidate
	err := a.store.pool.QueryRow(ctx, `SELECT id,question,answer,created_at FROM qq_faq_candidates WHERE id=$1 AND state='open' AND created_at>now()-$2::interval`, id, QQFAQCandidateWindow.String()).Scan(&c.ID, &c.Question, &c.Answer, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrQQFAQCandidateNotFound
	}
	return c, err
}

// OpenQQFAQCandidates 列出有效期内还没处理的候选，最新的在前。
func (a *Service) OpenQQFAQCandidates(ctx context.Context, limit int) ([]QQFAQCandidate, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,question,answer,created_at FROM qq_faq_candidates WHERE state='open' AND created_at>now()-$1::interval ORDER BY created_at DESC LIMIT $2`, QQFAQCandidateWindow.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QQFAQCandidate
	for rows.Next() {
		var c QQFAQCandidate
		if err := rows.Scan(&c.ID, &c.Question, &c.Answer, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CloseQQFAQCandidate 把候选标成 done（写进了 FAQ，ref 是 PR）或 ignored，返回是否改了（已经处理过的不再改）。
func (a *Service) CloseQQFAQCandidate(ctx context.Context, id int64, state, ref string) (bool, error) {
	if state != "done" && state != "ignored" {
		return false, errors.New("invalid candidate state")
	}
	tag, err := a.store.pool.Exec(ctx, `UPDATE qq_faq_candidates SET state=$2,ref=$3 WHERE id=$1 AND state='open'`, id, state, ref)
	return tag.RowsAffected() == 1, err
}
