package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func TestResourceValidation(t *testing.T) {
	a := &Service{}
	for _, c := range []ResourceContent{{Prompt: ""}, {Prompt: strings.Repeat("字", 2001)}, {Prompt: "test\x00"}, {Prompt: "test", Entries: []SharedWord{{}}}} {
		if _, err := a.validateResource(context.Background(), "reply", c); err == nil {
			t.Fatal("invalid prompt accepted")
		}
	}
	if _, err := a.validateResource(context.Background(), "reply", ResourceContent{Prompt: "简短回答\n保持礼貌"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []ResourceContent{{}, {Entries: make([]SharedWord, 129)}, {Entries: []SharedWord{{Kind: "unknown"}}}, {Prompt: "no", Entries: []SharedWord{{Kind: "pinyin"}}}} {
		if _, err := a.validateResource(context.Background(), "dictionary", c); err == nil {
			t.Fatal("invalid dictionary accepted")
		}
	}
}
func TestResourcePublishVersionSaveRatingIsolation(t *testing.T) {
	store := testStore(t)
	owner := complete(t, store, Identity{"apple", "resource-owner"})
	reader := complete(t, store, Identity{"apple", "resource-reader"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: store})
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	id := "ab334455-1234-1234-1234-123456789abc"
	path := "/v1/community/resources/" + id
	body := `{"id":"` + id + `","kind":"reply","name":"职场回复","description":"示例","content":{"prompt":"简短自然"},"revision":0}`
	check := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("got %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	check(call("POST", "/v1/community/resources", body, ""), 401)
	check(call("POST", "/v1/community/resources", body, owner.AccessToken), 201)
	check(call("POST", "/v1/community/resources", body, owner.AccessToken), 200)
	check(call("POST", "/v1/community/resources", body, reader.AccessToken), 409)
	// 登录即可评分，不需要先收藏。
	check(call("PUT", path+"/rating", `{"stars":5}`, reader.AccessToken), 200)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := call("PUT", path+"/save", `{"saved":true}`, reader.AccessToken)
			if w.Code != 200 {
				t.Error(w.Code)
			}
		}()
	}
	wg.Wait()
	check(call("PUT", path+"/rating", `{"stars":4}`, reader.AccessToken), 200)
	check(call("PUT", path+"/rating", `{"stars":5}`, owner.AccessToken), 403)
	check(call("DELETE", path, "", reader.AccessToken), 404)
	change := strings.Replace(strings.Replace(body, "简短自然", "友好清晰", 1), `"revision":0`, `"revision":1`, 1)
	check(call("POST", "/v1/community/resources", change, owner.AccessToken), 200)
	check(call("POST", "/v1/community/resources", change, owner.AccessToken), 200)
	check(call("POST", "/v1/community/resources", strings.Replace(change, "友好清晰", "过期编辑", 1), owner.AccessToken), 409)
	w := call("GET", path, "", reader.AccessToken)
	check(w, 200)
	var item CommunityResource
	if json.Unmarshal(w.Body.Bytes(), &item) != nil || item.Revision != 2 || item.Saves != 1 || !item.Saved || item.Owned || item.MyRating != 4 {
		t.Fatal(w.Body.String())
	}
	w = call("GET", "/v1/community/resources?kind=reply&scope=saved", "", reader.AccessToken)
	check(w, 200)
	if !strings.Contains(w.Body.String(), id) {
		t.Fatal("missing saved")
	}
	w = call("GET", "/v1/community/resources?kind=reply&scope=mine", "", reader.AccessToken)
	check(w, 200)
	if strings.Contains(w.Body.String(), id) {
		t.Fatal("owner leak")
	}
	check(call("GET", "/v1/community/resources?kind=reply&scope=saved", "", ""), 401)
	check(call("GET", "/v1/community/resources?kind=invalid", "", ""), 400)
	check(call("PUT", path+"/save", `{"saved":false}`, reader.AccessToken), 200)
	check(call("PUT", path+"/save", `{"saved":true}`, reader.AccessToken), 200)
	if err := store.DeleteUser(context.Background(), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	check(call("GET", path, "", ""), 404)
	check(call("PUT", path+"/save", `{"saved":true}`, reader.AccessToken), 404)
}
func TestResourceDictionaryUsesNativeValidation(t *testing.T) {
	binary := os.Getenv("MSIME_ENGINE_TEST_BINARY")
	if binary == "" {
		t.Skip("native engine required")
	}
	a := &Service{engine: engine.Config{Binary: binary, Resources: os.Getenv("MSIME_ENGINE_TEST_RESOURCES")}}
	input := ResourceContent{Entries: []SharedWord{{Kind: "pinyin", Code: "ni hao", Word: "你好", Weight: 10}}}
	result, err := a.validateResource(context.Background(), "dictionary", input)
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Code != "ni'hao" {
		t.Fatal(result, err)
	}
	input.Entries = append(input.Entries, input.Entries[0])
	if _, err = a.validateResource(context.Background(), "dictionary", input); err == nil {
		t.Fatal("accepted normalized duplicate")
	}
	input.Entries = []SharedWord{{Kind: "pinyin", Code: "not-valid", Word: "你好", Weight: 10}}
	if _, err = a.validateResource(context.Background(), "dictionary", input); err == nil {
		t.Fatal("accepted invalid pinyin")
	}
}

func TestStarterResourcesUseValidContent(t *testing.T) {
	raw, err := os.ReadFile("../../assets/community-starter-resources.json")
	if err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Slug, Kind, Name, Description string
		Content                       ResourceContent
	}
	if err = json.Unmarshal(raw, &items); err != nil || len(items) != 9 {
		t.Fatal(err)
	}
	binary := os.Getenv("MSIME_ENGINE_TEST_BINARY")
	a := &Service{engine: engine.Config{Binary: binary, Resources: os.Getenv("MSIME_ENGINE_TEST_RESOURCES")}}
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.Slug] || !resourceText(item.Name, 1, 32, false) || !resourceText(item.Description, 0, 280, true) {
			t.Fatal("invalid starter metadata")
		}
		seen[item.Slug] = true
		if item.Kind == "dictionary" && binary == "" {
			continue
		}
		if _, err = a.validateResource(context.Background(), item.Kind, item.Content); err != nil {
			t.Fatal(item.Slug, err)
		}
	}
}

