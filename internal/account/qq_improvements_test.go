package account

import (
	"context"
	"testing"
	"time"
)

func TestQQImprovementEvents(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_improvement_events, qq_bad_answers`); err != nil {
		t.Fatal(err)
	}
	if err := a.AddQQImprovementEvent(ctx, "unknown", "双拼词库怎么导入", "docs"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddQQImprovementEvent(ctx, "selftest", "", "11/12"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO qq_improvement_events(kind,topic,created_at) VALUES('unknown','旧的',now()-interval '10 days')`); err != nil {
		t.Fatal(err)
	}
	if err := a.AddQQBadAnswer(ctx, "五笔怎么切换", "在设置里", "不对", "按 Ctrl+` 切换"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	events, err := a.QQImprovementEvents(ctx, now.AddDate(0, 0, -7), now.Add(time.Minute))
	if err != nil || len(events) != 2 || events[0].Topic != "双拼词库怎么导入" || events[1].Detail != "11/12" {
		t.Fatal(events, err)
	}
	bad, err := a.QQBadAnswersBetween(ctx, now.AddDate(0, 0, -7), now.Add(time.Minute))
	if err != nil || len(bad) != 1 || bad[0].Feedback != "不对" {
		t.Fatal(bad, err)
	}
}
