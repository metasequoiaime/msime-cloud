package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQQDrafts(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_drafts`); err != nil {
		t.Fatal(err)
	}
	id, err := a.CreateQQDraft(ctx, "G/M1", "候选框跑了", `{"kind":"bug"}`)
	if err != nil || id <= 0 {
		t.Fatal(id, err)
	}
	d, err := a.QQDraft(ctx, id)
	if err != nil || d.Original != "候选框跑了" || d.Report != `{"kind":"bug"}` || d.Filed || !QQDraftOwnedBy(d, "G/M1") || QQDraftOwnedBy(d, "G/M2") {
		t.Fatal(d, err)
	}
	if ok, err := a.UpdateQQDraft(ctx, id, "候选框跑了\n补充：Windows 11", `{"kind":"bug","platform":"windows"}`); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if drafts, err := a.OpenQQDrafts(ctx, 10); err != nil || len(drafts) != 1 || drafts[0].ID != id {
		t.Fatal(drafts, err)
	}
	// 认领只成功一次；提交失败放回后可以再认领；提交后不能再改、不再列出。
	if ok, _ := a.ClaimQQDraft(ctx, id); !ok {
		t.Fatal("first claim failed")
	}
	if ok, _ := a.ClaimQQDraft(ctx, id); ok {
		t.Fatal("claimed twice")
	}
	if err := a.ReleaseQQDraft(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.ClaimQQDraft(ctx, id); !ok {
		t.Fatal("released draft not claimable")
	}
	// 认领了还没记下 issue：带上认领时间，不能再认领、补充，也不再列出；认领超过 QQDraftClaimTimeout 当作提交被打断（例如副本被杀掉，没来得及放回），又可以认领、补充和列出。
	if d, _ := a.QQDraft(ctx, id); !d.Filed || d.FiledAt.IsZero() || QQDraftClaimStale(d, time.Now()) {
		t.Fatal(d)
	}
	if ok, _ := a.UpdateQQDraft(ctx, id, "候选框跑了\n补充：Windows 11", `{"kind":"bug","platform":"windows"}`); ok {
		t.Fatal("claimed draft updated")
	}
	if drafts, _ := a.OpenQQDrafts(ctx, 10); len(drafts) != 0 {
		t.Fatal(drafts)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET filed_at=now()-interval '6 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.QQDraft(ctx, id); !QQDraftClaimStale(d, time.Now()) {
		t.Fatal("interrupted claim not stale", d)
	}
	if ok, err := a.UpdateQQDraft(ctx, id, "候选框跑了\n补充：Windows 11", `{"kind":"bug","platform":"windows"}`); err != nil || !ok {
		t.Fatal("interrupted draft not updatable", ok, err)
	}
	if drafts, _ := a.OpenQQDrafts(ctx, 10); len(drafts) != 1 {
		t.Fatal(drafts)
	}
	if ok, _ := a.ClaimQQDraft(ctx, id); !ok {
		t.Fatal("interrupted draft not claimable")
	}
	if err := a.SetQQDraftIssue(ctx, id, "o/r#5"); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseQQDraft(ctx, id); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.QQDraft(ctx, id); !d.Filed || d.IssueRef != "o/r#5" {
		t.Fatal("filed draft released", d)
	}
	// 记下 issue 的草稿认领再久也不会被重新认领。
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET filed_at=now()-interval '1 day' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.ClaimQQDraft(ctx, id); ok {
		t.Fatal("filed draft claimed again")
	}
	if ok, _ := a.UpdateQQDraft(ctx, id, "x", "{}"); ok {
		t.Fatal("filed draft updated")
	}
	if drafts, _ := a.OpenQQDrafts(ctx, 10); len(drafts) != 0 {
		t.Fatal(drafts)
	}
	// 过期的读不到。
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET created_at=now()-interval '31 days' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QQDraft(ctx, id); !errors.Is(err, ErrQQDraftNotFound) {
		t.Fatal(err)
	}
}
