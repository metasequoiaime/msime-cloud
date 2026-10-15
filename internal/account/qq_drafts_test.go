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
	if err != nil || d.Original != "候选框跑了" || d.Report != `{"kind":"bug"}` || d.Filed || !QQDraftOwnedBy(d, "G/M1") || QQDraftOwnedBy(d, "G/M2") || d.UpdatedAt.IsZero() {
		t.Fatal(d, err)
	}
	// 补充后 updated_at 往后走：机器人只把 30 分钟内更新过的草稿当作「刚起的」，同一件事并进去（qqMergeDraft）。
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET updated_at=now()-interval '1 hour' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.UpdateQQDraft(ctx, d, "候选框跑了\n补充：Windows 11", `{"kind":"bug","platform":"windows"}`); err != nil || !ok {
		t.Fatal(ok, err)
	}
	d, err = a.QQDraft(ctx, id)
	if err != nil || time.Since(d.UpdatedAt) > time.Minute {
		t.Fatal("updated_at not refreshed", d.UpdatedAt, err)
	}
	if drafts, total, err := a.OpenQQDrafts(ctx, 10); err != nil || len(drafts) != 1 || drafts[0].ID != id || total != 1 {
		t.Fatal(drafts, total, err)
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
	if ok, _ := a.UpdateQQDraft(ctx, d, "候选框跑了\n补充：Windows 11", `{"kind":"bug","platform":"windows"}`); ok {
		t.Fatal("claimed draft updated")
	}
	if drafts, _, _ := a.OpenQQDrafts(ctx, 10); len(drafts) != 0 {
		t.Fatal(drafts)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET filed_at=now()-interval '6 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.QQDraft(ctx, id); !QQDraftClaimStale(d, time.Now()) {
		t.Fatal("interrupted claim not stale", d)
	}
	if ok, err := a.UpdateQQDraft(ctx, d, "候选框跑了\n补充：Windows 11", `{"kind":"bug","platform":"windows"}`); err != nil || !ok {
		t.Fatal("interrupted draft not updatable", ok, err)
	}
	if drafts, _, _ := a.OpenQQDrafts(ctx, 10); len(drafts) != 1 {
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
	if ok, _ := a.UpdateQQDraft(ctx, d, "x", "{}"); ok {
		t.Fatal("filed draft updated")
	}
	if drafts, _, _ := a.OpenQQDrafts(ctx, 10); len(drafts) != 0 {
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

func TestQQDraftClaimLeaseCannotOverwriteNewClaim(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_drafts`); err != nil {
		t.Fatal(err)
	}
	id, err := a.CreateQQDraft(ctx, "G/M1", "原话", `{"kind":"bug"}`)
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := a.ClaimQQDraftLease(ctx, id)
	if err != nil || !ok {
		t.Fatal(first, ok, err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET filed_at=now()-interval '6 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	second, ok, err := a.ClaimQQDraftLease(ctx, id)
	if err != nil || !ok || !second.After(first) {
		t.Fatal(first, second, ok, err)
	}
	if set, err := a.SetQQDraftIssueLease(ctx, id, first, "old #1"); err != nil || set {
		t.Fatal("stale claim overwrote newer claim", set, err)
	}
	if set, err := a.SetQQDraftIssueLease(ctx, id, second, "new #2"); err != nil || !set {
		t.Fatal("current claim was not recorded", set, err)
	}
	var ref string
	if err := db.pool.QueryRow(ctx, `SELECT issue_ref FROM qq_drafts WHERE id=$1`, id).Scan(&ref); err != nil || ref != "new #2" {
		t.Fatal(ref, err)
	}
}

// 列表最早的在前，总数不受 limit 影响；忽略只对还没提交的草稿生效，忽略后不再列出、不能补充和认领，认领中的不能忽略，认领超时的可以。
func TestQQDraftsOrderAndDismiss(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_drafts`); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := range 3 {
		id, err := a.CreateQQDraft(ctx, "G/M1", "候选框跑了", `{"kind":"bug"}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET created_at=now()-make_interval(days => $2) WHERE id=$1`, id, 10-i); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if drafts, total, err := a.OpenQQDrafts(ctx, 2); err != nil || total != 3 || len(drafts) != 2 || drafts[0].ID != ids[0] || drafts[1].ID != ids[1] {
		t.Fatal(drafts, total, err)
	}
	if ok, err := a.DismissQQDraft(ctx, ids[0]); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, _ := a.DismissQQDraft(ctx, ids[0]); ok {
		t.Fatal("dismissed twice")
	}
	if d, _ := a.QQDraft(ctx, ids[0]); !d.Filed || d.IssueRef != QQDraftIgnored || QQDraftClaimStale(d, time.Now().Add(time.Hour)) {
		t.Fatal(d)
	}
	if ok, _ := a.UpdateQQDraft(ctx, QQDraft{ID: ids[0]}, "x", "{}"); ok {
		t.Fatal("dismissed draft updated")
	}
	if ok, _ := a.ClaimQQDraft(ctx, ids[0]); ok {
		t.Fatal("dismissed draft claimed")
	}
	if err := a.ReleaseQQDraft(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.QQDraft(ctx, ids[0]); d.IssueRef != QQDraftIgnored || !d.Filed {
		t.Fatal("dismissed draft released", d)
	}
	if drafts, total, _ := a.OpenQQDrafts(ctx, 10); total != 2 || len(drafts) != 2 || drafts[0].ID != ids[1] {
		t.Fatal(drafts, total)
	}
	// 认领中的不能忽略；认领超时的可以。
	if ok, _ := a.ClaimQQDraft(ctx, ids[1]); !ok {
		t.Fatal("claim failed")
	}
	if ok, _ := a.DismissQQDraft(ctx, ids[1]); ok {
		t.Fatal("draft being filed dismissed")
	}
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET filed_at=now()-interval '6 minutes' WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.DismissQQDraft(ctx, ids[1]); err != nil || !ok {
		t.Fatal("interrupted draft not dismissable", ok, err)
	}
}
