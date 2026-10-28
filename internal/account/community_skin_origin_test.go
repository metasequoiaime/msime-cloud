package account

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/bits"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"golang.org/x/image/draw"
)

// originTestArtwork 画一张确定性的合成插画：渐变底色上叠十几个随机圆，结构足够丰富，像真实插画一样让 dHash 的每一位都有明确的亮度差。
func originTestArtwork(seed uint64, size int) *image.RGBA {
	random := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	top, bottom := random.IntN(256), random.IntN(256)
	for y := range size {
		shade := uint8(top + (bottom-top)*y/size)
		for x := range size {
			img.SetRGBA(x, y, color.RGBA{shade, uint8(x * 255 / size), 255 - shade, 255})
		}
	}
	for range 14 {
		cx, cy, radius := random.IntN(size), random.IntN(size), size/20+random.IntN(size/4)
		fill := color.RGBA{uint8(random.IntN(256)), uint8(random.IntN(256)), uint8(random.IntN(256)), 255}
		for y := max(0, cy-radius); y < min(size, cy+radius); y++ {
			for x := max(0, cx-radius); x < min(size, cx+radius); x++ {
				if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= radius*radius {
					img.SetRGBA(x, y, fill)
				}
			}
		}
	}
	return img
}

// originTestClientPhoto 模拟客户端把插画交给社区之前的处理：等比缩放到 side，再按 quality 转成 JPEG。
func originTestClientPhoto(tb testing.TB, source image.Image, side, quality int) []byte {
	tb.Helper()
	scaled := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.CatmullRom.Scale(scaled, scaled.Bounds(), source, source.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, scaled, &jpeg.Options{Quality: quality}); err != nil {
		tb.Fatal(err)
	}
	return out.Bytes()
}

func originHashDistance(a, b int64) int {
	return bits.OnesCount64(uint64(a ^ b))
}

func TestSkinArtworkHashSurvivesClientProcessing(t *testing.T) {
	artwork := originTestArtwork(1, 1024)
	// 上游返回 PNG，服务端按解码后的图片记指纹。
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, artwork); err != nil {
		t.Fatal(err)
	}
	decoded, _, _, ok := DecodePNGJPEG(encoded.Bytes(), 2048, 0)
	if !ok {
		t.Fatal("合成插画解码失败")
	}
	recorded := skinArtworkHash(decoded)
	for _, tc := range []struct {
		name          string
		side, quality int
	}{{"same size q80", 1024, 80}, {"same size q20", 1024, 20}, {"downscaled q60", 720, 60}, {"small q40", 256, 40}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := communityDesignFieldsPhoto(t, originTestClientPhoto(t, artwork, tc.side, tc.quality))
			design, err := parseCommunityDesign(raw)
			if err != nil {
				t.Fatal(err)
			}
			hash, err := communityDesignPhotoHash(design)
			if err != nil || hash == nil {
				t.Fatal(hash, err)
			}
			d := originHashDistance(recorded, *hash)
			t.Logf("客户端处理后的距离 %d", d)
			if d > skinArtworkMatchDistance {
				t.Fatalf("客户端处理后的距离 %d 超过上限 %d", d, skinArtworkMatchDistance)
			}
		})
	}
	for seed := uint64(2); seed < 12; seed++ {
		d := originHashDistance(recorded, skinArtworkHash(originTestArtwork(seed, 512)))
		t.Logf("无关插画 %d 的距离 %d", seed, d)
		if d <= skinArtworkMatchDistance {
			t.Fatalf("无关插画 %d 的距离只有 %d", seed, d)
		}
	}
	if hash, err := communityDesignPhotoHash(CommunityDesign{}); hash != nil || err != nil {
		t.Fatal("没有照片时应返回 nil", hash, err)
	}
	if _, err := communityDesignPhotoHash(CommunityDesign{Photo: []byte("not a jpeg")}); err == nil {
		t.Fatal("损坏的照片没有报错")
	}
}

