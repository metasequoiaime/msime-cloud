package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestQQFAQCandidates(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_faq_candidates`); err != nil {
		t.Fatal(err)
	}
	first, err := a.AddQQFAQCandidate(ctx, "双拼词库怎么导入", "")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := a.AddQQFAQCandidate(ctx, "有几个开发者", "主仓库 12 位贡献者")
	if list, total, err := a.OpenQQFAQCandidates(ctx, 10); err != nil || len(list) != 2 || list[0].ID != first || total != 2 {
		t.Fatal(list, total, err)
	}
	if c, err := a.QQFAQCandidate(ctx, second); err != nil || c.Answer != "主仓库 12 位贡献者" || c.Writing {
		t.Fatal(c, err)
	}
	if ok, _ := a.CloseQQFAQCandidate(ctx, second, "done", "msime-docs #40"); !ok {
		t.Fatal("close failed")
	}
	if ok, _ := a.CloseQQFAQCandidate(ctx, second, "ignored", ""); ok {
		t.Fatal("closed twice")
	}
	if _, err := a.QQFAQCandidate(ctx, second); !errors.Is(err, ErrQQFAQCandidateNotFound) {
		t.Fatal(err)
	}
	if _, err := a.CloseQQFAQCandidate(ctx, first, "bogus", ""); err == nil {
		t.Fatal("invalid state accepted")
	}
	if list, total, _ := a.OpenQQFAQCandidates(ctx, 10); len(list) != 1 || list[0].ID != first || total != 1 {
		t.Fatal(list)
	}
}

// 两个管理员同时「写进FAQ」同一条候选时只有一个认领到；认领中的不能忽略、不再列出，读出来带 Writing；没开成 PR 放回后可以再认领；开成了可以从 writing 关成 done；认领超时的和没认领的一样可以重新认领。
func TestQQFAQCandidateClaim(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_faq_candidates`); err != nil {
		t.Fatal(err)
	}
	id, err := a.AddQQFAQCandidate(ctx, "五笔怎么切换", "按 Ctrl+` 选五笔")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := a.ClaimQQFAQCandidate(ctx, id)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		t.Fatal("claimed", claimed)
	}
	if c, err := a.QQFAQCandidate(ctx, id); err != nil || !c.Writing || QQFAQCandidateClaimStale(c, time.Now()) {
		t.Fatal(c, err)
	}
	if list, total, _ := a.OpenQQFAQCandidates(ctx, 10); len(list) != 0 || total != 0 {
		t.Fatal("writing candidate listed", list)
	}
	if ok, _ := a.CloseQQFAQCandidate(ctx, id, "ignored", ""); ok {
		t.Fatal("ignored while writing")
	}
	if err := a.ReleaseQQFAQCandidate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if c, _ := a.QQFAQCandidate(ctx, id); c.Writing {
		t.Fatal("still writing after release")
	}
	if ok, _ := a.ClaimQQFAQCandidate(ctx, id); !ok {
		t.Fatal("not claimable after release")
	}
	// 认领超时（副本中途被杀掉，来不及放回）：可以重新认领，也重新出现在列表里。
	if _, err := db.pool.Exec(ctx, `UPDATE qq_faq_candidates SET claimed_at=now()-interval '6 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if c, _ := a.QQFAQCandidate(ctx, id); !QQFAQCandidateClaimStale(c, time.Now()) {
		t.Fatal("claim not stale", c)
	}
	if list, _, _ := a.OpenQQFAQCandidates(ctx, 10); len(list) != 1 {
		t.Fatal("stale claim not listed", list)
	}
	if ok, _ := a.ClaimQQFAQCandidate(ctx, id); !ok {
		t.Fatal("stale claim not reclaimable")
	}
	if ok, _ := a.CloseQQFAQCandidate(ctx, id, "done", "msime-docs #41"); !ok {
		t.Fatal("writing candidate not closed as done")
	}
	if ok, _ := a.ClaimQQFAQCandidate(ctx, id); ok {
		t.Fatal("done candidate claimed")
	}
	if err := a.ReleaseQQFAQCandidate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QQFAQCandidate(ctx, id); !errors.Is(err, ErrQQFAQCandidateNotFound) {
		t.Fatal("release reopened a done candidate", err)
	}
}

// 加 writing 状态和差评 state 列之前的表迁移后：已有的候选和差评仍是 open，state 的 CHECK 各只有一份并且认 writing，重复迁移不再改变，Ready 探测随之通过。
func TestQQFAQCandidateAndBadAnswerSchemaUpgrade(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, statement := range []string{
		`SELECT pg_advisory_xact_lock(8372419)`,
		`TRUNCATE qq_faq_candidates`, `TRUNCATE qq_bad_answers`,
		`ALTER TABLE qq_faq_candidates DROP COLUMN claimed_at`,
		`ALTER TABLE qq_faq_candidates DROP CONSTRAINT qq_faq_candidates_state_check`,
		`ALTER TABLE qq_faq_candidates ADD CONSTRAINT qq_faq_candidates_state_check CHECK (state IN ('open','done','ignored'))`,
		`ALTER TABLE qq_bad_answers DROP COLUMN state`,
	} {
		if _, err = tx.Exec(ctx, statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx); err == nil {
		t.Fatal("Ready passed without the new columns")
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO qq_faq_candidates(question) VALUES('旧候选'); INSERT INTO qq_bad_answers(question,answer,feedback) VALUES('旧差评','a','不对')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	var candidateState, badState string
	var candidateChecks, badChecks int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT state FROM qq_faq_candidates LIMIT 1),(SELECT state FROM qq_bad_answers LIMIT 1),(SELECT count(*) FROM pg_constraint WHERE conrelid='qq_faq_candidates'::regclass AND contype='c' AND conname LIKE 'qq_faq_candidates_state%'),(SELECT count(*) FROM pg_constraint WHERE conrelid='qq_bad_answers'::regclass AND contype='c' AND conname LIKE 'qq_bad_answers_state%')`).Scan(&candidateState, &badState, &candidateChecks, &badChecks); err != nil || candidateState != "open" || badState != "open" || candidateChecks != 1 || badChecks != 1 {
		t.Fatal("upgrade", candidateState, badState, candidateChecks, badChecks, err)
	}
	a := &Service{store: db}
	if list, _, err := a.OpenQQFAQCandidates(ctx, 10); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if ok, err := a.ClaimQQFAQCandidate(ctx, 0); err != nil || ok {
		t.Fatal(ok, err)
	}
	var id int64
	if err := db.pool.QueryRow(ctx, `SELECT id FROM qq_faq_candidates`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.ClaimQQFAQCandidate(ctx, id); err != nil || !ok {
		t.Fatal("writing rejected after upgrade", ok, err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE qq_bad_answers SET state='bogus'`); err == nil {
		t.Fatal("bad answer state not checked")
	}
}