func TestResourcePhraseValidation(t *testing.T) {
	a := &Service{}
	ctx := context.Background()
	valid := ResourceContent{Phrases: []SharedPhrase{{Text: "此致\n敬礼", Group: " 正式 "}, {Text: "谢谢！", Group: ""}}}
	got, err := a.validateResource(ctx, "phrase", valid)
	if err != nil || got.Phrases[0].Group != "正式" || got.Phrases[0].Text != "此致\n敬礼" {
		t.Fatal("valid phrase pack rejected", got, err)
	}
	limit := make([]SharedPhrase, maximumResourcePhrases)
	for i := range limit {
		limit[i] = SharedPhrase{Text: strings.Repeat("字", 1990) + string(rune('A'+i%26)) + string(rune('a'+i/26))}
	}
	if _, err = a.validateResource(ctx, "phrase", ResourceContent{Phrases: limit}); err != nil {
		t.Fatal("pack at the limits rejected", err)
	}
	for name, c := range map[string]ResourceContent{
		"empty":           {},
		"too many":        {Phrases: append(append([]SharedPhrase{}, limit...), SharedPhrase{Text: "多一条"})},
		"blank":           {Phrases: []SharedPhrase{{Text: " \n "}}},
		"too long":        {Phrases: []SharedPhrase{{Text: strings.Repeat("字", 2001)}}},
		"tab":             {Phrases: []SharedPhrase{{Text: "a\tb"}}},
		"carriage return": {Phrases: []SharedPhrase{{Text: "a\r\nb"}}},
		"NUL":             {Phrases: []SharedPhrase{{Text: "a\x00b"}}},
		"invalid UTF-8":   {Phrases: []SharedPhrase{{Text: "\xff"}}},
		"group control":   {Phrases: []SharedPhrase{{Text: "a", Group: "a\nb"}}},
		"group too long":  {Phrases: []SharedPhrase{{Text: "a", Group: strings.Repeat("组", 33)}}},
		"duplicate":       {Phrases: []SharedPhrase{{Text: "同一句"}, {Text: "同一句", Group: "别组"}}},
		"with prompt":     {Prompt: "x", Phrases: []SharedPhrase{{Text: "a"}}},
		"with entries":    {Entries: []SharedWord{{Kind: "pinyin"}}, Phrases: []SharedPhrase{{Text: "a"}}},
	} {
		if _, err := a.validateResource(ctx, "phrase", c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// 其他类型不能夹带短语。
	if _, err = a.validateResource(ctx, "reply", ResourceContent{Prompt: "x", Phrases: []SharedPhrase{{Text: "a"}}}); err == nil {
		t.Fatal("reply carried phrases")
	}
	if _, err = a.validateResource(ctx, "dictionary", ResourceContent{Entries: []SharedWord{{Kind: "pinyin", Code: "ni", Word: "你", Weight: 1}}, Phrases: []SharedPhrase{{Text: "a"}}}); err == nil {
		t.Fatal("dictionary carried phrases")
	}
	if _, err = a.validateResource(ctx, "phrases", valid); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

// 短语包走与词库、回复相同的发布、列表、详情、收藏、评分和举报；导入接口对它返回 unsupported_kind，由客户端在本地合并；管理后台有自己的分区。
func TestResourcePhrasePackHTTP(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"apple", "phrase-owner"})
	reader := complete(t, db, Identity{"apple", "phrase-reader"})
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	mux.HandleFunc("POST /v1/community/reports", a.CommunityReport)
	id := "ab334455-1234-4234-8234-123456789abd"
	path := "/v1/community/resources/" + id
	body := `{"id":"` + id + `","kind":"phrase","name":"邮件签名","description":"落款模板","content":{"phrases":[{"text":"此致\n敬礼\n[姓名]","group":"正式"},{"text":"谢谢！","group":""}]},"revision":0}`
	apiRequest(t, mux, "POST", "/v1/community/resources", body, owner.AccessToken, 201)
	apiRequest(t, mux, "POST", "/v1/community/resources", body, owner.AccessToken, 200)
	apiRequest(t, mux, "POST", "/v1/community/resources", strings.Replace(body, `"group":""`, `"group":"","extra":1`, 1), owner.AccessToken, 400)
	apiRequest(t, mux, "POST", "/v1/community/resources", strings.Replace(body, `"text":"谢谢！"`, `"text":"谢\t谢"`, 1), owner.AccessToken, 400)
	apiRequest(t, mux, "POST", "/v1/community/resources", strings.Replace(body, `"kind":"phrase"`, `"kind":"reply"`, 1), owner.AccessToken, 400)
	var list struct {
		Items []CommunityResource `json:"items"`
	}
	if err := json.Unmarshal(apiRequest(t, mux, "GET", "/v1/community/resources?kind=phrase", "", "", 200).Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0].Kind != "phrase" || len(list.Items[0].Content.Phrases) != 2 || list.Items[0].Content.Phrases[0].Group != "正式" {
		t.Fatal("phrase list", list, err)
	}
	// 旧客户端请求 dictionary / reply 时看不到短语包。
	for _, kind := range []string{"dictionary", "reply"} {
		if err := json.Unmarshal(apiRequest(t, mux, "GET", "/v1/community/resources?kind="+kind, "", "", 200).Body.Bytes(), &list); err != nil || len(list.Items) != 0 {
			t.Fatal(kind, "list leaked phrase packs", list, err)
		}
	}
	apiRequest(t, mux, "GET", "/v1/community/resources?kind=phrases", "", "", 400)
	var detail CommunityResource
	if err := json.Unmarshal(apiRequest(t, mux, "GET", path, "", reader.AccessToken, 200).Body.Bytes(), &detail); err != nil || detail.Content.Phrases[0].Text != "此致\n敬礼\n[姓名]" || detail.Owned {
		t.Fatal("phrase detail", detail, err)
	}
	apiRequest(t, mux, "PUT", path+"/save", `{"saved":true}`, reader.AccessToken, 200)
	apiRequest(t, mux, "PUT", path+"/rating", `{"stars":4}`, reader.AccessToken, 200)
	if err := json.Unmarshal(apiRequest(t, mux, "GET", "/v1/community/resources?kind=phrase&scope=saved", "", reader.AccessToken, 200).Body.Bytes(), &list); err != nil || len(list.Items) != 1 || !list.Items[0].Saved || list.Items[0].RatingCount != 1 {
		t.Fatal("saved phrase packs", list, err)
	}
	if w := apiRequest(t, mux, "POST", path+"/apply", `{"resource_revision":1,"dictionary_revision":0}`, reader.AccessToken, 400); !strings.Contains(w.Body.String(), "unsupported_kind") {
		t.Fatal(w.Body.String())
	}
	if w := apiRequest(t, mux, "POST", "/v1/community/reports", `{"kind":"phrases","item_id":"`+id+`","reason":"广告"}`, reader.AccessToken, 201); !strings.Contains(w.Body.String(), "reported") {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "POST", "/v1/community/reports", `{"kind":"replies","item_id":"`+id+`","reason":"广告"}`, reader.AccessToken, 404)
	// 管理后台：短语包分区的列表、计数、详情、作者的其他作品和删除。
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.AdminHTTP(w, r.WithContext(adminTestContext(r.Context(), "test-admin")))
	})
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(apiRequest(t, admin, "GET", "/api/phrases?status=pending", "", "", 200).Body.Bytes(), &page); err != nil || page.Total != 1 || page.Items[0]["entries"] != float64(2) || page.Items[0]["reports"] != float64(1) {
		t.Fatal("admin phrase list", page, err)
	}
	if preview, _ := page.Items[0]["phrases"].([]any); len(preview) != 2 || preview[1] != "谢谢！" {
		t.Fatal("admin phrase preview", page.Items[0])
	}
	if err := json.Unmarshal(apiRequest(t, admin, "GET", "/api/replies", "", "", 200).Body.Bytes(), &page); err != nil || page.Total != 0 {
		t.Fatal("phrase pack listed as a reply", page, err)
	}
	var counts map[string]map[string]int
	if err := json.Unmarshal(apiRequest(t, admin, "GET", "/api/community/counts", "", "", 200).Body.Bytes(), &counts); err != nil || counts["phrases"]["pending"] != 1 || counts["replies"]["pending"] != 0 {
		t.Fatal("admin counts", counts, err)
	}
	other := "ab334455-1234-4234-8234-123456789abe"
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+other+`","kind":"reply","name":"回复","description":"","content":{"prompt":"简短"},"revision":0}`, owner.AccessToken, 201)
	var item map[string]any
	if err := json.Unmarshal(apiRequest(t, admin, "GET", "/api/phrases/"+id, "", "", 200).Body.Bytes(), &item); err != nil || item["report_count"] != float64(1) {
		t.Fatal("admin phrase detail", item, err)
	}
	works, _ := item["owner_items"].([]any)
	if len(works) != 1 || works[0].(map[string]any)["section"] != "replies" {
		t.Fatal("owner items", works)
	}
	apiRequest(t, admin, "GET", "/api/replies/"+id, "", "", 404)
	if err := json.Unmarshal(apiRequest(t, admin, "GET", "/api/users/"+owner.User.ID, "", "", 200).Body.Bytes(), &item); err != nil || item["phrases"] != float64(1) || item["replies"] != float64(1) {
		t.Fatal("admin user counts", item, err)
	}
	sections := map[string]bool{}
	for _, work := range item["works"].([]any) {
		sections[work.(map[string]any)["section"].(string)] = true
	}
	if !sections["phrases"] || !sections["replies"] {
		t.Fatal("admin user works", sections)
	}
	apiRequest(t, admin, "POST", "/api/actions", `{"action":"remove_content","section":"phrases","id":"`+id+`","reason":"广告"}`, "", 200)
	apiRequest(t, mux, "GET", path, "", reader.AccessToken, 404)
	apiRequest(t, admin, "POST", "/api/actions", `{"action":"delete_reply","id":"`+id+`"}`, "", 404)
	apiRequest(t, admin, "POST", "/api/actions", `{"action":"delete_phrase","id":"`+id+`"}`, "", 200)
	apiRequest(t, mux, "GET", path, "", owner.AccessToken, 404)
}

