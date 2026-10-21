package account

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestAdminCandidateSkinImage(t *testing.T) {
	db, a, owner, _, call := moderationFixture(t)
	id := "ad334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, id, owner.User.ID, "合成候选窗皮肤")
	image := candidatePNG(t, 12, 8)
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET visibility='private',moderation='removed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_files(skin_id,path,bytes) VALUES($1,'assets/background.png',$2)`, id, image); err != nil {
		t.Fatal(err)
	}
	path := "/api/candidate-skins/" + id + "/image?path=assets%2Fbackground.png"
	for _, role := range []string{"readonly", "reviewer", RoleMaintainer} {
		r := jsonRequest("GET", path, "", "")
		w := httptest.NewRecorder()
		a.AdminHTTP(w, r.WithContext(WithAdminAccess(r.Context(), AdminAccess{Actor: "test:reader@example.test", Role: role})))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), image) || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(role, w.Code, w.Header())
		}
		if w.Result().Header.Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
			t.Fatal(role, w.Header())
		}
	}
	for suffix, status := range map[string]int{
		id + "/image":                                     400,
		id + "/image?path=../background.png":              400,
		id + "/image?path=https://example.test/image.png": 400,
		id + "/image?path=skin.toml":                      400,
		id + "/image?path=assets/missing.png":             404,
		"missing/image?path=preview.png":                  404,
		"a/b/image?path=preview.png":                      400,
	} {
		if w := call("GET", "/api/candidate-skins/"+suffix, ""); w.Code != status {
			t.Fatal(suffix, w.Code, w.Body.String())
		}
	}
}
