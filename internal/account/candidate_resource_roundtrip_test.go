package account

import (
	"bytes"
	"context"
	"encoding/json"
	"image/gif"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func candidateAnimatedCanvasFixture(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	manifest, files := candidateCanvasFixture(t)
	delete(files, "pin.png")
	manifest = strings.ReplaceAll(manifest, "pin.png", "petals.apng")
	manifest += "\n[[candidate_window.decorations]]\nimage = 'stars.gif'\nwidth_dip = 8\nheight_dip = 8\n[[candidate_window.decorations]]\nimage = 'moon.webp'\nwidth_dip = 4\nheight_dip = 3\n"
	files["petals.apng"] = candidateAPNGFixture(t)
	files["stars.gif"] = candidateGIFFixture(t, 3)
	files["moon.webp"] = candidateWebPFixture(t, "animation")
	return manifest, files
}

func TestCommunityCandidateAnimatedResourcesRoundTrip(t *testing.T) {
	db := testStore(t)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	owner := complete(t, db, Identity{"email", "animated-canvas@example.test"})
	const root = "/v1/community/candidate-skins"
	const option = "resources=canvas-v1"
	const id = "ce334455-1234-4234-8234-000000000033"
	manifest, files := candidateAnimatedCanvasFixture(t)
	body := candidatePublishBody(t, id, "合成动画画布", manifest, files)
	apiRequest(t, mux, "POST", root, body, owner.AccessToken, 400)
	w := apiRequest(t, mux, "POST", root+"?"+option, body, owner.AccessToken, 201)
	if w.Header().Get(candidateResourcesHeader) != candidateResourcesVersion {
		t.Fatal("上传未声明服务端能力")
	}
	checkList := func(path, token string, want int) {
		t.Helper()
		w := apiRequest(t, mux, "GET", path, "", token, 200)
		var result struct {
			Skins []json.RawMessage `json:"skins"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Skins) != want {
			t.Fatalf("%s: %s %v", path, w.Body.String(), err)
		}
	}
	checkList(root, "", 0)
	checkList(root+"?"+option, "", 1)
	checkList(root+"?scope=mine&fields=sync", owner.AccessToken, 0)
	checkList(root+"?scope=mine&fields=sync&"+option, owner.AccessToken, 1)
	checkList(root+"/sync", owner.AccessToken, 0)
	checkList(root+"/sync?"+option, owner.AccessToken, 1)
	apiRequest(t, mux, "GET", root+"/"+id, "", "", 404)
	apiRequest(t, mux, "GET", root+"/"+id+"?"+option, "", "", 200)
	for _, token := range []string{"", owner.AccessToken} {
		apiRequest(t, mux, "POST", root+"/"+id+"/download", "", token, 404)
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("拒绝请求留下下载记录", count, err)
	}
	response := apiRequest(t, mux, "POST", root+"/"+id+"/download?"+option, "", "", 200)
	var result struct {
		Manifest string            `json:"manifest"`
		Files    map[string][]byte `json:"files"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Manifest != manifest || len(result.Files) != 5 {
		t.Fatal("下载丢失清单或资源", err)
	}
	if code := validCandidateResourceFiles(result.Manifest, result.Files, true); code != "" {
		t.Fatal(code)
	}
	if _, code := validCandidatePackage(result.Manifest, result.Files, true); code != "" {
		t.Fatal(code)
	}
	a, code := parseCandidateAPNG(result.Files["petals.apng"])
	if code != "" || a.images != 3 || !a.decode() {
		t.Fatal("APNG 丢失动画", code)
	}
	g, err := gif.DecodeAll(bytes.NewReader(result.Files["stars.gif"]))
	if err != nil || len(g.Image) != 3 || g.LoopCount != 3 {
		t.Fatal("GIF 丢失动画", err)
	}
	web, code := parseCandidateWebP(result.Files["moon.webp"])
	if code != "" || web.images != 2 || !web.decode() {
		t.Fatal("WebP 丢失动画", code)
	}
	// 预览继续使用所有旧客户端可解码的 PNG/JPEG 响应。
	apiRequest(t, mux, "GET", root+"/"+id+"/preview", "", "", 200)
	replacement := candidateReplaceBody(t, "合成动画画布二", manifest, files)
	apiRequest(t, mux, "PUT", root+"/"+id, replacement, owner.AccessToken, 400)
	apiRequest(t, mux, "PUT", root+"/"+id+"?"+option, replacement, owner.AccessToken, 200)
	// 替换成旧格式后，目录立即恢复可见，无需更新第二份能力字段。
	plainManifest, plainFiles := candidateCanvasFixture(t)
	apiRequest(t, mux, "PUT", root+"/"+id, candidateReplaceBody(t, "合成静态画布", plainManifest, plainFiles), owner.AccessToken, 200)
	checkList(root, "", 1)
	for _, path := range []string{root, root + "/sync", root + "/" + id, root + "/" + id + "/download"} {
		method := "GET"
		if path == root+"/"+id+"/download" {
			method = "POST"
		}
		apiRequest(t, mux, method, path+"?resources=unknown", "", owner.AccessToken, 400)
	}
}

func TestCommunityCandidateResourceConstraintUpgrade(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `ALTER TABLE community_candidate_skin_files DROP CONSTRAINT community_candidate_skin_files_path_check, ADD CONSTRAINT community_candidate_skin_files_path_check CHECK(path ~ '\.(png|jpe?g)$')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Migrate(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := db.Ready(t.Context()); err == nil {
		t.Fatal("旧社区图片约束未触发迁移")
	}
	for range 2 {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// 固定提交的上游素材只用于本机验收；仓库回归使用上面的合成动画。
func TestCandidateUpstreamSakuraNightResources(t *testing.T) {
	root := os.Getenv("MSIME_WINDOWS_SKINS_DIR")
	if root == "" {
		t.Skip("未指定固定提交的 msime-windows 皮肤目录")
	}
	root = filepath.Join(root, "sakura-night")
	manifestBytes, err := os.ReadFile(filepath.Join(root, "skin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := "preview = 'assets/panel.png'\n" + string(manifestBytes)
	files := map[string][]byte{}
	for _, path := range []string{"assets/panel.png", "assets/moon.png", "assets/petals.apng", "assets/stars.gif"} {
		files[path], err = os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
	}
	input := candidateUpload{Manifest: manifest, Files: files, Name: "合成验收标题", extendedResources: true}
	pkg, _, code := input.prepare(true)
	if code != "" {
		t.Fatal("上游夜樱清单或资源被拒绝", code)
	}
	clean, code := sanitizeCandidateImages(files, pkg.Preview)
	if code != "" || len(clean) != 4 {
		t.Fatal("上游夜樱图片处理失败", code)
	}
	a, code := parseCandidateAPNG(clean["assets/petals.apng"])
	if code != "" || a.images != 24 || !a.decode() {
		t.Fatal("上游 24 帧 APNG 丢失", code)
	}
	g, err := gif.DecodeAll(bytes.NewReader(clean["assets/stars.gif"]))
	if err != nil || len(g.Image) != 8 || g.Delay[0] != 15 {
		t.Fatal("上游 8 帧 GIF 丢失", err)
	}
}
