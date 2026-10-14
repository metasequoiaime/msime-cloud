package account

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPluginPaginationLimitsInteractionQueries(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "plugin-page-owner@example.test"})
	reader := complete(t, db, Identity{"email", "plugin-page-reader@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256,moderation,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),$1,CASE WHEN n%3=0 THEN 'helpcode' ELSE 'sound' END,'page-'||n,
 CASE WHEN n>90 THEN 'different' ELSE 'page example' END,'1','MIT','id','archive',repeat('a',64),CASE WHEN n%10=0 THEN 'removed' ELSE 'approved' END,
 '2026-01-01'::timestamptz+((n+1)/2)*interval '1 second' FROM generate_series(1,95) n`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{owner.User.ID, reader.User.ID} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugin_saves(pack_id,user_id,created_at)
 SELECT id,$1,'2026-02-01'::timestamptz-((right(id,12)::int+1)/2)*interval '1 second' FROM community_plugins`, user); err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugin_downloads(pack_id,user_id) SELECT id,$1 FROM community_plugins`, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, rating := range []struct {
		user  string
		stars int
	}{{owner.User.ID, 3}, {reader.User.ID, 5}} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugin_ratings(pack_id,user_id,stars) SELECT id,$1,$2 FROM community_plugins`, rating.user, rating.stars); err != nil {
			t.Fatal(err)
		}
	}
	cfg := db.pool.Config()
	trace := &communityPaginationTrace{table: "community_plugins", queries: make(chan communityPageQuery, 16)}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, tc := range []struct {
		name, scope, token string
		owned, saved       bool
		rating             int
	}{
		{"public", "", "", false, false, 0},
		{"mine", "mine", owner.AccessToken, true, true, 3},
		{"saved-reader", "saved", reader.AccessToken, false, true, 5},
		{"saved-owner", "saved", owner.AccessToken, true, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, capability := range []struct {
				name, query              string
				helpCodes, onlyHelpCodes bool
			}{
				{"legacy", "", false, false},
				{"declared", "&kinds=helpcode,unknown", true, false},
				{"explicit", "&kind=helpcode&kinds=sound", true, true},
			} {
				t.Run(capability.name, func(t *testing.T) {
					var numbers []int
					for n := 1; n <= 90; n++ {
						helpCode := n%3 == 0
						if (!helpCode || capability.helpCodes) && (!capability.onlyHelpCodes || helpCode) && (tc.owned || n%10 != 0) {
							numbers = append(numbers, n)
						}
					}
					if tc.scope != "saved" {
						// 按两条一组的发布时间倒序，同时间按 ID 升序。
						var ordered []int
						for group := 45; group > 0; group-- {
							for _, n := range numbers {
								if (n+1)/2 == group {
									ordered = append(ordered, n)
								}
							}
						}
						numbers = ordered
					}
					for _, offset := range []int{20, 40, 1000} {
						r := httptest.NewRequest("GET", fmt.Sprintf("/v1/community/plugins?q=page&fields=saved,moderation&scope=%s&offset=%d%s", tc.scope, offset, capability.query), nil)
						r.Header.Set("Authorization", "Bearer "+tc.token)
						w := httptest.NewRecorder()
						a.communityPluginList(w, r)
						var body struct {
							Plugins []CommunityPlugin `json:"plugins"`
							HasMore bool              `json:"has_more"`
						}
						want := numbers[min(offset, len(numbers)):min(offset+20, len(numbers))]
						if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Plugins == nil || len(body.Plugins) != len(want) || body.HasMore != (offset+20 < len(numbers)) {
							t.Fatal("分页长度错误：", offset, w.Code, w.Body.String())
						}
						for i, item := range body.Plugins {
							n := want[i]
							kind, moderation := "sound", ""
							if n%3 == 0 {
								kind = "helpcode"
							}
							if tc.owned {
								moderation = "approved"
								if n%10 == 0 {
									moderation = "removed"
								}
							}
							if item.ID != fmt.Sprintf("00000000-0000-4000-8000-%012d", n) || item.Kind != kind || item.Downloads != 2 || item.RatingCount != 2 || item.RatingAverage != 4 || item.MyRating != tc.rating || item.Owned != tc.owned || item.Moderation != moderation || item.Saved == nil || *item.Saved != tc.saved || item.Saves == nil || *item.Saves != 2 || item.Size != 7 || len(item.SHA256) != 64 {
								t.Fatal("分页顺序或条目字段错误：", offset, i, item)
							}
						}
						assertCommunityPageWork(t, db, trace, "community_plugin_downloads", "community_plugin_ratings", "community_plugin_saves")
					}
				})
			}
		})
	}
}
