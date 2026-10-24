package account

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// QQImprovementEvent 是一条改进依据：答不上的问题或一次自测的结果。
type QQImprovementEvent struct {
	Kind      string
	Topic     string
	Detail    string
	CreatedAt time.Time
}

// AddQQImprovementEvent 记下一条改进依据。
func (a *Service) AddQQImprovementEvent(ctx context.Context, kind, topic, detail string) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO qq_improvement_events(kind,topic,detail) VALUES($1,$2,$3)`, kind, topic, detail)
	return err
}

// QQImprovementEvents 列出 [from, to) 之间的改进依据，按时间从早到晚。
func (a *Service) QQImprovementEvents(ctx context.Context, from, to time.Time) ([]QQImprovementEvent, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT kind,topic,detail,created_at FROM qq_improvement_events WHERE created_at>=$1 AND created_at<$2 ORDER BY created_at`, from, to)
	if err != nil {
		return nil, err
	}
	return collectQQRows(rows, func(row pgx.CollectableRow) (QQImprovementEvent, error) {
		var e QQImprovementEvent
		err := row.Scan(&e.Kind, &e.Topic, &e.Detail, &e.CreatedAt)
		return e, err
	})
}

// QQBadAnswersBetween 列出 [from, to) 之间被说答错的回答，按时间从早到晚，周报用。
func (a *Service) QQBadAnswersBetween(ctx context.Context, from, to time.Time) ([]QQBadAnswer, error) {
	return a.queryQQBadAnswers(ctx, `SELECT id,question,answer,feedback,retry,created_at FROM qq_bad_answers WHERE created_at>=$1 AND created_at<$2 ORDER BY created_at`, from, to)
}
