package account

import "testing"

func TestExecOwnedCommunityMutationUsesUserWriteTransaction(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "community-owned-mutation@example.test"}).User.ID

	rows, err := (&Service{store: db}).execOwnedCommunityMutation(ctx, user, `UPDATE auth_users SET banned_at=banned_at WHERE id=$1`, user)
	if err != nil {
		t.Fatalf("执行社区作者 mutation 失败：%v", err)
	}
	if rows != 1 {
		t.Fatalf("影响行数=%d，期望 1", rows)
	}
}