func TestCommunitySkinOriginQueryOptions(t *testing.T) {
	for _, tc := range []struct {
		query     string
		fields    []string
		excludeAI bool
		code      string
		header    bool
	}{
		{"", nil, false, "", false},
		{"fields=origin", []string{"origin"}, false, "", true},
		{"fields=moderation,saved,origin", []string{"moderation", "origin", "saved"}, false, "", true},
		{"exclude=ai", nil, true, "", true},
		{"fields=saved&exclude=ai", []string{"saved"}, true, "", true},
		{"fields=moderation", []string{"moderation"}, false, "", false},
		{"fields=kinds", nil, false, "invalid_fields", false},
		{"exclude=user", nil, false, "invalid_exclude", false},
		{"exclude=AI", nil, false, "invalid_exclude", false},
		{"exclude=ai,user", nil, false, "invalid_exclude", false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/community/skins?"+tc.query, nil)
			w := httptest.NewRecorder()
			fields, ok := communitySkinFields(r.URL.Query())
			code := ""
			excludeAI := false
			if !ok {
				code = "invalid_fields"
			} else if excludeAI, ok = communitySkinOriginOptions(w, r.URL.Query(), fields); !ok {
				code = "invalid_exclude"
				if !strings.Contains(w.Body.String(), `"invalid_exclude"`) || w.Code != 400 {
					t.Fatalf("错误响应 %d %s", w.Code, w.Body.String())
				}
			}
			var names []string
			for name := range fields {
				names = append(names, name)
			}
			slices.Sort(names)
			if code != tc.code || excludeAI != tc.excludeAI || (code == "" && !slices.Equal(names, tc.fields)) {
				t.Fatalf("code=%q exclude=%v fields=%v", code, excludeAI, names)
			}
			if got := w.Header().Get(skinOriginHeader) == skinOriginVersion; got != tc.header {
				t.Fatalf("能力头 = %v，want %v", got, tc.header)
			}
		})
	}
	// 插件仍用目录共用的字段，不接受 origin。
	if _, ok := communityCatalogFields(httptest.NewRequest("GET", "/?fields=origin", nil).URL.Query()); ok {
		t.Fatal("插件目录接受了 fields=origin")
	}
	if communitySkinOriginFilter(false, "s") != "" {
		t.Fatal("没有声明 exclude 时不应改变查询")
	}
}

type originTestSkin struct {
	ID     string  `json:"id"`
	Origin *string `json:"origin"`
}

