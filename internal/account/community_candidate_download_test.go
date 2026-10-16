package account

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCommunityCandidateDownloadWithoutLogin(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "public-download-owner@example.test"})
	reader := complete(t, db, Identity{"email", "public-download-reader@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	id := "ad334455-1234-4234-8234-123456789abc"
	path := "/v1/community/candidate-skins/" + id
	manifest, files := candidateFixture(t, "anonymous-download")
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, id, "合成皮肤", manifest, files), owner.AccessToken, 201)
	want := apiRequest(t, mux, "POST", path+"/download", "", reader.AccessToken, 200).Body.String()
	count := func(want int) {
		t.Helper()
		var got int
		if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=$1`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("downloads = %d, want %d: %v", got, want, err)
		}
	}
	for _, token := range []string{"", "expired-synthetic-token", reader.AccessToken} {
		got := apiRequest(t, mux, "POST", path+"/download", "", token, 200).Body.String()
		if got != want {
			t.Fatal("匿名下载改变了清单或图片")
		}
	}
	count(1)
	for _, state := range []struct{ visibility, moderation string }{{"private", "pending"}, {"public", "removed"}} {
		if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET visibility=$2,moderation=$3 WHERE id=$1`, id, state.visibility, state.moderation); err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{"", "expired-synthetic-token", reader.AccessToken} {
			w := apiRequest(t, mux, "POST", path+"/download", "", token, 404)
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["manifest"] != nil || body["files"] != nil {
				t.Fatal("拒绝响应泄漏皮肤包", err)
			}
		}
		count(1)
		apiRequest(t, mux, "POST", path+"/download", "", owner.AccessToken, 200)
		count(2)
		// 下一种状态只保留读者的原始下载，确保拒绝请求没有写入。
		if _, err := db.pool.Exec(t.Context(), `DELETE FROM community_candidate_skin_downloads WHERE skin_id=$1 AND user_id=$2`, id, owner.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/missing/download", "", "", 404)
}
