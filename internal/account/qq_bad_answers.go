package account

import (
	"context"
	"time"
)

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

// RecentQQBadAnswers 列出 since 以来被说答错的回答，最新的在前。
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
