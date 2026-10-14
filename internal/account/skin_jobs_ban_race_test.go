package account

import (
	"context"
	"errors"
	"testing"
)

func TestCreateSkinJobRejectsBanRace(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "skin-job-ban-race@example.test"})
	ctx := t.Context()
	ban, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ban.Rollback(context.Background())
	if _, err := actionBanUser(&Service{store: db}, ctx, ban, actionRequest{ID: user.User.ID, Reason: "合成封禁原因"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := (&Service{store: db}).CreateSkinJob(ctx, "skin-job-ban-race", "user:"+user.User.ID, 3, 8, 10)
		done <- err
	}()
	waitForAdminDatabaseLock(t, db, ban.Conn().PgConn().PID())
	if err := ban.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrBanned) {
		t.Fatalf("封禁提交后仍创建插画任务：%v", err)
	}
	var jobs int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM skin_jobs WHERE id=$1`, "skin-job-ban-race").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("封禁后的插画任务仍留在数据库：%d", jobs)
	}
}
