package account

import "testing"

func TestUserDataRevisionTransactionRollsBackOnRevisionConflict(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "revision-transaction@example.test"}).User.ID

	tx, revision, err := db.userDataRevisionTransaction(ctx, user, 1)
	if err != errRevisionConflict {
		t.Fatalf("版本冲突结果=(%v,%d,%v)，期望 errRevisionConflict 且无事务", tx, revision, err)
	}
	if tx != nil || revision != 0 {
		t.Fatalf("版本冲突仍返回事务或版本：事务=%v，版本=%d", tx, revision)
	}
	if _, err := db.pool.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("版本冲突后事务未回滚并释放连接：%v", err)
	}
}
