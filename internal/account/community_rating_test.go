package account

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCommunityRatingSharedBehavior(t *testing.T) {
	resource := savedKind{base: "/v1/community/resources", table: "community_resources", column: "resource_id", notFound: "resource_not_found", ownRatingCode: "save_before_rating_or_own_resource", insert: func(t *testing.T, db *Store, id, owner, name string) {
		t.Helper()
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'reply',$3,'{"prompt":"合成回复"}')`, id, owner, name); err != nil {
			t.Fatal(err)
		}
	}}
	for _, kind := range []struct {
		fixture savedKind
		ratings string
	}{{savedSkins, "community_skin_ratings"}, {savedCandidateSkins, "community_candidate_skin_ratings"}, {savedPlugins, "community_plugin_ratings"}, {resource, "community_resource_ratings"}} {
		t.Run(kind.fixture.table, func(t *testing.T) {
			k := kind.fixture
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "rating-owner@example.test"})
			reader := complete(t, db, Identity{"email", "rating-reader@example.test"})
			mux := http.NewServeMux()
			Mount(mux, &Service{store: db})
			id, removed, missing := savedID(40), savedID(41), savedID(42)
			k.insert(t, db, id, owner.User.ID, "公开作品")
			k.insert(t, db, removed, owner.User.ID, "下架作品")
			if _, err := db.pool.Exec(t.Context(), `UPDATE `+k.table+` SET moderation='removed' WHERE id=$1`, removed); err != nil {
				t.Fatal(err)
			}
			call := func(id, body, token string, status int, code string) {
				t.Helper()
				w := apiRequest(t, mux, "PUT", k.base+"/"+id+"/rating", body, token, status)
				if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
					t.Fatal(w.Body.String())
				}
				if status == 200 {
					var got struct {
						Stars int `json:"stars"`
					}
					if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Stars < 1 || got.Stars > 5 {
						t.Fatal(w.Body.String())
					}
				}
			}
			call(id, `{`, "", 401, "")
			for _, body := range []string{`{}`, `{"stars":0}`, `{"stars":6}`, `{"stars":-1}`} {
				call(missing, body, reader.AccessToken, 400, "invalid_rating")
			}
			call(id, `{"stars":4}`, owner.AccessToken, 403, k.ownRatingCode)
			for _, unavailable := range []string{missing, removed} {
				call(unavailable, `{"stars":4}`, reader.AccessToken, 404, k.notFound)
				call(unavailable, `{"stars":4}`, owner.AccessToken, 404, k.notFound)
			}
			call(id, `{"stars":1}`, reader.AccessToken, 200, "")
			call(id, `{"stars":5}`, reader.AccessToken, 200, "")
			var count, stars int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*),max(stars) FROM `+kind.ratings+` WHERE `+k.column+`=$1 AND user_id=$2`, id, reader.User.ID).Scan(&count, &stars); err != nil || count != 1 || stars != 5 {
				t.Fatal("评分未提交或更新重复", count, stars, err)
			}
			if k.table == "community_candidate_skins" {
				if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET visibility='private' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				call(id, `{"stars":4}`, reader.AccessToken, 404, k.notFound)
				call(id, `{"stars":4}`, owner.AccessToken, 404, k.notFound)
			}
		})
	}
}
