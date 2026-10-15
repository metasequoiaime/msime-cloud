package account

import (
	"context"
	"testing"
	"time"
)

func TestQQDraftClaimSnapshotIncludesConcurrentSupplement(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_drafts`); err != nil {
		t.Fatal(err)
	}
	id, err := a.CreateQQDraft(ctx, "G/M1", "旧原话", `{"kind":"bug","platform":"android"}`)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	const original = "补充后改为 Windows"
	const report = `{"kind":"bug","platform":"windows","ime_version":"0.9.5"}`
	if _, err := blocker.Exec(ctx, `UPDATE qq_drafts SET original=$2,report=$3,updated_at=clock_timestamp() WHERE id=$1`, id, original, report); err != nil {
		t.Fatal(err)
	}
	type result struct {
		draft QQDraft
		ok    bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		draft, ok, err := a.ClaimQQDraftSnapshot(ctx, id)
		done <- result{draft, ok, err}
	}()
	// 确认认领确实在等待补充事务，提交后必须返回新版本，而非语句启动时的旧内容。
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got.err != nil || !got.ok || got.draft.ID != id || got.draft.Subject != QQDraftSubject("G/M1") || got.draft.Original != original || got.draft.Report != report || !got.draft.Filed || got.draft.FiledAt.IsZero() || got.draft.UpdatedAt.IsZero() {
		t.Fatalf("认领未返回补充后的完整草稿：%+v", got)
	}
	if ok, err := a.UpdateQQDraft(ctx, id, "迟到的补充", `{}`); err != nil || ok {
		t.Fatalf("已认领的内容仍可修改：ok=%v，错误=%v", ok, err)
	}
	if _, ok, err := a.ClaimQQDraftSnapshot(ctx, id); err != nil || ok {
		t.Fatalf("已认领的草稿再次被认领：ok=%v，错误=%v", ok, err)
	}
	if ok, err := a.ReleaseQQDraftLease(ctx, id, got.draft.FiledAt); err != nil || !ok {
		t.Fatalf("返回的认领时间不能释放草稿：ok=%v，错误=%v", ok, err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE qq_drafts SET created_at=now()-interval '31 days' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.ClaimQQDraftSnapshot(ctx, id); err != nil || ok {
		t.Fatalf("过期草稿被认领：ok=%v，错误=%v", ok, err)
	}
}