// 早期建的库里 community_resources 和 community_reports 的 kind 约束是自动命名、不含短语包的；迁移按 pg_constraint 查名替换，可以重复执行，替换之前 Ready 让启动走迁移。
func TestResourceKindConstraintUpgrade(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "phrase-upgrade@example.test"})
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, statement := range []string{
		`SELECT pg_advisory_xact_lock(8372419)`,
		`DELETE FROM community_resources WHERE kind='phrase'`,
		`DELETE FROM community_reports WHERE kind='phrases'`,
		`ALTER TABLE community_resources DROP CONSTRAINT community_resources_kind_known`,
		`ALTER TABLE community_resources ADD CONSTRAINT community_resources_kind_check CHECK(kind IN ('dictionary','reply'))`,
		`ALTER TABLE community_reports DROP CONSTRAINT community_reports_kind_known`,
		`ALTER TABLE community_reports ADD CONSTRAINT community_reports_kind_check CHECK(kind IN ('skins','candidate-skins','plugins','dictionaries','replies'))`,
	} {
		if _, err = tx.Exec(t.Context(), statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = db.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "phrase") {
		t.Fatal("Ready passed with the old kind constraints", err)
	}
	insert := `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('ab334455-1234-4234-8234-123456789abf',$1,'phrase','短语','{"phrases":[{"text":"a","group":""}]}')`
	if _, err = db.pool.Exec(t.Context(), insert, owner.User.ID); err == nil {
		t.Fatal("old constraint accepted a phrase pack")
	}
	for range 2 {
		if err = db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	var resourceChecks, reportChecks int
	if err = db.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM pg_constraint WHERE conrelid='community_resources'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%reply%'),(SELECT count(*) FROM pg_constraint WHERE conrelid='community_reports'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%replies%')`).Scan(&resourceChecks, &reportChecks); err != nil || resourceChecks != 1 || reportChecks != 1 {
		t.Fatal("old constraints left behind", resourceChecks, reportChecks, err)
	}
	if _, err = db.pool.Exec(t.Context(), insert, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(t.Context(), `INSERT INTO community_reports(kind,item_id,reporter_id,reason) VALUES('phrases','ab334455-1234-4234-8234-123456789abf',$1,'r')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('ab334455-1234-4234-8234-123456789ab0',$1,'plugin','x','{}')`, owner.User.ID); err == nil {
		t.Fatal("new constraint accepted an unknown kind")
	}
}
