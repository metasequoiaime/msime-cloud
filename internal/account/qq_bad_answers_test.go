package account

import (
	"context"
	"testing"
	"time"
)

func TestQQBadAnswers(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_bad_answers`); err != nil {
		t.Fatal(err)
	}
	if err := a.AddQQBadAnswer(ctx, "五笔怎么切换", "在设置里", "不对", "按 Ctrl+` 切换"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO qq_bad_answers(question,answer,feedback,created_at) VALUES('旧的','a','错了',now()-interval '20 days')`); err != nil {
		t.Fatal(err)
	}
	list, err := a.RecentQQBadAnswers(ctx, 14*24*time.Hour, 10)
	if err != nil || len(list) != 1 || list[0].Question != "五笔怎么切换" || list[0].Retry != "按 Ctrl+` 切换" {
		t.Fatal(list, err)
	}
}