func originTestList(t *testing.T, mux http.Handler, path, token string) ([]originTestSkin, *httptest.ResponseRecorder) {
	t.Helper()
	w := apiRequest(t, mux, "GET", path, "", token, 200)
	var page struct {
		Skins []originTestSkin `json:"skins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Skins, w
}

func originTestIDs(skins []originTestSkin) []string {
	ids := make([]string, len(skins))
	for i, skin := range skins {
		ids[i] = skin.ID
	}
	slices.Sort(ids)
	return ids
}

func originTestPublish(t *testing.T, mux http.Handler, token, id string, design json.RawMessage, extra string, status int) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"id":"` + id + `","name":"来源测试","description":"","design":` + string(design) + extra + `}`
	return apiRequest(t, mux, "POST", "/v1/community/skins", body, token, status)
}

func originTestValue(t *testing.T, db *Store, id string) string {
	t.Helper()
	var origin string
	if err := db.pool.QueryRow(context.Background(), `SELECT origin FROM community_skins WHERE id=$1`, id).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	return origin
}

func TestCommunitySkinOriginPublishAndExclude(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_artwork_fingerprints`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "skin-origin-owner@example.test"})
	viewer := complete(t, db, Identity{"email", "skin-origin-viewer@example.test"})

	artwork := originTestArtwork(1, 1024)
	if err := a.RecordSkinArtwork(ctx, artwork); err != nil {
		t.Fatal(err)
	}
	// 同一张插画重复记录不报错。
	if err := a.RecordSkinArtwork(ctx, artwork); err != nil {
		t.Fatal(err)
	}
	const (
		aiPhoto     = "0f1e2d3c-0000-4000-8000-000000000001"
		userPhoto   = "0f1e2d3c-0000-4000-8000-000000000002"
		palette     = "0f1e2d3c-0000-4000-8000-000000000003"
		declared    = "0f1e2d3c-0000-4000-8000-000000000004"
		legacyPhoto = "0f1e2d3c-0000-4000-8000-000000000005"
		legacyPlain = "0f1e2d3c-0000-4000-8000-000000000006"
	)
	aiDesign := communityDesignFieldsPhoto(t, originTestClientPhoto(t, artwork, 1024, 60))
	userDesign := communityDesignFieldsPhoto(t, originTestClientPhoto(t, originTestArtwork(7, 1024), 1024, 60))
	originTestPublish(t, mux, owner.AccessToken, aiPhoto, aiDesign, "", 201)
	originTestPublish(t, mux, owner.AccessToken, userPhoto, userDesign, `,"origin":null`, 201)
	originTestPublish(t, mux, owner.AccessToken, palette, json.RawMessage(communityFixture), "", 201)
	originTestPublish(t, mux, owner.AccessToken, declared, json.RawMessage(communityFixture), `,"origin":"ai"`, 201)
	for _, bad := range []string{`"user"`, `"unknown"`, `"AI"`, `""`, `1`} {
		w := originTestPublish(t, mux, owner.AccessToken, "0f1e2d3c-0000-4000-8000-0000000000ff", json.RawMessage(communityFixture), `,"origin":`+bad, 400)
		if bad != `1` && !strings.Contains(w.Body.String(), `"invalid_skin_origin"`) {
			t.Fatalf("origin=%s: %s", bad, w.Body.String())
		}
	}
	// 重试不比较来源，返回已存的作品，来源不变。
	originTestPublish(t, mux, owner.AccessToken, userPhoto, userDesign, `,"origin":"ai"`, 200)
	for id, want := range map[string]string{aiPhoto: "ai", userPhoto: "user", palette: "user", declared: "ai"} {
		if got := originTestValue(t, db, id); got != want {
			t.Fatalf("%s 的来源 = %q，want %q", id, got, want)
		}
	}
	// 滚动升级期间旧副本写入的作品取列默认值 unknown，有没有照片都一样。
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,description,design) VALUES($1,$3,'旧照片','',$4),($2,$3,'旧配色','',$5)`, legacyPhoto, legacyPlain, owner.User.ID, []byte(userDesign), communityFixture); err != nil {
		t.Fatal(err)
	}
	if originTestValue(t, db, legacyPhoto) != "unknown" || originTestValue(t, db, legacyPlain) != "unknown" {
		t.Fatal("旧副本写入的作品应为 unknown")
	}

	all := []string{aiPhoto, userPhoto, palette, declared, legacyPhoto, legacyPlain}
	// exclude=ai 只留下 user：unknown 推不出来源，宁可不显示。
	visible := []string{userPhoto, palette}
	// 没有声明的请求：不带来源字段和能力头，也不过滤。
	skins, w := originTestList(t, mux, "/v1/community/skins", viewer.AccessToken)
	if !slices.Equal(originTestIDs(skins), all) || strings.Contains(w.Body.String(), `"origin"`) || w.Header().Get(skinOriginHeader) != "" {
		t.Fatalf("默认列表改变：%v %s %v", originTestIDs(skins), w.Body.String(), w.Header())
	}
	skins, w = originTestList(t, mux, "/v1/community/skins?fields=origin", "")
	if !slices.Equal(originTestIDs(skins), all) || w.Header().Get(skinOriginHeader) != skinOriginVersion || !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), skinOriginHeader) {
		t.Fatalf("fields=origin：%v %v", originTestIDs(skins), w.Header())
	}
	want := map[string]string{aiPhoto: "ai", userPhoto: "user", palette: "user", declared: "ai", legacyPhoto: "unknown", legacyPlain: "unknown"}
	for _, skin := range skins {
		if skin.Origin == nil || *skin.Origin != want[skin.ID] {
			t.Fatalf("%s 的 origin = %v", skin.ID, skin.Origin)
		}
	}
	for _, path := range []string{"/v1/community/skins?exclude=ai", "/v1/community/skins?exclude=ai&fields=saved&include=category", "/v1/community/skins?exclude=ai&q=%E6%9D%A5%E6%BA%90%E6%B5%8B%E8%AF%95"} {
		skins, w = originTestList(t, mux, path, viewer.AccessToken)
		if !slices.Equal(originTestIDs(skins), visible) || w.Header().Get(skinOriginHeader) != skinOriginVersion || strings.Contains(w.Body.String(), `"origin"`) {
			t.Fatalf("%s：%v %v", path, originTestIDs(skins), w.Header())
		}
	}
	// 作者自己的作品和收藏同样过滤。
	skins, _ = originTestList(t, mux, "/v1/community/skins?scope=mine&exclude=ai&fields=origin", owner.AccessToken)
	if !slices.Equal(originTestIDs(skins), visible) {
		t.Fatal("scope=mine 没有过滤", originTestIDs(skins))
	}
	for _, id := range all {
		apiRequest(t, mux, "PUT", "/v1/community/skins/"+id+"/save", `{"saved":true}`, viewer.AccessToken, 200)
	}
	skins, _ = originTestList(t, mux, "/v1/community/skins?scope=saved&exclude=ai", viewer.AccessToken)
	if !slices.Equal(originTestIDs(skins), visible) {
		t.Fatal("scope=saved 没有过滤", originTestIDs(skins))
	}
	w = apiRequest(t, mux, "GET", "/v1/community/skins?exclude=human", "", "", 400)
	if !strings.Contains(w.Body.String(), `"invalid_exclude"`) {
		t.Fatal(w.Body.String())
	}
	// 详情与下载：被排除的作品与不存在一样返回 404。
	for _, id := range all {
		hidden := !slices.Contains(visible, id)
		detail := 200
		if hidden {
			detail = 404
		}
		w = apiRequest(t, mux, "GET", "/v1/community/skins/"+id+"?exclude=ai", "", viewer.AccessToken, detail)
		if w.Header().Get(skinOriginHeader) != skinOriginVersion {
			t.Fatal("详情缺少能力头", id)
		}
		apiRequest(t, mux, "POST", "/v1/community/skins/"+id+"/download?exclude=ai", "", viewer.AccessToken, detail)
		w = apiRequest(t, mux, "GET", "/v1/community/skins/"+id, "", viewer.AccessToken, 200)
		if strings.Contains(w.Body.String(), `"origin"`) || w.Header().Get(skinOriginHeader) != "" {
			t.Fatal("默认详情改变", w.Body.String())
		}
		apiRequest(t, mux, "POST", "/v1/community/skins/"+id+"/download", "", viewer.AccessToken, 200)
	}
	w = apiRequest(t, mux, "GET", "/v1/community/skins/"+aiPhoto+"?fields=origin", "", "", 200)
	if !strings.Contains(w.Body.String(), `"origin":"ai"`) {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "GET", "/v1/community/skins/"+aiPhoto+"?exclude=1", "", "", 400)
	apiRequest(t, mux, "POST", "/v1/community/skins/"+aiPhoto+"/download?exclude=1", "", viewer.AccessToken, 400)
	// 作者改分类的 PATCH 不认 fields=origin，返回里也不带来源。
	w = apiRequest(t, mux, "PATCH", "/v1/community/skins/"+aiPhoto, `{"category":"food"}`, owner.AccessToken, 200)
	if strings.Contains(w.Body.String(), `"origin"`) {
		t.Fatal(w.Body.String())
	}
}

