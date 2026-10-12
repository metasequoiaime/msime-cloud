package account

import (
	"context"
	"time"
)

// QQConversationWindow 是 QQ 群答疑接上追问的时间范围：更早的问答不再读取。
const QQConversationWindow = 30 * time.Minute

// QQConversationRetention 是问答在表里留多久，更早的由 Store.Prune 删除。比接上追问的范围长得多，是为了群里说机器人答得差时维护者能回头看它到底答了什么：曾经 30 分钟就删，群友嘲讽之后连原话都找不到。群和成员标识照样只存摘要。
const QQConversationRetention = 7 * 24 * time.Hour

// QQTurn 是 QQ 群答疑的一轮问答。
type QQTurn struct {
	Question string
	Answer   string
}

// QQConversation 返回 subject（群和成员标识，由调用方拼成，这里只存摘要）最近 QQConversationWindow 内的最多 limit 轮问答，从早到晚排列。
func (a *Service) QQConversation(ctx context.Context, subject string, limit int) ([]QQTurn, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT question,answer FROM (SELECT question,answer,created_at FROM qq_conversations WHERE subject=$1 AND created_at>now()-$2::interval ORDER BY created_at DESC LIMIT $3) t ORDER BY created_at`, hash(subject), QQConversationWindow.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []QQTurn
	for rows.Next() {
		var turn QQTurn
		if err := rows.Scan(&turn.Question, &turn.Answer); err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

// RecordQQTurn 记下 subject 的一轮问答。
func (a *Service) RecordQQTurn(ctx context.Context, subject, question, answer string) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO qq_conversations(id,subject,question,answer) VALUES($1,$2,$3,$4)`, randomToken(), hash(subject), question, answer)
	return err
}
