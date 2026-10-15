package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQQDraftSupplementRejectsConcurrentChanges(t *testing.T) {
	for _, test := range []struct {
		name, original, report string
	}{
		{"原文改变", "另一条补充", `{"kind":"bug"}`},
		{"报告改变", "原话", `{"kind":"feature"}`},
		{"原文和报告改变", "另一条补充", `{"kind":"feature"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			id, err := a.CreateQQDraft(ctx, "G/M1", "原话", `{"kind":"bug"}`)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := a.QQDraft(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var savedAt time.Time
			if err := blocker.QueryRow(ctx, `UPDATE qq_drafts SET original=$2,report=$3,updated_at=clock_timestamp() WHERE id=$1 RETURNING updated_at`, id, test.original, test.report).Scan(&savedAt); err != nil {
				t.Fatal(err)
			}
			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				ok, err := a.UpdateQQDraft(ctx, previous, "旧原话加迟到的补充", `{}`)
				done <- result{ok, err}
			}()
			// 后来的保存已经开始等待行锁，仍必须比较锁释放后的最新内容。
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got.ok || !errors.Is(got.err, ErrQQDraftChanged) {
					t.Fatalf("未拒绝冲突补充：%+v", got)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			current, err := a.QQDraft(ctx, id)
			if err != nil || current.Original != test.original || current.Report != test.report || !current.UpdatedAt.Equal(savedAt) {
				t.Fatalf("已保存内容或更新时间被改写：草稿=%+v，错误=%v", current, err)
			}
			if ok, err := a.UpdateQQDraft(ctx, current, current.Original+"\n补充：重新发送的内容", current.Report); err != nil || !ok {
				t.Fatalf("基于最新内容重发仍失败：ok=%v，错误=%v", ok, err)
			}
		})
	}
}

func TestQQDraftSupplementOnlyOneWriterSucceeds(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := a.CreateQQDraft(ctx, "G/M1", "原话", `{"kind":"bug"}`)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := a.QQDraft(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		original, report string
		ok               bool
		err              error
	}
	start := make(chan struct{})
	done := make(chan result, 2)
	for _, original := range []string{"原话加版本", "原话加复现软件"} {
		go func() {
			<-start
			report := `{"problem":"` + original + `"}`
			ok, err := a.UpdateQQDraft(ctx, previous, original, report)
			done <- result{original, report, ok, err}
		}()
	}
	close(start)
	var winner result
	successes, conflicts := 0, 0
	for range 2 {
		select {
		case got := <-done:
			switch {
			case got.ok && got.err == nil:
				successes++
				winner = got
			case !got.ok && errors.Is(got.err, ErrQQDraftChanged):
				conflicts++
			default:
				t.Fatalf("保存结果异常：%+v", got)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	current, err := a.QQDraft(ctx, id)
	if successes != 1 || conflicts != 1 || err != nil || current.Original != winner.original || current.Report != winner.report {
		t.Fatalf("并发写入未保留唯一成功版本：成功=%d，冲突=%d，草稿=%+v，错误=%v", successes, conflicts, current, err)
	}
	if ok, err := a.UpdateQQDraft(ctx, QQDraft{ID: id + 1000000}, "不存在", `{}`); err != nil || ok {
		t.Fatalf("不存在的草稿未按不可更新处理：ok=%v，错误=%v", ok, err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET created_at=now()-interval '31 days' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.UpdateQQDraft(ctx, current, "过期", `{}`); err != nil || ok {
		t.Fatalf("过期草稿未按不可更新处理：ok=%v，错误=%v", ok, err)
	}
}
