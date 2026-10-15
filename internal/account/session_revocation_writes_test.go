package account

import (
	"errors"
	"testing"
)

func TestSessionRevocationsSkipAlreadyRevokedRows(t *testing.T) {
	for _, operation := range []string{"logout_all", "logout_current", "refresh_replay"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			first := complete(t, db, Identity{"email", "revoke-write@example.test"})
			other := complete(t, db, Identity{"email", "revoke-other@example.test"})
			principal, err := db.Authenticate(ctx, first.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `INSERT INTO auth_sessions(id,user_id,access_hash,refresh_hash,access_expires,expires_at,revoked)
SELECT 'revoke-extra-'||n,$1,'access-'||n,'refresh-'||n,now()+interval '15 minutes',
 CASE WHEN n=4 THEN now()-interval '1 day' ELSE now()+interval '30 days' END,n<=2
FROM generate_series(1,4) n`, first.User.ID); err != nil {
				t.Fatal(err)
			}
			access := first.AccessToken
			if operation == "refresh_replay" {
				next, err := db.Refresh(ctx, first.RefreshToken)
				if err != nil {
					t.Fatal(err)
				}
				access = next.AccessToken
				if _, err := db.pool.Exec(ctx, `UPDATE auth_used_refresh SET used_at=clock_timestamp()-interval '1 day' WHERE hash=$1`, hash(first.RefreshToken)); err != nil {
					t.Fatal(err)
				}
			}
			// 对比实际行版本，兼容独立 UPDATE 和数据修改 CTE，并捕获重复写入。
			versions := func() map[string]string {
				t.Helper()
				rows, err := db.pool.Query(ctx, `SELECT id,xmin::text FROM auth_sessions`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				result := map[string]string{}
				for rows.Next() {
					var id, version string
					if err := rows.Scan(&id, &version); err != nil {
						t.Fatal(err)
					}
					result[id] = version
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				return result
			}
			for attempt := range 2 {
				before := versions()
				if operation == "refresh_replay" {
					if _, err := db.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrInvalid) {
						t.Fatal("重放令牌未被拒绝", err)
					}
				} else if err := db.Logout(ctx, principal, operation == "logout_all"); err != nil {
					t.Fatal(err)
				}
				want := 0
				if attempt == 0 {
					want = 1
					if operation == "logout_all" {
						want = 3
					}
				}
				updated := 0
				for id, version := range versions() {
					if before[id] != version {
						updated++
					}
				}
				if updated != want {
					t.Errorf("第 %d 次撤销更新了 %d 行，期望 %d 行", attempt+1, updated, want)
				}
			}
			if _, err := db.Authenticate(ctx, access); !errors.Is(err, ErrInvalid) {
				t.Fatal("撤销后访问令牌仍有效", err)
			}
			if _, err := db.Authenticate(ctx, other.AccessToken); err != nil {
				t.Fatal("撤销影响了其他用户", err)
			}
			wantRemaining := 2
			if operation == "logout_all" {
				wantRemaining = 0
			}
			if n := clipboardTestCount(t, db, `SELECT count(*) FROM auth_sessions WHERE user_id=$1 AND NOT revoked`, first.User.ID); n != wantRemaining {
				t.Fatal("撤销范围不正确", n)
			}
		})
	}
}
