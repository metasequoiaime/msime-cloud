package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const communityFixture = `{"background":15266027,"keyBackground":16777215,"keyForeground":1516829,"accent":1596487,"actionBackground":1596487,"cornerRadius":8,"borderWidth":0,"shadow":0,"pattern":0,"monospaced":false}`

func TestCommunityDesignValidation(t *testing.T) {
	if _, e := parseCommunityDesign(json.RawMessage(communityFixture)); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{`{}`, `null`, strings.Replace(communityFixture, `"cornerRadius":8`, `"cornerRadius":21`, 1), strings.Replace(communityFixture, `"pattern":0`, `"pattern":4`, 1), strings.Replace(communityFixture, `"monospaced":false`, `"monospaced":false,"photo":"aW52YWxpZA=="`, 1)} {
		if _, e := parseCommunityDesign(json.RawMessage(raw)); e == nil {
			t.Fatal("accepted", raw)
		}
	}
}

func TestCommunityPublishRejectsControlCharactersInMetadata(t *testing.T) {
	store := testStore(t)
	owner := complete(t, store, Identity{"apple", "metadata-owner"})
	a := &Service{store: store}
	request := func(body string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		Mount(mux, a)
		r := httptest.NewRequest("POST", "/v1/community/skins", bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+owner.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	base := `{"id":"ab334455-1234-1234-1234-123456789abd","name":"正常标题","description":"正常说明","design":` + communityFixture + `}`
	for _, body := range []string{
		strings.Replace(base, "正常标题", `坏\u0007标题`, 1),
		strings.Replace(base, "正常说明", `坏\u0000说明`, 1),
	} {
		w := request(body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_skin_metadata"`) {
			t.Fatalf("control metadata accepted: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestCommunityPublishDownloadRatingOwnershipAndRestart(t *testing.T) {
	store := testStore(t)
	owner := complete(t, store, Identity{"apple", "owner"})
	user := complete(t, store, Identity{"apple", "reader"})
	a := &Service{store: store}
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		Mount(mux, a)
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	id := "ab334455-1234-1234-1234-123456789abc"
	path := "/v1/community/skins/" + id
	body := `{"id":"` + id + `","name":"测试皮肤","description":"示例","design":` + communityFixture + `}`
	if w := request("POST", "/v1/community/skins", body, ""); w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "/v1/community/skins", body, owner.AccessToken); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "/v1/community/skins", body, owner.AccessToken); w.Code != 200 {
		t.Fatal("retry", w.Code, w.Body.String())
	}
	if w := request("POST", "/v1/community/skins", strings.Replace(body, "测试皮肤", "变更皮肤", 1), owner.AccessToken); w.Code != 409 {
		t.Fatal("changed retry", w.Code)
	}
	if w := request("POST", "/v1/community/skins", body, user.AccessToken); w.Code != 409 {
		t.Fatal("identity", w.Code)
	}
	// 登录即可评分，不需要先下载。
	if w := request("PUT", path+"/rating", `{"stars":5}`, user.AccessToken); w.Code != 200 {
		t.Fatal("rating before download", w.Code, w.Body.String())
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request("POST", path+"/download", `{}`, user.AccessToken)
			if w.Code != 200 {
				t.Error(w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	for _, stars := range []string{"5", "3"} {
		if w := request("PUT", path+"/rating", `{"stars":`+stars+`}`, user.AccessToken); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request("PUT", path+"/rating", `{"stars":6}`, user.AccessToken); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := request("PUT", path+"/rating", `{"stars":5}`, owner.AccessToken); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := request("DELETE", path, ``, user.AccessToken); w.Code != 404 {
		t.Fatal("owner check", w.Code)
	}
	if _, err := store.pool.Exec(context.Background(), "UPDATE auth_users SET display_name='' WHERE id=$1", owner.User.ID); err != nil {
		t.Fatal(err)
	}
	a = &Service{store: store}
	w := request("GET", path, ``, user.AccessToken)
	var v CommunitySkin
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Downloads != 1 || v.RatingCount != 1 || v.RatingAverage != 3 || v.MyRating != 3 || v.Owned {
		t.Fatal(w.Code, w.Body.String())
	}
	if v.Author != defaultUserName(owner.User.ID) {
		t.Fatalf("legacy author name = %q", v.Author)
	}
	w = request("GET", "/v1/community/skins", ``, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "测试皮肤") || strings.Contains(w.Body.String(), user.User.ID) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request("DELETE", path, ``, owner.AccessToken); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("GET", path, ``, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestCommunityKeyStylesRoundTrip(t *testing.T) {
	for _, shape := range []string{"rounded", "capsule", "ticket", "pebble"} {
		for _, material := range []string{"flat", "raised", "glass", "paper"} {
			raw := strings.TrimSuffix(communityFixture, "}") + `,"keyShape":"` + shape + `","keyMaterial":"` + material + `"}`
			value, err := parseCommunityDesign(json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := parseCommunityDesign(encoded)
			if err != nil || restored.KeyShape != shape || restored.KeyMaterial != material {
				t.Fatal("style lost", err)
			}
		}
	}
	for _, extra := range []string{`,"keyShape":"remote.svg"}`, `,"keyMaterial":"metal"}`} {
		if _, err := parseCommunityDesign(json.RawMessage(strings.TrimSuffix(communityFixture, "}") + extra)); err == nil {
			t.Fatal("invalid style accepted")
		}
	}
}

// 键盘皮肤的分类只在 include=category 时出现：没有声明的列表、详情和 PATCH 响应与加入分类之前逐字节相同，已发布客户端按拒绝未知字段解析条目；发布响应仍只有 id。
func TestCommunitySkinCategory(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "skin-category-owner@example.test"})
	other := complete(t, db, Identity{"email", "skin-category-other@example.test"})
	publish := func(id, name, extra string) string {
		body := `{"id":"` + id + `","name":"` + name + `","description":"d","design":` + communityFixture
		if extra != "" {
			body += "," + extra
		}
		return body + "}"
	}
	// legacy 把条目去掉 category 后重新编码，与响应逐字节比较；条目里出现 category 键时失败。
	legacy := func(w *httptest.ResponseRecorder) {
		t.Helper()
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		var want bytes.Buffer
		if skins, ok := raw["skins"]; ok {
			var page struct {
				Skins   []CommunitySkin `json:"skins"`
				HasMore bool            `json:"has_more"`
			}
			var items []map[string]json.RawMessage
			if err := json.Unmarshal(skins, &items); err != nil || json.Unmarshal(w.Body.Bytes(), &page) != nil {
				t.Fatal(w.Body.String(), err)
			}
			for _, item := range items {
				if _, present := item["category"]; present {
					t.Fatal("released clients would reject category", w.Body.String())
				}
			}
			for i := range page.Skins {
				page.Skins[i].Category = ""
			}
			_ = json.NewEncoder(&want).Encode(map[string]any{"skins": page.Skins, "has_more": page.HasMore})
		} else {
			if _, present := raw["category"]; present {
				t.Fatal("released clients would reject category", w.Body.String())
			}
			var item CommunitySkin
			if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
				t.Fatal(err)
			}
			item.Category = ""
			_ = json.NewEncoder(&want).Encode(item)
		}
		if w.Body.String() != want.String() {
			t.Fatalf("released shape changed:\n%s\n%s", w.Body.String(), want.String())
		}
	}
	category := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var v struct {
			Category *string `json:"category"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.Category == nil || *v.Category == "" {
			t.Fatal("category missing", w.Body.String(), err)
		}
		return *v.Category
	}
	listCategories := func(path, token string) map[string]string {
		t.Helper()
		w := apiRequest(t, mux, "GET", path, "", token, 200)
		if !strings.Contains(path, "include=category") {
			legacy(w)
		}
		var page struct {
			Skins []struct {
				ID       string  `json:"id"`
				Category *string `json:"category"`
			} `json:"skins"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		items := map[string]string{}
		for _, item := range page.Skins {
			if (item.Category != nil) != strings.Contains(path, "include=category") {
				t.Fatal(path, "category does not follow the opt-in", w.Body.String())
			}
			items[item.ID] = ""
			if item.Category != nil {
				items[item.ID] = *item.Category
			}
		}
		return items
	}

	// 发布：缺省和 null 为 other，响应仍只有 id；未知值和空串是 400。
	plain := "ac334455-1234-4234-8234-000000000001"
	food := "ac334455-1234-4234-8234-000000000002"
	null := "ac334455-1234-4234-8234-000000000003"
	if w := apiRequest(t, mux, "POST", "/v1/community/skins", publish(plain, "Plain", ""), owner.AccessToken, 201); w.Body.String() != `{"id":"`+plain+`"}`+"\n" {
		t.Fatal("publish response changed", w.Body.String())
	}
	if w := apiRequest(t, mux, "POST", "/v1/community/skins?include=category", publish(food, "Food", `"category":"food"`), owner.AccessToken, 201); w.Body.String() != `{"id":"`+food+`"}`+"\n" {
		t.Fatal("publish response changed", w.Body.String())
	}
	apiRequest(t, mux, "POST", "/v1/community/skins", publish(null, "Null", `"category":null`), owner.AccessToken, 201)
	// 同一内容换一个分类重试仍是同一请求：分类不参与重试比较，已存的分类不变。
	apiRequest(t, mux, "POST", "/v1/community/skins", publish(food, "Food", `"category":"acg"`), owner.AccessToken, 200)
	for _, extra := range []string{`"category":"anime"`, `"category":""`, `"category":"Food"`, `"category":1`} {
		w := apiRequest(t, mux, "POST", "/v1/community/skins", publish("ac334455-1234-4234-8234-000000000009", "Bad", extra), owner.AccessToken, 400)
		if extra != `"category":1` && !strings.Contains(w.Body.String(), `"invalid_category"`) {
			t.Fatal(extra, w.Body.String())
		}
	}

	// 列表：category 筛选沿用发布时间倒序，与 scope=mine、fields=moderation 可以同时使用。
	if got := listCategories("/v1/community/skins", ""); len(got) != 3 {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/skins?include=category", ""); len(got) != 3 || got[food] != "food" || got[plain] != "other" || got[null] != "other" {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/skins?category=food&include=category", ""); len(got) != 1 || got[food] != "food" {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/skins?category=other", ""); len(got) != 2 {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/skins?scope=mine&fields=moderation", owner.AccessToken); len(got) != 3 {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/skins?scope=mine&fields=moderation&category=other&include=category", owner.AccessToken); len(got) != 2 || got[plain] != "other" {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/skins?category=nature", ""); len(got) != 0 {
		t.Fatal(got)
	}
	var page struct {
		Skins []struct {
			ID string `json:"id"`
		} `json:"skins"`
	}
	if err := json.Unmarshal(apiRequest(t, mux, "GET", "/v1/community/skins?category=other", "", "", 200).Body.Bytes(), &page); err != nil || len(page.Skins) != 2 || page.Skins[0].ID != null || page.Skins[1].ID != plain {
		t.Fatal("filtered order", page, err)
	}
	for path, code := range map[string]string{
		"/v1/community/skins?category=Food":           "invalid_category",
		"/v1/community/skins?category=":               "",
		"/v1/community/skins?include=categories":      "invalid_include",
		"/v1/community/skins/" + food + "?include=x":  "invalid_include",
		"/v1/community/skins/" + food + "?fields=all": "invalid_fields",
	} {
		if code == "" {
			apiRequest(t, mux, "GET", path, "", "", 200)
			continue
		}
		if w := apiRequest(t, mux, "GET", path, "", "", 400); !strings.Contains(w.Body.String(), `"`+code+`"`) {
			t.Fatal(path, w.Body.String())
		}
	}

	// 详情：未声明时逐字节不变，声明后带分类，与 fields=moderation 互相独立。
	legacy(apiRequest(t, mux, "GET", "/v1/community/skins/"+food, "", other.AccessToken, 200))
	legacy(apiRequest(t, mux, "GET", "/v1/community/skins/"+food+"?fields=moderation", "", owner.AccessToken, 200))
	if w := apiRequest(t, mux, "GET", "/v1/community/skins/"+food+"?include=category", "", "", 200); category(w) != "food" {
		t.Fatal(w.Body.String())
	}
	if w := apiRequest(t, mux, "GET", "/v1/community/skins/"+food+"?fields=moderation&include=category", "", owner.AccessToken, 200); category(w) != "food" || !strings.Contains(w.Body.String(), `"moderation":"pending"`) {
		t.Fatal("detail with both opt-ins", w.Body.String())
	}

	// PATCH：作者只改分类，返回详情形状的作品；不改变审核状态。
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_skins SET moderation='approved' WHERE id=$1`, plain); err != nil {
		t.Fatal(err)
	}
	w := apiRequest(t, mux, "PATCH", "/v1/community/skins/"+plain, `{"category":"guofeng"}`, owner.AccessToken, 200)
	legacy(w)
	var patched CommunitySkin
	if err := json.Unmarshal(w.Body.Bytes(), &patched); err != nil || patched.ID != plain || !patched.Owned || len(patched.Design) == 0 {
		t.Fatal(w.Body.String(), err)
	}
	if w = apiRequest(t, mux, "GET", "/v1/community/skins/"+plain+"?include=category", "", "", 200); category(w) != "guofeng" {
		t.Fatal(w.Body.String())
	}
	if state, _, _, _ := moderationState(t, db, "community_skins", plain); state != "approved" {
		t.Fatal("category change sent the item back to review", state)
	}
	if w = apiRequest(t, mux, "PATCH", "/v1/community/skins/"+plain+"?include=category&fields=moderation", `{"category":"minimal"}`, owner.AccessToken, 200); category(w) != "minimal" || !strings.Contains(w.Body.String(), `"moderation":"approved"`) {
		t.Fatal(w.Body.String())
	}
	// 设为当前分类同样成功。
	if w = apiRequest(t, mux, "PATCH", "/v1/community/skins/"+plain+"?include=category", `{"category":"minimal"}`, owner.AccessToken, 200); category(w) != "minimal" {
		t.Fatal(w.Body.String())
	}
	for body, code := range map[string]string{`{}`: "invalid_category", `{"category":null}`: "invalid_category", `{"category":""}`: "invalid_category", `{"category":"anime"}`: "invalid_category", `{"category":"tech","name":"x"}`: "invalid_json"} {
		if w = apiRequest(t, mux, "PATCH", "/v1/community/skins/"+plain, body, owner.AccessToken, 400); !strings.Contains(w.Body.String(), `"`+code+`"`) {
			t.Fatal(body, w.Body.String())
		}
	}
	apiRequest(t, mux, "PATCH", "/v1/community/skins/"+plain+"?include=1", `{"category":"tech"}`, owner.AccessToken, 400)
	apiRequest(t, mux, "PATCH", "/v1/community/skins/"+plain+"?fields=sync", `{"category":"tech"}`, owner.AccessToken, 400)
	apiRequest(t, mux, "PATCH", "/v1/community/skins/"+plain, `{"category":"tech"}`, "", 401)
	if w = apiRequest(t, mux, "PATCH", "/v1/community/skins/"+food, `{"category":"tech"}`, other.AccessToken, 404); !strings.Contains(w.Body.String(), `"skin_not_found"`) {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "PATCH", "/v1/community/skins/ac334455-1234-4234-8234-00000000ffff", `{"category":"tech"}`, owner.AccessToken, 404)
	// 被下架的作品作者仍能看到，也能改分类。
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_skins SET moderation='removed' WHERE id=$1`, food); err != nil {
		t.Fatal(err)
	}
	if w = apiRequest(t, mux, "PATCH", "/v1/community/skins/"+food+"?include=category", `{"category":"cute"}`, owner.AccessToken, 200); category(w) != "cute" {
		t.Fatal(w.Body.String())
	}
	// PATCH 计入每个社区接口都用的按地址额度。
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_rates SET count=120 WHERE key=$1`, "ip:"+hash("192.0.2.1")); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "PATCH", "/v1/community/skins/"+plain, `{"category":"tech"}`, owner.AccessToken, 429)
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM auth_rates`); err != nil {
		t.Fatal(err)
	}

	// 数据库约束兜底拒绝未知分类。
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_skins SET category='anime' WHERE id=$1`, plain); err == nil {
		t.Fatal("unknown category stored")
	}
}

// 引入分类之前的 community_skins 迁移后：已有行为 other，命名约束和按分类排序的索引各只有一份，重复迁移不再改变，Ready 探测随之通过。
func TestCommunitySkinCategorySchemaUpgrade(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "skin-category-upgrade@example.test"})
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, statement := range []string{`SELECT pg_advisory_xact_lock(8372419)`, `ALTER TABLE community_skins DROP COLUMN category`} {
		if _, err = tx.Exec(t.Context(), statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = db.Ready(t.Context()); err == nil {
		t.Fatal("Ready passed without the category column")
	}
	id := "ad334455-1234-4234-8234-123456789abc"
	if _, err = db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'released',$3)`, id, owner.User.ID, communityFixture); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var category string
	var checks, indexes int
	// 其他包会并行创建测试 schema，索引计数只检查当前 schema。
	if err = db.pool.QueryRow(t.Context(), `SELECT category,(SELECT count(*) FROM pg_constraint WHERE conrelid='community_skins'::regclass AND contype='c' AND conname LIKE 'community_skins_category%'),(SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND tablename='community_skins' AND indexname='community_skins_category_newest') FROM community_skins WHERE id=$1`, id).Scan(&category, &checks, &indexes); err != nil || category != "other" || checks != 1 || indexes != 1 {
		t.Fatal("upgrade", category, checks, indexes, err)
	}
	if err = db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// 管理后台按分类筛选键盘皮肤，并用审计过的 set_skin_category 修改分类。
func TestAdminSkinCategory(t *testing.T) {
	db, _, _, _, call := moderationFixture(t)
	ctx := context.Background()
	if w := call("POST", "/api/actions", `{"action":"set_skin_category","id":"skin-a","value":{"category":"cute"}}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, _, _ := moderationState(t, db, "community_skins", "skin-a"); state != "pending" {
		t.Fatal("category change moved the moderation state", state)
	}
	var action, target, detail string
	if err := db.pool.QueryRow(ctx, `SELECT action,target,detail::text FROM admin_audit ORDER BY id DESC LIMIT 1`).Scan(&action, &target, &detail); err != nil {
		t.Fatal(err)
	}
	var audited map[string]any
	if err := json.Unmarshal([]byte(detail), &audited); err != nil || action != "set_skin_category" || target != "skins" || audited["category"] != "cute" || audited["from"] != "other" || audited["name"] != "春日樱" || audited["section"] != "skins" {
		t.Fatal(action, target, detail, err)
	}
	if w := call("POST", "/api/actions", `{"action":"set_skin_category","section":"skins","ids":["skin-a","skin-b","missing"],"value":{"category":"tech"}}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := db.pool.QueryRow(ctx, `SELECT detail::text FROM admin_audit ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil || !strings.Contains(detail, `"count": 2`) || strings.Contains(detail, `"from"`) {
		t.Fatal(detail, err)
	}
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = `"id-` + strconv.Itoa(i) + `"`
	}
	for body, want := range map[string]string{
		`{"action":"set_skin_category","id":"skin-a","value":{"category":"anime"}}`:                             "invalid_category",
		`{"action":"set_skin_category","id":"skin-a"}`:                                                          "invalid_category",
		`{"action":"set_skin_category","id":"skin-a","value":{"category":"tech","x":1}}`:                        "invalid_category",
		`{"action":"set_skin_category","section":"candidate-skins","id":"skin-a","value":{"category":"tech"}}`:  "invalid_section",
		`{"action":"set_skin_category","value":{"category":"tech"}}`:                                            "invalid_id",
		`{"action":"set_skin_category","id":"missing","value":{"category":"tech"}}`:                             "not_found",
		`{"action":"set_skin_category","ids":[` + strings.Join(tooMany, ",") + `],"value":{"category":"tech"}}`: "invalid_ids",
		`{"action":"set_candidate_skin_category","id":"skin-a","value":{"category":"tech"}}`:                    "not_found",
	} {
		if w := call("POST", "/api/actions", body); !strings.Contains(w.Body.String(), `"`+want+`"`) {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if w := call("GET", "/api/skins?category=tech", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"total": 2`) && !strings.Contains(w.Body.String(), `"total":2`) || !strings.Contains(w.Body.String(), `"category":"tech"`) {
		t.Fatal(w.Body.String())
	}
	if w := call("GET", "/api/skins?category=nature", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":0`) && !strings.Contains(w.Body.String(), `"total": 0`) {
		t.Fatal(w.Body.String())
	}
	if w := call("GET", "/api/skins?category=anime", ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("GET", "/api/skins/skin-a", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"category":"tech"`) {
		t.Fatal(w.Body.String())
	}
}
