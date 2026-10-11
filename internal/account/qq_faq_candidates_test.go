package account

import (
	"context"
	"errors"
	"testing"
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
	if list, err := a.OpenQQFAQCandidates(ctx, 10); err != nil || len(list) != 2 || list[0].ID != second {
		t.Fatal(list, err)
	}
	if c, err := a.QQFAQCandidate(ctx, second); err != nil || c.Answer != "主仓库 12 位贡献者" {
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
	if list, _ := a.OpenQQFAQCandidates(ctx, 10); len(list) != 1 || list[0].ID != first {
		t.Fatal(list)
	}
}
