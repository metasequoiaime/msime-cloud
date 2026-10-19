package account

import (
	"net/http"
	"strings"
	"testing"
)

func TestCommunityDeleteOwnershipAndResult(t *testing.T) {
	resource := savedKind{base: "/v1/community/resources", table: "community_resources", notFound: "resource_not_found", insert: func(t *testing.T, db *Store, id, owner, name string) {
		t.Helper()
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'reply',$3,'{"prompt":"合成回复"}')`, id, owner, name); err != nil {
			t.Fatal(err)
		}
	}}
	for _, kind := range []savedKind{savedSkins, savedCandidateSkins, savedPlugins, resource} {
		t.Run(kind.table, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "delete-owner@example.test"})
			other := complete(t, db, Identity{"email", "delete-other@example.test"})
			mux := http.NewServeMux()
			Mount(mux, &Service{store: db})
			id, removed, missing := savedID(60), savedID(61), savedID(62)
			kind.insert(t, db, id, owner.User.ID, "公开作品")
			kind.insert(t, db, removed, owner.User.ID, "下架作品")
			if _, err := db.pool.Exec(t.Context(), `UPDATE `+kind.table+` SET moderation='removed' WHERE id=$1`, removed); err != nil {
				t.Fatal(err)
			}
			call := func(id, token string, status int, code string) {
				t.Helper()
				w := apiRequest(t, mux, http.MethodDelete, kind.base+"/"+id, "", token, status)
				if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
					t.Fatal(w.Body.String())
				}
				if status == http.StatusOK && w.Body.String() != "{\"deleted\":true}\n" {
					t.Fatal(w.Body.String())
				}
			}
			call(id, "", http.StatusUnauthorized, "")
			call(id, other.AccessToken, http.StatusNotFound, kind.notFound)
			call(missing, owner.AccessToken, http.StatusNotFound, kind.notFound)
			var count int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+kind.table+` WHERE owner_id=$1`, owner.User.ID).Scan(&count); err != nil || count != 2 {
				t.Fatal("拒绝的删除改变了作者作品", count, err)
			}
			for _, target := range []string{id, removed} {
				call(target, owner.AccessToken, http.StatusOK, "")
				call(target, owner.AccessToken, http.StatusNotFound, kind.notFound)
			}
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+kind.table+` WHERE owner_id=$1`, owner.User.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("作者删除未提交", count, err)
			}
		})
	}
}
