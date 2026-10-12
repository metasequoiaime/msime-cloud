package account

import (
	"context"
	"errors"
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

// 「差评」只列还没处理的，最新的在前并带总数；写进 FAQ 或忽略后读不到、不再列出，但周报用的 RecentQQBadAnswers 照样计数。
func TestQQBadAnswerStates(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_bad_answers`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"一", "二", "三"} {
		if err := a.AddQQBadAnswer(ctx, q, "a", "不对", ""); err != nil {
			t.Fatal(err)
		}
	}
	list, total, err := a.OpenQQBadAnswers(ctx, 14*24*time.Hour, 2)
	if err != nil || len(list) != 2 || total != 3 || list[0].Question != "三" {
		t.Fatal(list, total, err)
	}
	b, err := a.QQBadAnswer(ctx, list[0].ID)
	if err != nil || b.Question != "三" {
		t.Fatal(b, err)
	}
	if ok, _ := a.CloseQQBadAnswer(ctx, b.ID, "done"); !ok {
		t.Fatal("close failed")
	}
	if ok, _ := a.CloseQQBadAnswer(ctx, b.ID, "ignored"); ok {
		t.Fatal("closed twice")
	}
	if _, err := a.CloseQQBadAnswer(ctx, b.ID, "bogus"); err == nil {
		t.Fatal("invalid state accepted")
	}
	if _, err := a.QQBadAnswer(ctx, b.ID); !errors.Is(err, ErrQQBadAnswerNotFound) {
		t.Fatal(err)
	}
	if list, total, _ := a.OpenQQBadAnswers(ctx, 14*24*time.Hour, 10); len(list) != 2 || total != 2 {
		t.Fatal(list, total)
	}
	if recent, _ := a.RecentQQBadAnswers(ctx, 7*24*time.Hour, 10); len(recent) != 3 {
		t.Fatal(recent)
	}
}

// 同一个问题又被说「不对」时并进 30 分钟内最新的那条还没处理的差评，不新增一行；隔得久了、问题不同或者已经处理过的不并。
func TestQQBadAnswerUpdateFeedback(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_bad_answers`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO qq_bad_answers(question,answer,feedback,created_at) VALUES('五笔怎么切换','a','旧的',now()-interval '2 hours')`); err != nil {
		t.Fatal(err)
	}
	if err := a.AddQQBadAnswer(ctx, "五笔怎么切换", "在设置里", "不对", "按 Ctrl+` 切换"); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.UpdateQQBadAnswerFeedback(ctx, "五笔怎么切换", "还是不对", 30*time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := a.UpdateQQBadAnswerFeedback(ctx, "双拼怎么设置", "不对", 30*time.Minute); err != nil || ok {
		t.Fatal(ok, err)
	}
	list, total, err := a.OpenQQBadAnswers(ctx, 14*24*time.Hour, 10)
	if err != nil || total != 2 || list[0].Feedback != "不对；还是不对" || list[0].Retry != "按 Ctrl+` 切换" || list[1].Feedback != "旧的" {
		t.Fatal(list, total, err)
	}
	if ok, _ := a.CloseQQBadAnswer(ctx, list[0].ID, "ignored"); !ok {
		t.Fatal("close failed")
	}
	// 30 分钟内的已经处理过，更早的那条超出范围：都不并。
	if ok, err := a.UpdateQQBadAnswerFeedback(ctx, "五笔怎么切换", "不对", 30*time.Minute); err != nil || ok {
		t.Fatal(ok, err)
	}
}
