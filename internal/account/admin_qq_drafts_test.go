package account

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestAdminQQDraftConcurrentEdits(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := WithAdminAccess(t.Context(), AdminAccess{Actor: "synthetic-admin", Permissions: []string{PermTriageIssues}})
	id, err := a.CreateQQDraft(ctx, "synthetic-subject", "合成原话", `{"kind":"bug"}`)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := a.QQDraft(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AdminEditQQDraft(context.Background(), draft, "拒绝编辑", `{}`); !errors.Is(err, errPermissionDenied) {
		t.Fatal(err)
	}
	if _, err := a.AdminCloseQQDraft(context.Background(), id); !errors.Is(err, errPermissionDenied) {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for _, text := range []string{"合成修改甲", "合成修改乙"} {
		go func() { _, err := a.AdminEditQQDraft(ctx, draft, text, draft.Report); done <- err }()
	}
	success, conflict := 0, 0
	for range 2 {
		err := <-done
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrQQDraftChanged):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	current, err := a.QQDraft(ctx, id)
	if err != nil || !current.UpdatedAt.After(draft.UpdatedAt) {
		t.Fatal(current, err)
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE action='qq_draft_edit' AND target=$1 AND actor='synthetic-admin'`, "D"+fmt.Sprint(id)).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if ok, err := a.AdminCloseQQDraft(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := a.AdminEditQQDraft(ctx, current, "关闭后的修改", `{}`); !errors.Is(err, ErrQQDraftChanged) {
		t.Fatal(err)
	}
}
