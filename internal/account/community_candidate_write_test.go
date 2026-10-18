package account

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCommitCandidateSkin(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "candidate-commit@example.test"})
	for index, phase := range []string{"成功", "读取失败", "提交失败"} {
		t.Run(phase, func(t *testing.T) {
			id := fmt.Sprintf("a0334455-1234-4234-8234-%012d", index)
			insertCandidateSkin(t, db, id, owner.User.ID, "写入前")
			tx, err := db.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), `UPDATE community_candidate_skins SET name='写入后' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if phase == "提交失败" {
				if _, err := tx.Exec(t.Context(), `CREATE TEMP TABLE candidate_commit_failure (value int UNIQUE DEFERRABLE INITIALLY DEFERRED) ON COMMIT DROP;
INSERT INTO candidate_commit_failure VALUES (1),(1)`); err != nil {
					t.Fatal(err)
				}
			}
			readID := id
			if phase == "读取失败" {
				readID = "a0334455-1234-4234-8234-ffffffffffff"
			}
			item, err := commitCandidateSkin(t.Context(), tx, owner.User.ID, readID)
			wantName := "写入前"
			switch phase {
			case "成功":
				if err != nil || item.ID != id || item.Name != "写入后" || !item.Owned {
					t.Fatalf("提交结果不符：%+v %v", item, err)
				}
				wantName = "写入后"
			case "读取失败":
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("读取失败未保留原错误：%v", err)
				}
				var pending string
				if err := tx.QueryRow(t.Context(), `SELECT name FROM community_candidate_skins WHERE id=$1`, id).Scan(&pending); err != nil || pending != "写入后" {
					t.Fatalf("读取失败不应提交或关闭事务：%q %v", pending, err)
				}
				if err := tx.Rollback(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "提交失败":
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
					t.Fatalf("提交失败未保留原错误：%v", err)
				}
			}
			if _, err := tx.Exec(t.Context(), `SELECT 1`); !errors.Is(err, pgx.ErrTxClosed) {
				t.Fatalf("事务尚未结束：%v", err)
			}
			var stored string
			if err := db.pool.QueryRow(t.Context(), `SELECT name FROM community_candidate_skins WHERE id=$1`, id).Scan(&stored); err != nil || stored != wantName {
				t.Fatalf("数据库提交状态不符：%q %v", stored, err)
			}
		})
	}
}
