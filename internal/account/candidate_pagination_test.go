package account

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCandidatePaginationLimitsStatisticsQueries(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "candidate-page-owner@example.test"})
	reader := complete(t, db, Identity{"email", "candidate-page-reader@example.test"})
	other := complete(t, db, Identity{"email", "candidate-page-other@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skins
 (id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,visibility,moderation,category,created_at,updated_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),$1,'page-'||n,
 CASE WHEN n>65 THEN 'different' ELSE 'page example' END,'1',CASE WHEN n%7=0 THEN '' ELSE 'example assets' END,
 'x'::bytea,'preview.png',repeat('a',64),CASE WHEN n%7=0 THEN 'private' ELSE 'public' END,
 CASE WHEN n%10=0 THEN 'removed' ELSE 'approved' END,CASE WHEN n BETWEEN 61 AND 65 THEN 'nature' ELSE 'other' END,
 '2026-01-01'::timestamptz+((n+1)/2)*interval '1 second','2026-02-01'::timestamptz
 FROM generate_series(1,70) n`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skins
 (id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,visibility)
 VALUES('00000000-0000-4000-8000-000000000071',$1,'page-other','page example','1','','x'::bytea,'preview.png',repeat('b',64),'private')`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_files(skin_id,path,bytes)
 SELECT id,path,bytes FROM community_candidate_skins CROSS JOIN (VALUES ('preview.png',decode('000102','hex')),('key.png',decode('0304050607','hex'))) f(path,bytes)
 WHERE right(id,12)::int%9<>0`); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{owner.User.ID, reader.User.ID} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_saves(skin_id,user_id,created_at)
 SELECT id,$1,'2026-03-01'::timestamptz-((right(id,12)::int+1)/2)*interval '1 second' FROM community_candidate_skins`, user); err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_downloads(skin_id,user_id) SELECT id,$1 FROM community_candidate_skins`, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, rating := range []struct {
		user  string
		stars int
	}{{owner.User.ID, 3}, {reader.User.ID, 5}} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_ratings(skin_id,user_id,stars) SELECT id,$1,$2 FROM community_candidate_skins`, rating.user, rating.stars); err != nil {
			t.Fatal(err)
		}
	}
	cfg := db.pool.Config()
	trace := &communityPaginationTrace{table: "community_candidate_skins", queries: make(chan communityPageQuery, 8), fixedJoinOrder: true}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, tc := range []struct {
		name, scope, token string
		owned, sync, saved bool
		myRating           int
	}{
		{"public", "", "", false, false, false, 0},
		{"public-owner-sync", "", owner.AccessToken, true, true, true, 3},
		{"mine-legacy", "mine", owner.AccessToken, true, false, true, 3},
		{"mine-sync", "mine", owner.AccessToken, true, true, true, 3},
		{"saved-owner-legacy", "saved", owner.AccessToken, true, false, true, 3},
		{"saved-owner-sync", "saved", owner.AccessToken, true, true, true, 3},
		{"saved-reader-sync", "saved", reader.AccessToken, false, true, true, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var numbers []int
			for n := 1; n <= 60; n++ {
				if (n%10 != 0 || tc.owned) && (n%7 != 0 || (tc.owned && tc.scope != "" && tc.sync)) {
					numbers = append(numbers, n)
				}
			}
			if tc.scope != "saved" {
				// 发布时间按两条一组倒序，同一组内仍按 ID 升序。
				var ordered []int
				for group := 30; group > 0; group-- {
					for _, n := range numbers {
						if (n+1)/2 == group {
							ordered = append(ordered, n)
						}
					}
				}
				numbers = ordered
			}
			for _, offset := range []int{20, 40, 1000} {
				fields := "saved,moderation"
				if tc.sync {
					fields += ",sync"
				}
				r := httptest.NewRequest("GET", fmt.Sprintf("/v1/community/candidate-skins?q=page&category=other&include=category&fields=%s&scope=%s&offset=%d", fields, tc.scope, offset), nil)
				r.Header.Set("Authorization", "Bearer "+tc.token)
				w := httptest.NewRecorder()
				a.communityCandidateList(w, r)
				var body struct {
					Skins   []CommunityCandidateSkin `json:"skins"`
					HasMore bool                     `json:"has_more"`
				}
				want := numbers[min(offset, len(numbers)):min(offset+20, len(numbers))]
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Skins == nil || len(body.Skins) != len(want) || body.HasMore != (offset+20 < len(numbers)) {
					t.Fatal("分页长度或可见性错误：", offset, w.Code, w.Body.String())
				}
				for i, item := range body.Skins {
					n := want[i]
					if item.ID != fmt.Sprintf("00000000-0000-4000-8000-%012d", n) || item.Downloads != 2 || item.RatingCount != 2 || item.RatingAverage != 4 || item.MyRating != tc.myRating || item.Owned != tc.owned || item.Category != "other" || item.Saved == nil || *item.Saved != tc.saved || item.Saves == nil || *item.Saves != 2 {
						t.Fatal("分页顺序或个人字段错误：", offset, i, item)
					}
					files, size := 2, int64(8)
					if n%9 == 0 {
						files, size = 0, 0
					}
					if item.FileCount != files || item.Size != size {
						t.Fatal("文件统计错误：", item)
					}
					moderation, visibility, digest := "", "", ""
					if tc.owned {
						moderation = "approved"
						if n%10 == 0 {
							moderation = "removed"
						}
					}
					if tc.sync {
						visibility = "public"
						if n%7 == 0 {
							visibility = "private"
						}
						if tc.owned {
							digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
						}
					}
					if item.Moderation != moderation || item.Visibility != visibility || item.RequestSHA256 != digest || item.UpdatedAt.IsZero() == tc.sync {
						t.Fatal("审核或同步字段可见性错误：", item)
					}
				}
				assertCommunityPageWork(t, db, trace, "community_candidate_skin_files", "community_candidate_skin_downloads", "community_candidate_skin_ratings", "community_candidate_skin_saves")
			}
		})
	}
}
