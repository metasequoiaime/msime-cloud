package account

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCandidateWindowsTrailingDotPathsRejected(t *testing.T) {
	for _, path := range []string{"images./preview.png", "a/b.../preview.png", ".../preview.png"} {
		t.Run(path, func(t *testing.T) {
			manifest, files := candidatePathFixture(t, path, "assets/deco.jpg")
			if code := candidatePipeline(manifest, files); code != "candidate_skin_file_path" {
				t.Fatal("Windows 会规范化的目录尾点未被拒绝", code)
			}
		})
	}
	for _, path := range []string{"conifer.png", "con_v1.png", "CON.png", "NUL.png", "AUX/preview.png", "COM1/preview.png", "LPT9/preview.png", "auxiliary/preview.png", "com0/preview.png", "COM10/preview.png", "lpt0/preview.png", "LPT10/preview.png", ".hidden/preview.png", "images.v1/preview.png"} {
		t.Run("合法/"+path, func(t *testing.T) {
			manifest, files := candidatePathFixture(t, path, "assets/deco.jpg")
			if code := candidatePipeline(manifest, files); code != "" {
				t.Fatal("合法相似名称被误拒绝", code)
			}
		})
	}
}

func TestCandidateWindowsTrailingDotPathsRejectedByWriteEndpoints(t *testing.T) {
	for _, path := range []string{"images./preview.png", "a/b.../preview.png", ".../preview.png"} {
		for _, action := range []string{"发布", "替换", "后台创建", "后台编辑"} {
			t.Run(action+"/"+path, func(t *testing.T) {
				db := testStore(t)
				owner := complete(t, db, Identity{"email", "candidate-windows@example.test"})
				a := &Service{store: db}
				mux := http.NewServeMux()
				Mount(mux, a)
				const id = "ca334455-1234-4234-8234-123456789abc"
				var token any
				if action == "替换" || action == "后台编辑" {
					manifest, files := candidateFixture(t, "shared")
					apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, id, "有效包", manifest, files), owner.AccessToken, 201)
					if action == "后台编辑" {
						token = adminEditDetail(t, a, "candidate-skins", id)["edit_token"]
					}
				}
				var audits int
				if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit`).Scan(&audits); err != nil {
					t.Fatal(err)
				}
				manifest, files := candidatePathFixture(t, path, "assets/deco.jpg")
				var w *httptest.ResponseRecorder
				switch action {
				case "发布":
					w = userDataRequest(mux, "POST", "/v1/community/candidate-skins", owner.AccessToken, candidatePublishBody(t, id, "不兼容路径", manifest, files))
				case "替换":
					w = userDataRequest(mux, "PUT", "/v1/community/candidate-skins/"+id, owner.AccessToken, candidateReplaceBody(t, "不兼容路径", manifest, files))
				default:
					adminAction := "create_candidate_skin"
					if action == "后台编辑" {
						adminAction = "update_candidate_skin"
					}
					w = contentEditCall(t, a, adminAction, "candidate-skins", id, map[string]any{
						"owner_id": owner.User.ID, "name": "不兼容路径", "moderation": "approved", "category": "other",
						"visibility": "public", "manifest": manifest, "files": files, "edit_token": token,
					})
				}
				if w.Code != 400 || !strings.Contains(w.Body.String(), `"candidate_skin_file_path"`) {
					t.Error("Windows 不兼容路径未被拒绝", w.Code, w.Body.String())
				}
				var rows, afterAudits int
				if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM community_candidate_skins WHERE id=$1 AND name='不兼容路径'`, id).Scan(&rows); err != nil || rows != 0 {
					t.Error("无效包被持久化", rows, err)
				}
				if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit`).Scan(&afterAudits); err != nil || afterAudits != audits {
					t.Error("失败操作生成了成功审计", afterAudits, audits, err)
				}
				if action == "替换" || action == "后台编辑" {
					w = userDataRequest(mux, "POST", "/v1/community/candidate-skins/"+id+"/download", owner.AccessToken, "{}")
					if w.Code != 200 || !strings.Contains(w.Body.String(), `"preview.png"`) || strings.Contains(w.Body.String(), path) {
						t.Error("无效编辑破坏了原作品", w.Code, w.Body.String())
					}
				}
			})
		}
	}
}
