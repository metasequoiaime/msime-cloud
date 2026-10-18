package account

import (
	"errors"
	"testing"
)

func TestCandidateWriteRateSharedBudgetAndTransaction(t *testing.T) {
	db := testStore(t)
	for _, tc := range []struct {
		name, visibility, scope string
		official                bool
		limit                   int
	}{
		{"普通公开", "public", "candidate-publish", false, candidatePublishesPerHour},
		{"普通私有", "private", "candidate-library", false, candidateLibraryWritesPerHour},
		{"缺省可见性", "", "candidate-library", false, candidateLibraryWritesPerHour},
		{"官方公开", "public", "candidate-publish", true, candidateOfficialWritesPerHour},
		{"官方私有", "private", "candidate-library", true, candidateOfficialWritesPerHour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userID := hash(tc.name)
			a := &Service{store: db}
			if tc.official {
				a.config.Community.OfficialSkinPublishers = []string{userID}
			}
			key := tc.scope + ":" + hash(userID)
			if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,$2,now()+interval '1 hour')`, key, tc.limit-1); err != nil {
				t.Fatal(err)
			}
			if err := a.candidateWriteRate(t.Context(), userID, tc.visibility); err != nil {
				t.Fatalf("最后一次普通额度被拒绝：%v", err)
			}
			tx, err := db.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err := a.candidateWriteRateTx(t.Context(), tx, userID, tc.visibility); !errors.Is(err, ErrLimited) {
				t.Fatalf("事务调用没有共用已满的额度：%v", err)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&count); err != nil || count != tc.limit {
				t.Fatalf("回滚超限事务后计数不符：%d %v", count, err)
			}
			if _, err := db.pool.Exec(t.Context(), `UPDATE auth_rates SET count=0 WHERE key=$1`, key); err != nil {
				t.Fatal(err)
			}
			for _, commit := range []bool{true, false} {
				tx, err := db.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				if err := a.candidateWriteRateTx(t.Context(), tx, userID, tc.visibility); err != nil {
					t.Fatal(err)
				}
				if commit {
					err = tx.Commit(t.Context())
				} else {
					err = tx.Rollback(t.Context())
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&count); err != nil || count != 1 {
					t.Fatalf("事务提交或回滚的计数不符：提交=%v 计数=%d 错误=%v", commit, count, err)
				}
			}
		})
	}
}
