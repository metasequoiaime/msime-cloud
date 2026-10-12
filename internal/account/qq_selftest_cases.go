package account

import "context"

// QQSelfTestCase 是一道从核对过的答案里学到的自测题。
type QQSelfTestCase struct {
	Question string
	Answer   string
}

// AddQQSelfTestCase 记下一道自测题；同一个问题已经有了就换成新的答案。
func (a *Service) AddQQSelfTestCase(ctx context.Context, question, answer string) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO qq_selftest_cases(question,answer) VALUES($1,$2) ON CONFLICT (question) DO UPDATE SET answer=EXCLUDED.answer, created_at=now()`, question, answer)
	return err
}

// QQSelfTestCases 列出最近记下的至多 limit 道自测题，从新到旧。
func (a *Service) QQSelfTestCases(ctx context.Context, limit int) ([]QQSelfTestCase, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT question,answer FROM qq_selftest_cases ORDER BY created_at DESC, question LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QQSelfTestCase
	for rows.Next() {
		var c QQSelfTestCase
		if err := rows.Scan(&c.Question, &c.Answer); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
