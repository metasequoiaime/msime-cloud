package account

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCandidateOverlongFilenamesRejectedByWriteEndpoints(t *testing.T) {
	for _, action := range []string{"发布", "替换", "后台创建"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "candidate-filename@example.test"})
			a := &Service{store: db}
			mux := http.NewServeMux()
			Mount(mux, a)
			id := "ca334455-1234-4234-8234-123456789abc"
			preview := strings.Repeat("a", 252) + ".png"
			manifest, files := candidatePathFixture(t, preview, "assets/deco.jpg")
			var w *httptest.ResponseRecorder
			switch action {
			case "发布":
				w = userDataRequest(mux, "POST", "/v1/community/candidate-skins", owner.AccessToken, candidatePublishBody(t, id, "超长文件名", manifest, files))
			case "替换":
				validManifest, validFiles := candidateFixture(t, "shared")
				w = userDataRequest(mux, "POST", "/v1/community/candidate-skins", owner.AccessToken, candidatePublishBody(t, id, "有效包", validManifest, validFiles))
				if w.Code != 201 {
					t.Fatal("创建原作品失败", w.Code, w.Body.String())
				}
				w = userDataRequest(mux, "PUT", "/v1/community/candidate-skins/"+id, owner.AccessToken, candidateReplaceBody(t, "超长文件名", manifest, files))
			case "后台创建":
				w = contentEditCall(t, a, "create_candidate_skin", "candidate-skins", id, map[string]any{
					"owner_id": owner.User.ID, "name": "超长文件名", "moderation": "approved", "category": "other",
					"visibility": "public", "manifest": manifest, "files": files,
				})
			}
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"candidate_skin_file_path"`) {
				t.Errorf("超过文件系统名称上限的包未被拒绝：%d %s", w.Code, w.Body.String())
			}
			var rows int
			if err := db.pool.QueryRow(t.Context(), "SELECT count(*) FROM community_candidate_skins WHERE id=$1 AND name='超长文件名'", id).Scan(&rows); err != nil || rows != 0 {
				t.Fatal("无效包被持久化", rows, err)
			}
			if action == "替换" {
				w = userDataRequest(mux, "POST", "/v1/community/candidate-skins/"+id+"/download", owner.AccessToken, "{}")
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"preview.png"`) || strings.Contains(w.Body.String(), preview) {
					t.Fatal("无效替换破坏了原作品", w.Code, w.Body.String())
				}
			}
		})
	}
}
