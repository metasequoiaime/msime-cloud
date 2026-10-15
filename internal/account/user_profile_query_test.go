package account

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestUserProfileSelectsLatestValidGoogleFields(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "profile-query-owner@example.test"}).User
	other := complete(t, db, Identity{"email", "profile-query-other@example.test"}).User
	if _, err := db.pool.Exec(ctx, `UPDATE auth_identities SET email='email-provider@example.test',email_verified=true,
 picture='https://example.test/email-provider.jpg',updated_at=now()+interval '1 day' WHERE user_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_identities(provider,subject,user_id,email,email_verified,picture,updated_at)
 VALUES('google','profile-other-google',$1,'other@example.test',true,'https://example.test/other.jpg',now()+interval '1 day')`, other.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent, latest := old.Add(time.Hour), old.Add(2*time.Hour)
	type profile struct {
		email, picture string
		verified       bool
		updated        *time.Time
	}
	for _, tc := range []struct {
		name           string
		profiles       []profile
		email, picture string
	}{
		{name: "没有 Google 身份"},
		{"单个已验证身份", []profile{{"one@example.test", "https://example.test/one.jpg", true, &old}}, "one@example.test", "https://example.test/one.jpg"},
		{"未验证邮箱仍可提供头像", []profile{{"hidden@example.test", "https://example.test/unverified.jpg", false, &latest}}, "", "https://example.test/unverified.jpg"},
		{"空邮箱和空头像", []profile{{"", "", true, &latest}}, "", ""},
		{"字段独立选取最新有效值", []profile{
			{"old@example.test", "https://example.test/old.jpg", true, &old},
			{"recent@example.test", "", true, &recent},
			{"hidden@example.test", "https://example.test/latest.jpg", false, &latest},
		}, "recent@example.test", "https://example.test/latest.jpg"},
		{"空字段回退到较早身份", []profile{
			{"old@example.test", "https://example.test/old.jpg", true, &old},
			{"", "", true, &latest},
		}, "old@example.test", "https://example.test/old.jpg"},
		{"没有更新时间也可作为回退值", []profile{{"null@example.test", "https://example.test/null.jpg", true, nil}}, "null@example.test", "https://example.test/null.jpg"},
		{"已知更新时间优先于空时间", []profile{
			{"null@example.test", "https://example.test/null.jpg", true, nil},
			{"old@example.test", "https://example.test/old.jpg", true, &old},
		}, "old@example.test", "https://example.test/old.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.pool.Exec(ctx, `DELETE FROM auth_identities WHERE user_id=$1 AND provider='google'`, owner.ID); err != nil {
				t.Fatal(err)
			}
			for i, p := range tc.profiles {
				if _, err := db.pool.Exec(ctx, `INSERT INTO auth_identities(provider,subject,user_id,email,email_verified,picture,updated_at)
 VALUES('google',$1,$2,$3,$4,$5,$6)`, "profile-query-"+strconv.Itoa(i), owner.ID, p.email, p.verified, p.picture, p.updated); err != nil {
					t.Fatal(err)
				}
			}
			var got User
			if err := scanUser(db.pool.QueryRow(ctx, userQuery, owner.ID), &got); err != nil {
				t.Fatal(err)
			}
			if got.Email != tc.email || got.googlePicture != tc.picture || got.ID != owner.ID || got.DisplayName != owner.DisplayName || !got.CreatedAt.Equal(owner.CreatedAt) || got.avatarKey != owner.avatarKey {
				t.Fatalf("用户资料不兼容：%+v，头像=%s，期望邮箱=%s，头像=%s", got, got.googlePicture, tc.email, tc.picture)
			}
		})
	}
	var missing User
	if err := scanUser(db.pool.QueryRow(ctx, userQuery, "missing-profile-user"), &missing); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("不存在的用户应保持无记录错误", missing, err)
	}
}

func TestUserProfileUsesOrderIndexAfterUpgrade(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "profile-index-owner@example.test"}).User.ID
	other := complete(t, db, Identity{"email", "profile-index-other@example.test"}).User.ID
	// 模拟已存在用户和 Google 身份、但还没有排序索引的旧数据库。
	if _, err := db.pool.Exec(ctx, `DROP INDEX IF EXISTS auth_identities_google_profile`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_identities(provider,subject,user_id,email,email_verified,picture,updated_at)
 SELECT 'google','profile-index-'||n,CASE WHEN n<=128 THEN $1 ELSE $2 END,
   CASE WHEN n=2 THEN '' ELSE 'profile-'||n||'@example.test' END,n<>1,
   CASE WHEN n=1 THEN '' ELSE 'https://example.test/profile-'||n||'.jpg' END,
   CASE WHEN n=128 THEN NULL ELSE now()-make_interval(secs=>n) END
 FROM generate_series(1,4096) n`, owner, other); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE auth_identities`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var got User
	if err := scanUser(db.pool.QueryRow(ctx, userQuery, owner), &got); err != nil || got.Email != "profile-3@example.test" || got.googlePicture != "https://example.test/profile-2.jpg" {
		t.Fatal("升级后资料字段选择错误", got, got.googlePicture, err)
	}
	var raw json.RawMessage
	if err := db.pool.QueryRow(ctx, `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+userQuery, owner).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Type     string `json:"Node Type"`
		Index    string `json:"Index Name"`
		Relation string `json:"Relation Name"`
		Rows     int    `json:"Actual Rows"`
		Filtered int    `json:"Rows Removed by Filter"`
		Plans    []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	var indexScans, sorts, visited int
	var visit func(node)
	visit = func(n node) {
		if n.Index == "auth_identities_google_profile" {
			indexScans++
		}
		if n.Type == "Sort" {
			sorts++
		}
		if n.Relation == "auth_identities" {
			visited += n.Rows + n.Filtered
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if indexScans != 2 || sorts != 0 || visited > 5 {
		t.Fatalf("资料读取未按排序索引提前停止：索引扫描 %d 次、排序 %d 次、访问 %d 行", indexScans, sorts, visited)
	}
}

func TestGoogleProfileIndexStartupMigration(t *testing.T) {
	db := testStore(t)
	other, _ := communityIndexStoreWithLeadingSchema(t, db)
	if err := other.Ready(t.Context()); err != nil {
		t.Fatal("前置空 schema 不应影响实际业务表的索引探测", err)
	}
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS auth_identities_google_profile`); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "google profile index") {
		t.Fatalf("缺少 Google 资料索引时未请求迁移：%v", err)
	}
	t.Setenv("GOOGLE_PROFILE_INDEX_TEST_DB", os.Getenv("MSIME_TEST_DATABASE_URL"))
	t.Setenv("GOOGLE_PROFILE_INDEX_TEST_PEPPER", strings.Repeat("p", 32))
	a, err := New(t.Context(), Config{Enabled: true, DatabaseEnv: "GOOGLE_PROFILE_INDEX_TEST_DB", PepperEnv: "GOOGLE_PROFILE_INDEX_TEST_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := other.Ready(t.Context()); err != nil {
		t.Fatal("启动后 Google 资料索引仍缺失", err)
	}
}
