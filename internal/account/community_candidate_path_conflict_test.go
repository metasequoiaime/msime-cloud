package account

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func candidatePathFixture(t *testing.T, preview, decoration string) (string, map[string][]byte) {
	t.Helper()
	manifest, files := candidateFixture(t, "shared")
	manifest = strings.Replace(manifest, "preview = 'preview.png'", "preview = '"+preview+"'", 1)
	manifest = strings.Replace(manifest, "image = 'assets/deco.jpg'", "image = '"+decoration+"'", 1)
	return manifest, map[string][]byte{preview: files["preview.png"], decoration: files["assets/deco.jpg"]}
}

func TestCandidatePackagePathsCanBeMaterialized(t *testing.T) {
	for _, tc := range []struct {
		name, preview, decoration string
		valid                     bool
	}{
		{"图片同时充当父目录", "preview.png", "preview.png/deco.jpg", false},
		{"忽略大小写的父目录", "z.png", "Z.PNG/deco.jpg", false},
		{"深层文件与目录冲突", "images/preview.png", "images/preview.png/nested/deco.jpg", false},
		{"清单同时充当父目录", "skin.toml/preview.png", "assets/deco.jpg", false},
		{"忽略大小写的清单目录", "SKIN.TOML/preview.png", "assets/deco.jpg", false},
		{"预览文件名超过文件系统上限", strings.Repeat("a", 252) + ".png", "assets/deco.jpg", false},
		{"装饰文件名超过文件系统上限", "preview.png", strings.Repeat("a", 252) + ".jpg", false},
		{"预览文件名恰好达到上限", strings.Repeat("a", 251) + ".png", "assets/deco.jpg", true},
		{"装饰文件名恰好达到上限", "preview.png", strings.Repeat("a", 251) + ".jpg", true},
		{"完整路径达到上限但各段合法", "a/" + strings.Repeat("b", 250) + ".png", "assets/deco.jpg", true},
		{"原有路径", "preview.png", "assets/deco.jpg", true},
		{"同一父目录", "images/preview.png", "images/deco.jpg", true},
		{"不同大小写的共享目录", "Images/preview.png", "images/deco.jpg", true},
		{"仅文本前缀相同", "preview.png", "preview.png-extra/deco.jpg", true},
		{"清单名称仅文本前缀相同", "skin.toml-images/preview.png", "assets/deco.jpg", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, files := candidatePathFixture(t, tc.preview, tc.decoration)
			want := "candidate_skin_file_path"
			if tc.valid {
				want = ""
			}
			if code := candidatePipeline(manifest, files); code != want {
				t.Fatalf("完整上传校验返回 %q，期望 %q", code, want)
			}
		})
	}
}

func TestCandidatePathConflictsRejectedByWriteEndpoints(t *testing.T) {
	for _, action := range []string{"发布", "替换", "后台创建"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "candidate-paths@example.test"})
			a := &Service{store: db}
			mux := http.NewServeMux()
			Mount(mux, a)
			id := "ca334455-1234-4234-8234-123456789abc"
			manifest, files := candidatePathFixture(t, "preview.png", "preview.png/deco.jpg")
			var w *httptest.ResponseRecorder
			switch action {
			case "发布":
				w = userDataRequest(mux, "POST", "/v1/community/candidate-skins", owner.AccessToken, candidatePublishBody(t, id, "路径冲突", manifest, files))
			case "替换":
				validManifest, validFiles := candidateFixture(t, "shared")
				w = userDataRequest(mux, "POST", "/v1/community/candidate-skins", owner.AccessToken, candidatePublishBody(t, id, "有效包", validManifest, validFiles))
				if w.Code != 201 {
					t.Fatal("创建原作品失败", w.Code, w.Body.String())
				}
				w = userDataRequest(mux, "PUT", "/v1/community/candidate-skins/"+id, owner.AccessToken, candidateReplaceBody(t, "路径冲突", manifest, files))
			case "后台创建":
				w = contentEditCall(t, a, "create_candidate_skin", "candidate-skins", id, map[string]any{
					"owner_id": owner.User.ID, "name": "路径冲突", "moderation": "approved", "category": "other",
					"visibility": "public", "manifest": manifest, "files": files,
				})
			}
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"candidate_skin_file_path"`) {
				t.Errorf("文件与目录冲突未被拒绝：%d %s", w.Code, w.Body.String())
			}
			var rows int
			if err := db.pool.QueryRow(t.Context(), "SELECT count(*) FROM community_candidate_skins WHERE id=$1 AND name='路径冲突'", id).Scan(&rows); err != nil || rows != 0 {
				t.Fatal("无效包被持久化", rows, err)
			}
			if action == "替换" {
				w = userDataRequest(mux, "POST", "/v1/community/candidate-skins/"+id+"/download", owner.AccessToken, "{}")
				if w.Code != 200 || !strings.Contains(w.Body.String(), "assets/deco.jpg") || strings.Contains(w.Body.String(), "preview.png/deco.jpg") {
					t.Fatal("无效替换破坏了原作品", w.Code, w.Body.String())
				}
			}
		})
	}
}
