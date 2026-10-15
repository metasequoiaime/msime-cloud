package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSkinJobMutationsReportWriteFailures(t *testing.T) {
	for _, name := range []string{"heartbeat", "finish", "delete"} {
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
				t.Fatal(err)
			}
			id := skinJobID('a')
			var heartbeat time.Time
			if err := db.pool.QueryRow(ctx, `INSERT INTO skin_jobs(id,owner,expires_at)
VALUES($1,'reject-write',clock_timestamp()+interval '1 minute') RETURNING heartbeat_at`, id).Scan(&heartbeat); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `ALTER TABLE skin_jobs ADD CONSTRAINT skin_job_test_reject CHECK(owner<>'reject-write') NOT VALID`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE skin_jobs DROP CONSTRAINT IF EXISTS skin_job_test_reject`); err != nil {
					t.Error(err)
				}
			})
			a := &Service{store: db}
			var accepted bool
			var err error
			switch name {
			case "heartbeat":
				accepted, err = a.SkinJobHeartbeat(ctx, id)
			case "finish":
				accepted, err = a.FinishSkinJob(ctx, id, "succeeded", "", []byte(`{"width":1}`))
			case "delete":
				err = a.DeleteSkinJob(ctx, id, "reject-write")
			}
			var failure *pgconn.PgError
			if accepted || !errors.As(err, &failure) || failure.Code != "23514" {
				t.Fatalf("任务写入失败未正确返回：accepted=%v err=%v", accepted, err)
			}
			var unchanged bool
			if err := db.pool.QueryRow(ctx, `SELECT state='running' AND reason='' AND artwork IS NULL AND NOT cancelled AND heartbeat_at=$2
FROM skin_jobs WHERE id=$1`, id, heartbeat).Scan(&unchanged); err != nil || !unchanged {
				t.Fatal("写入失败改变了原任务", unchanged, err)
			}
		})
	}
}
