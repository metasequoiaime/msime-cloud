package account

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminPermissionsOwnersUseOneQuery(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	latest := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_members(email,role,enabled,created_at) VALUES
 ('owner-099@example.test','readonly',false,$1),('helper@example.test','reviewer',true,$1),('off@example.test','operator',false,$1)`, latest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_sessions(token_hash,subject,email,expires_at,last_seen_at) VALUES
 ('owner-active-1','owner','owner-099@example.test',now()+interval '1 day',$1::timestamptz-interval '2 hours'),
 ('owner-active-2','owner','owner-099@example.test',now()+interval '1 day',$1::timestamptz-interval '1 hour'),
 ('owner-expired','owner','owner-099@example.test',now()-interval '1 day',$1),
 ('owner-expired-only','owner','owner-050@example.test',now()-interval '1 day',$1),
 ('helper-active','helper','helper@example.test',now()+interval '1 day',$1)`, latest); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "admin_sessions", queries: make(chan communityPageQuery, 101)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, count := range []int{0, 1, 10, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			// 故意按邮箱倒序配置，所有者须保留配置顺序，普通成员仍按邮箱排序。
			owners := make([]string, count)
			for i := range owners {
				owners[i] = fmt.Sprintf("owner-%03d@example.test", 99-i)
			}
			a.admin.Owners = owners
			w := permissionsRequest(a, usersOwner, "GET", "")
			var result permissionsResponse
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			if queries := len(trace.queries); queries != 1 {
				t.Errorf("%d 位所有者的权限页发送了 %d 次会话查询，期望固定 1 次", count, queries)
			}
			for len(trace.queries) > 0 {
				<-trace.queries
			}
			emails := append([]string{}, owners...)
			for _, email := range []string{"helper@example.test", "off@example.test", "owner-099@example.test"} {
				if !slices.Contains(owners, email) {
					emails = append(emails, email)
				}
			}
			if len(result.Members) != len(emails) {
				t.Fatalf("成员数错误：%d，期望 %d", len(result.Members), len(emails))
			}
			for i, email := range emails {
				got := result.Members[i]
				owner := i < count
				if got.Email != email || got.Owner != owner {
					t.Fatalf("成员顺序或所有者标记错误：%+v，期望 %s/%v", got, email, owner)
				}
				if owner {
					if got.Role != RoleMaintainer || !got.Enabled || got.CreatedAt != nil {
						t.Fatalf("所有者被旧成员记录覆盖：%+v", got)
					}
				} else {
					role := map[string]string{"helper@example.test": "reviewer", "off@example.test": "operator", "owner-099@example.test": "readonly"}[email]
					if got.Role != role || got.Enabled != (email == "helper@example.test") || got.CreatedAt == nil || !got.CreatedAt.Equal(latest) {
						t.Fatalf("普通成员字段错误：%+v", got)
					}
				}
				sessions := int64(0)
				seen := false
				switch email {
				case "owner-099@example.test":
					sessions, seen = 2, true
				case "owner-050@example.test":
					seen = true
				case "helper@example.test":
					sessions, seen = 1, true
				}
				if got.Sessions != sessions || (got.LastSeenAt != nil) != seen || (seen && !got.LastSeenAt.Equal(latest)) {
					t.Fatalf("会话统计错误：%+v，期望 %d/%v", got, sessions, seen)
				}
			}
		})
	}
	// 空配置与空成员表必须返回空数组，不能返回 null。
	a.admin.Owners = nil
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_members`); err != nil {
		t.Fatal(err)
	}
	w := permissionsRequest(a, usersOwner, "GET", "")
	var result permissionsResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Members == nil || len(result.Members) != 0 {
		t.Fatal("空权限成员列表错误：", w.Code, w.Body.String())
	}
}
