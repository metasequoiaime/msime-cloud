package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrQQBadAnswerNotFound 表示差评不存在、已经处理或者已被清理。
var ErrQQBadAnswerNotFound = errors.New("qq bad answer not found")

// QQBadAnswer 是一条被群友说答错的回答。
type QQBadAnswer struct {
	ID        int64
	Question  string
	Answer    string
	Feedback  string
	Retry     string
	CreatedAt time.Time
}

// AddQQBadAnswer 记下一条被说答错的回答和重新查以后的回答。
func (a *Service) AddQQBadAnswer(ctx context.Context, question, answer, feedback, retry string) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO qq_bad_answers(question,answer,feedback,retry) VALUES($1,$2,$3,$4)`, question, answer, feedback, retry)
	return err
}

// RecentQQBadAnswers 列出 since 以来被说答错的回答（不论处理了没有，周报按它计数），最新的在前。
func (a *Service) RecentQQBadAnswers(ctx context.Context, since time.Duration, limit int) ([]QQBadAnswer, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,question,answer,feedback,retry,created_at FROM qq_bad_answers WHERE created_at>now()-$1::interval ORDER BY created_at DESC LIMIT $2`, since.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QQBadAnswer
	for rows.Next() {
		var b QQBadAnswer
		if err := rows.Scan(&b.ID, &b.Question, &b.Answer, &b.Feedback, &b.Retry, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// OpenQQBadAnswers 列出 since 以来还没处理的差评，最新的在前，另外返回这样的差评一共有多少条。管理员的「差评」用它：写进 FAQ 或忽略过的不再列出。
func (a *Service) OpenQQBadAnswers(ctx context.Context, since time.Duration, limit int) ([]QQBadAnswer, int, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,question,answer,feedback,retry,created_at,count(*) OVER() FROM qq_bad_answers WHERE state='open' AND created_at>now()-$1::interval ORDER BY created_at DESC, id DESC LIMIT $2`, since.String(), limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []QQBadAnswer
	total := 0
	for rows.Next() {
		var b QQBadAnswer
		if err := rows.Scan(&b.ID, &b.Question, &b.Answer, &b.Feedback, &b.Retry, &b.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// QQBadAnswer 读一条还没处理的差评。
func (a *Service) QQBadAnswer(ctx context.Context, id int64) (QQBadAnswer, error) {
	var b QQBadAnswer
	err := a.store.pool.QueryRow(ctx, `SELECT id,question,answer,feedback,retry,created_at FROM qq_bad_answers WHERE id=$1 AND state='open'`, id).Scan(&b.ID, &b.Question, &b.Answer, &b.Feedback, &b.Retry, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrQQBadAnswerNotFound
	}
	return b, err
}

// CloseQQBadAnswer 把差评标成 done（写进了 FAQ）或 ignored，返回是否改了（已经处理过的不再改）。
func (a *Service) CloseQQBadAnswer(ctx context.Context, id int64, state string) (bool, error) {
	if state != "done" && state != "ignored" {
		return false, errors.New("invalid bad answer state")
	}
	tag, err := a.store.pool.Exec(ctx, `UPDATE qq_bad_answers SET state=$2 WHERE id=$1 AND state='open'`, id, state)
	return tag.RowsAffected() == 1, err
}