func TestAdminSkinOrigin(t *testing.T) {
	db, a, owner, _, call := moderationFixture(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_artwork_fingerprints`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{`{"action":"set_skin_origin","id":"skin-a","value":{"origin":"robot"}}`, "invalid_origin", 400},
		{`{"action":"set_skin_origin","id":"skin-a"}`, "invalid_origin", 400},
		{`{"action":"set_skin_origin","id":"skin-a","value":{"origin":"ai","extra":1}}`, "invalid_origin", 400},
		{`{"action":"set_skin_origin","id":"skin-a","section":"plugins","value":{"origin":"ai"}}`, "invalid_section", 400},
		{`{"action":"set_skin_origin","id":"missing","value":{"origin":"ai"}}`, "not_found", 404},
	} {
		w := call("POST", "/api/actions", tc.body)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
			t.Fatalf("%s：%d %s", tc.body, w.Code, w.Body.String())
		}
	}
	w := call("POST", "/api/actions", `{"action":"set_skin_origin","id":"skin-a","section":"skins","value":{"origin":"ai"}}`)
	if w.Code != 200 || originTestValue(t, db, "skin-a") != "ai" || originTestValue(t, db, "skin-b") != "unknown" {
		t.Fatalf("单项改判：%d %s", w.Code, w.Body.String())
	}
	var detail string
	if err := db.pool.QueryRow(ctx, `SELECT detail::text FROM admin_audit WHERE action='set_skin_origin' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, `"from": "unknown"`) || !strings.Contains(detail, `"origin": "ai"`) {
		t.Fatal("审计缺少来源", detail)
	}
	w = call("POST", "/api/actions", `{"action":"set_skin_origin","ids":["skin-a","skin-b"],"value":{"origin":"user"}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) || originTestValue(t, db, "skin-a") != "user" || originTestValue(t, db, "skin-b") != "user" {
		t.Fatalf("批量改判：%d %s", w.Code, w.Body.String())
	}
	// 后台列表带来源，方便审核时判断。
	w = call("GET", "/api/skins", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"origin":"user"`) {
		t.Fatalf("后台列表：%d %s", w.Code, w.Body.String())
	}
	// 按来源筛选，审核员据此找出待改判的 unknown 作品。
	if _, err := db.pool.Exec(ctx, `UPDATE community_skins SET origin='unknown' WHERE id='skin-b'`); err != nil {
		t.Fatal(err)
	}
	w = call("GET", "/api/skins?origin=unknown", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"skin-b"`) || strings.Contains(w.Body.String(), `"id":"skin-a"`) {
		t.Fatalf("按来源筛选：%d %s", w.Code, w.Body.String())
	}
	if w = call("GET", "/api/skins?origin=robot", ""); w.Code != 400 {
		t.Fatalf("未知来源筛选：%d %s", w.Code, w.Body.String())
	}
	if _, err := db.pool.Exec(ctx, `UPDATE community_skins SET origin='user' WHERE id='skin-b'`); err != nil {
		t.Fatal(err)
	}

	// 后台新建按照片判定；修改只在新照片匹配到插画时抬成 ai，其余保留原值。
	artwork := originTestArtwork(3, 1024)
	if err := a.RecordSkinArtwork(ctx, artwork); err != nil {
		t.Fatal(err)
	}
	aiDesign := communityDesignFieldsPhoto(t, originTestClientPhoto(t, artwork, 800, 70))
	userDesign := communityDesignFieldsPhoto(t, originTestClientPhoto(t, originTestArtwork(9, 1024), 800, 70))
	save := func(id string, design json.RawMessage, create bool) {
		t.Helper()
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := saveAdminSkin(ctx, tx, id, contentEdit{OwnerID: owner.User.ID, Name: "后台", Design: design, Category: "other"}, create); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	save("admin-ai", aiDesign, true)
	save("admin-user", userDesign, true)
	save("admin-plain", json.RawMessage(communityFixture), true)
	for id, want := range map[string]string{"admin-ai": "ai", "admin-user": "user", "admin-plain": "user"} {
		if got := originTestValue(t, db, id); got != want {
			t.Fatalf("后台新建 %s 的来源 = %q", id, got)
		}
	}
	save("admin-plain", aiDesign, false)
	save("admin-ai", userDesign, false)
	save("skin-b", json.RawMessage(communityFixture), false)
	for id, want := range map[string]string{"admin-plain": "ai", "admin-ai": "ai", "skin-b": "user"} {
		if got := originTestValue(t, db, id); got != want {
			t.Fatalf("后台修改 %s 的来源 = %q", id, got)
		}
	}
}

// 迁移只加列，不读 design；没有照片的已有作品由后台回填分批改成 user，带照片的保持 unknown。回填按游标推进，重跑只处理之后新出现的作品，不覆盖人工改判，也不碰创建不到 5 分钟的作品。
func TestCommunitySkinOriginBackfill(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	owner := complete(t, db, Identity{"email", "skin-origin-backfill@example.test"})
	photo := communityDesignFieldsPhoto(t, originTestClientPhoto(t, originTestArtwork(5, 256), 256, 70))
	if _, err := db.pool.Exec(ctx, `ALTER TABLE community_skins DROP COLUMN origin`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM site_settings WHERE key=$1`, skinOriginBackfillKey); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx); err == nil {
		t.Fatal("缺少来源列时 Ready 应失败并触发迁移")
	}
	// 一批之外再多几行，覆盖跨批推进游标；照片行夹在中间。
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,description,design,created_at) SELECT 'backfill-'||lpad(n::text,3,'0'),$1,'旧配色','',$2,now()-interval '1 day'+n*interval '1 second' FROM generate_series(1,$3::int) n`, owner.User.ID, communityFixture, skinOriginBackfillBatch+20); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,description,design,created_at) VALUES('backfill-photo',$1,'旧照片','',$2,now()-interval '1 day'+interval '50 seconds')`, owner.User.ID, []byte(photo)); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	var unknown int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM community_skins WHERE id LIKE 'backfill-%' AND origin<>'unknown'`).Scan(&unknown); err != nil || unknown != 0 {
		t.Fatal("迁移不应回填", unknown, err)
	}

	// 另一个副本正在回填时让出，什么都不改。
	holder, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, skinOriginBackfillLock); err != nil {
		t.Fatal(err)
	}
	if err = db.backfillSkinOrigins(ctx); err != nil {
		t.Fatal(err)
	}
	if originTestValue(t, db, "backfill-001") != "unknown" {
		t.Fatal("拿不到锁时不应回填")
	}
	if err = holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if err = db.backfillSkinOrigins(ctx); err != nil {
		t.Fatal(err)
	}
	var plain int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM community_skins WHERE id LIKE 'backfill-%' AND id<>'backfill-photo' AND origin='user'`).Scan(&plain); err != nil || plain != skinOriginBackfillBatch+20 {
		t.Fatal("没有照片的作品应全部回填为 user", plain, err)
	}
	if originTestValue(t, db, "backfill-photo") != "unknown" {
		t.Fatal("带照片的作品应保持 unknown")
	}

	// 回填之后：审核员改判的结果不被覆盖；滚动升级期间旧副本写入的作品下一轮补上；刚写入的作品等到下一轮。
	if _, err = db.pool.Exec(ctx, `UPDATE community_skins SET origin='ai' WHERE id='backfill-001'`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,description,design,created_at) VALUES('backfill-late',$1,'旧副本','',$2,now()-interval '10 minutes'),('backfill-fresh',$1,'刚写入','',$2,now())`, owner.User.ID, communityFixture); err != nil {
		t.Fatal(err)
	}
	if err = db.backfillSkinOrigins(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"backfill-001": "ai", "backfill-late": "user", "backfill-fresh": "unknown", "backfill-photo": "unknown"} {
		if got := originTestValue(t, db, id); got != want {
			t.Fatalf("%s 的来源 = %q，want %q", id, got, want)
		}
	}

	// 重跑迁移不改动来源；约束拒绝未知来源，且只有一份。
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if originTestValue(t, db, "backfill-001") != "ai" || originTestValue(t, db, "backfill-002") != "user" {
		t.Fatal("重跑迁移改动了来源")
	}
	if _, err = db.pool.Exec(ctx, `UPDATE community_skins SET origin='robot' WHERE id='backfill-002'`); err == nil {
		t.Fatal("约束接受了未知来源")
	}
	var constraints int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='community_skins'::regclass AND conname LIKE 'community_skins_origin_check%'`).Scan(&constraints); err != nil {
		t.Fatal(err)
	}
	if constraints != 1 {
		t.Fatal("来源约束数", constraints)
	}
	// 游标损坏时报错，不从头扫一遍。
	if _, err = db.pool.Exec(ctx, `UPDATE site_settings SET value='not json' WHERE key=$1`, skinOriginBackfillKey); err != nil {
		t.Fatal(err)
	}
	if err = db.backfillSkinOrigins(ctx); err == nil {
		t.Fatal("损坏的游标没有报错")
	}
	if _, err = db.pool.Exec(ctx, `DELETE FROM site_settings WHERE key=$1`, skinOriginBackfillKey); err != nil {
		t.Fatal(err)
	}
}
