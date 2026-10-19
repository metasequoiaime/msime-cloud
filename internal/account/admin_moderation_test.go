package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// moderationFixture seeds one item per section for owner and returns a console caller with every permission.
func moderationFixture(t *testing.T) (*Store, *Service, Tokens, Tokens, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_audit,community_reports`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	owner := complete(t, db, Identity{"email", "moderated-author@example.test"})
	reader := complete(t, db, Identity{"email", "moderation-reader@example.test"})
	for _, query := range []string{
		`INSERT INTO community_skins(id,owner_id,name,description,design,moderation) VALUES('skin-a',$1,'春日樱','粉色','` + communityFixture + `','pending'),('skin-b',$1,'墨竹','深色','` + communityFixture + `','approved')`,
		`INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES('dict-a',$1,'dictionary','前端术语','{"entries":[{"kind":"pinyin","code":"fangdou","word":"防抖","weight":10},{"kind":"pinyin","code":"shuihe","word":"水合","weight":9}]}','pending'),('reply-a',$1,'reply','委婉拒绝','{"prompt":"今天实在没法加班"}','pending')`,
	} {
		if _, err := db.pool.Exec(ctx, query, owner.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := adminJSONRequest(method, path, body)
		w := httptest.NewRecorder()
		a.AdminHTTP(w, r)
		return w
	}
	return db, a, owner, reader, call
}

func moderationState(t *testing.T, db *Store, table, id string) (state string, previous, reason, by *string) {
	t.Helper()
	if err := db.pool.QueryRow(context.Background(), `SELECT moderation,previous_moderation,moderation_reason,moderated_by FROM `+table+` WHERE id=$1`, id).Scan(&state, &previous, &reason, &by); err != nil {
		t.Fatal(err)
	}
	return state, previous, reason, by
}

func TestModerationActionsApproveRemoveRestore(t *testing.T) {
	db, _, _, _, call := moderationFixture(t)
	ctx := context.Background()
	// A batch removal applies to every id in one transaction and records the state each one replaced.
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","ids":["skin-a","skin-b","skin-missing"],"reason":"侵犯版权或商标：素材来自官方宣传图"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	state, previous, reason, by := moderationState(t, db, "community_skins", "skin-a")
	if state != "removed" || previous == nil || *previous != "pending" || reason == nil || *reason != "侵犯版权或商标：素材来自官方宣传图" || by == nil || *by != "legacy-token" {
		t.Fatal(state, previous, reason, by)
	}
	if state, previous, _, _ = moderationState(t, db, "community_skins", "skin-b"); state != "removed" || *previous != "approved" {
		t.Fatal(state, previous)
	}
	// Removing again keeps the original previous state and only replaces the reason.
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"内容低俗"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, previous, reason, _ = moderationState(t, db, "community_skins", "skin-a"); *previous != "pending" || *reason != "内容低俗" {
		t.Fatal(state, *previous, *reason)
	}
	var detail map[string]any
	var target string
	if err := db.pool.QueryRow(ctx, `SELECT target,detail FROM admin_audit WHERE action='remove_content' ORDER BY id LIMIT 1`).Scan(&target, &detail); err != nil {
		t.Fatal(err)
	}
	if target != "skins" || detail["section"] != "skins" || detail["count"] != float64(2) || detail["reason"] != "侵犯版权或商标：素材来自官方宣传图" {
		t.Fatal(target, detail)
	}
	// Restore puts each row back where the removal found it.
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"skins","ids":["skin-a","skin-b"]}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, previous, reason, _ = moderationState(t, db, "community_skins", "skin-a"); state != "pending" || previous != nil || reason != nil {
		t.Fatal(state, previous, reason)
	}
	if state, _, _, _ = moderationState(t, db, "community_skins", "skin-b"); state != "approved" {
		t.Fatal(state)
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"skins","id":"skin-a"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "not_removed") {
		t.Fatal(w.Code, w.Body.String())
	}
	// Approve, then undo the approval through restore_content with an explicit target state.
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"dictionaries","ids":["dict-a"]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, _, by = moderationState(t, db, "community_resources", "dict-a"); state != "approved" || *by != "legacy-token" {
		t.Fatal(state)
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"dictionaries","id":"dict-a","value":{"to":"pending"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, _, _ = moderationState(t, db, "community_resources", "dict-a"); state != "pending" {
		t.Fatal(state)
	}
	// The section narrows the shared resources table: a reply id is not a dictionary.
	var before int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{`{"action":"approve_content","section":"dictionaries","id":"reply-a"}`, "not_found", 404},
		{`{"action":"approve_content","section":"users","id":"skin-a"}`, "invalid_section", 400},
		{`{"action":"approve_content","section":"skins"}`, "invalid_id", 400},
		{`{"action":"remove_content","section":"skins","id":"skin-a","reason":"  "}`, "invalid_reason", 400},
		{`{"action":"restore_content","section":"skins","id":"skin-a","value":{"to":"removed"}}`, "invalid_value", 400},
		{`{"action":"restore_content","section":"skins","id":"skin-a","value":{"to":"pending","x":1}}`, "invalid_value", 400},
		{`{"action":"restore_content","section":"skins","id":"skin-missing"}`, "not_found", 404},
	} {
		if w := call("POST", "/api/actions", tc.body); w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Fatal(tc.body, w.Code, w.Body.String())
		}
	}
	var after int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit`).Scan(&after); err != nil || after != before {
		t.Fatal("failed actions were audited", before, after, err)
	}
}

func TestModerationActionsRequireReviewCommunity(t *testing.T) {
	db, a, _, _, _ := moderationFixture(t)
	for _, access := range []AdminAccess{
		{Actor: "pat:ops@example.test", Email: "ops@example.test", Role: "operator", Permissions: []string{PermTriageIssues, PermBanUsers}},
		{Actor: "pat:ro@example.test", Email: "ro@example.test", Role: "readonly", Permissions: []string{PermViewCloudUsage}},
	} {
		for _, body := range []string{
			`{"action":"approve_content","section":"skins","id":"skin-a"}`,
			`{"action":"remove_content","section":"skins","id":"skin-a","reason":"x"}`,
			`{"action":"restore_content","section":"skins","id":"skin-a"}`,
		} {
			r := jsonRequest("POST", "/api/actions", body, "")
			w := httptest.NewRecorder()
			a.AdminHTTP(w, r.WithContext(WithAdminAccess(r.Context(), access)))
			if w.Code != 403 {
				t.Fatal(access.Role, body, w.Code)
			}
		}
		// Every role may read the moderation lists and counts.
		r := jsonRequest("GET", "/api/community/counts", "", "")
		w := httptest.NewRecorder()
		a.AdminHTTP(w, r.WithContext(WithAdminAccess(r.Context(), access)))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	reviewer := AdminAccess{Actor: "google:r:reviewer@example.test", Email: "reviewer@example.test", Role: "reviewer", Permissions: []string{PermReviewCommunity}}
	r := jsonRequest("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"skin-a"}`, "")
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r.WithContext(WithAdminAccess(r.Context(), reviewer)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, _, _, by := moderationState(t, db, "community_skins", "skin-a"); *by != reviewer.Actor {
		t.Fatal(*by)
	}
}

func TestModerationListsCountsAndDetail(t *testing.T) {
	db, a, owner, reader, call := moderationFixture(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `UPDATE community_skins SET moderation_reason='命中敏感词：「加V」' WHERE id='skin-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_reports(kind,item_id,reporter_id,reason,detail) VALUES('skins','skin-a',$1,'商标侵权','附截图 2 张')`, reader.User.ID); err != nil {
		t.Fatal(err)
	}
	public, private := "ad334455-1234-4234-8234-123456789abc", "ae334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, public, owner.User.ID, "Public")
	insertCandidateSkin(t, db, private, owner.User.ID, "Private")
	if _, err := db.pool.Exec(ctx, `UPDATE community_candidate_skins SET moderation='pending',visibility=CASE id WHEN $1 THEN 'public' ELSE 'private' END`, public); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			ID, Moderation, Author string
			Flag                   *string
			Reports                int
			Design                 json.RawMessage
		}
		Total int
	}
	w := call("GET", "/api/skins?status=pending", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Total != 1 || list.Items[0].ID != "skin-a" || list.Items[0].Reports != 1 || list.Items[0].Flag == nil || *list.Items[0].Flag != "命中敏感词：「加V」" || len(list.Items[0].Design) == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	for path, total := range map[string]int{"skins": 2, "skins?status=approved": 1, "skins?status=removed": 0, "dictionaries?status=pending": 1, "replies?status=pending": 1, "plugins?status=pending": 0, "candidate-skins?status=pending&visibility=public": 1, "candidate-skins?status=pending": 2} {
		w = call("GET", "/api/"+path, "")
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Total != total {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	if w = call("GET", "/api/dictionaries?status=pending", ""); !strings.Contains(w.Body.String(), `"preview":[`) || !strings.Contains(w.Body.String(), "防抖") {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{"skins?status=hidden", "users?status=pending"} {
		if w = call("GET", "/api/"+path, ""); w.Code != 400 {
			t.Fatal(path, w.Code)
		}
	}
	// Counts leave out private candidate skins, and the shell badge is the sum of pending items.
	w = call("GET", "/api/community/counts", "")
	var counts map[string]map[string]int
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &counts) != nil || counts["skins"]["pending"] != 1 || counts["skins"]["approved"] != 1 || counts["candidate-skins"]["pending"] != 1 || counts["dictionaries"]["pending"] != 1 || counts["replies"]["pending"] != 1 || counts["plugins"]["removed"] != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	if pending, err := a.PendingCommunity(ctx); err != nil || pending != 4 {
		t.Fatal(pending, err)
	}
	// The detail carries reports, live sensitive-word flags and the author's other works.
	w = call("GET", "/api/skins/skin-a", "")
	var detail struct {
		Moderation  string `json:"moderation"`
		ReportCount int    `json:"report_count"`
		Reports     []struct{ Reason, Detail, Reporter string }
		Flags       []SensitiveHit
		OwnerItems  []struct{ Section, ID, Name, Moderation string } `json:"owner_items"`
		OwnerBanned bool                                             `json:"owner_banned"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Moderation != "pending" || detail.ReportCount != 1 || len(detail.Reports) != 1 || detail.Reports[0].Detail != "附截图 2 张" || detail.Flags == nil || len(detail.OwnerItems) != 5 || detail.OwnerBanned {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, item := range detail.OwnerItems {
		if item.Section == "skins" && item.ID == "skin-a" {
			t.Fatal("the item itself is listed as another work")
		}
	}
	if strings.Contains(w.Body.String(), reader.User.ID) || strings.Contains(w.Body.String(), "moderation-reader@example.test") {
		t.Fatal("reporter identity leaked", w.Body.String())
	}
	// The preview route serves the stored image bytes of public and private rows alike.
	for _, id := range []string{public, private} {
		w = call("GET", "/api/candidate-skins/"+id+"/preview", "")
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG")) || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(w.Code, w.Header())
		}
	}
	for path, status := range map[string]int{"candidate-skins/missing/preview": 404, "candidate-skins/a/b/preview": 400} {
		if w = call("GET", "/api/"+path, ""); w.Code != status {
			t.Fatal(path, w.Code)
		}
	}
}

func TestRemovedContentIsHiddenFromThePublicExceptItsOwner(t *testing.T) {
	db, _, owner, reader, call := moderationFixture(t)
	a := &Service{store: db}
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		Mount(mux, a)
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for _, body := range []string{
		`{"action":"remove_content","section":"skins","id":"skin-a","reason":"内容低俗"}`,
		`{"action":"remove_content","section":"dictionaries","id":"dict-a","reason":"含导流或广告"}`,
	} {
		if w := call("POST", "/api/actions", body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		method, path, body, token string
		status                    int
	}{
		{"GET", "/v1/community/skins/skin-a", "", "", 404},
		{"GET", "/v1/community/skins/skin-a", "", reader.AccessToken, 404},
		{"POST", "/v1/community/skins/skin-a/download", `{}`, reader.AccessToken, 404},
		{"GET", "/v1/community/skins/skin-a", "", owner.AccessToken, 200},
		{"POST", "/v1/community/skins/skin-a/download", `{}`, owner.AccessToken, 200},
		{"GET", "/v1/community/skins/skin-b", "", "", 200},
		{"GET", "/v1/community/resources/dict-a", "", reader.AccessToken, 404},
		{"PUT", "/v1/community/resources/dict-a/save", `{"saved":true}`, reader.AccessToken, 404},
		{"GET", "/v1/community/resources/dict-a", "", owner.AccessToken, 200},
	} {
		if w := request(tc.method, tc.path, tc.body, tc.token); w.Code != tc.status {
			t.Fatal(tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	w := request("GET", "/v1/community/skins", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "skin-a") || !strings.Contains(w.Body.String(), "skin-b") {
		t.Fatal(w.Body.String())
	}
	if w = request("GET", "/v1/community/skins", "", owner.AccessToken); !strings.Contains(w.Body.String(), "skin-a") {
		t.Fatal("owner lost their removed skin", w.Body.String())
	}
	if w = request("GET", "/v1/community/resources?kind=dictionary", "", ""); strings.Contains(w.Body.String(), "dict-a") {
		t.Fatal(w.Body.String())
	}
	var stats CommunityStats
	if w = request("GET", "/v1/community/stats", "", ""); json.Unmarshal(w.Body.Bytes(), &stats) != nil || stats.Skins != 1 || stats.Dictionaries != 0 || stats.Replies != 1 {
		t.Fatal(w.Body.String())
	}
	// A restore makes it public again.
	if w = call("POST", "/api/actions", `{"action":"restore_content","section":"skins","id":"skin-a"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("GET", "/v1/community/skins/skin-a", "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestCommunityUploadsArePendingAndAuthorEditsGoBackToReview(t *testing.T) {
	db, _, owner, _, call := moderationFixture(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	id := "af334455-1234-4234-8234-123456789abc"
	if w := apiRequest(t, mux, "POST", "/v1/community/skins", `{"id":"`+id+`","name":"秋色","description":"示例","design":`+communityFixture+`}`, owner.AccessToken, 201); w.Code != 201 {
		t.Fatal(w.Code)
	}
	if state, _, reason, _ := moderationState(t, db, "community_skins", id); state != "pending" || reason != nil {
		t.Fatal(state, reason)
	}
	// Public at once: post-moderation never hides a pending upload.
	apiRequest(t, mux, "GET", "/v1/community/skins/"+id, "", "", 200)
	resource := "b0334455-1234-4234-8234-123456789abc"
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+resource+`","kind":"reply","name":"模板","description":"","content":{"prompt":"礼貌回复"}}`, owner.AccessToken, 201)
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"replies","id":"`+resource+`"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+resource+`","kind":"reply","name":"模板","description":"","content":{"prompt":"更礼貌地回复"},"revision":1}`, owner.AccessToken, 200)
	if state, _, _, _ := moderationState(t, db, "community_resources", resource); state != "pending" {
		t.Fatal("an edited item skipped review", state)
	}
	// An edit does not undo a removal.
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"replies","id":"`+resource+`","reason":"质量不达标"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+resource+`","kind":"reply","name":"模板","description":"","content":{"prompt":"第三版"},"revision":2}`, owner.AccessToken, 200)
	if state, previous, reason, _ := moderationState(t, db, "community_resources", resource); state != "removed" || *previous != "pending" || *reason != "质量不达标" {
		t.Fatal(state, previous, reason)
	}
}

type fakeMatcher struct {
	hits []SensitiveHit
	err  error
	text string
}

func (m *fakeMatcher) Match(_ context.Context, text string) ([]SensitiveHit, error) {
	m.text = text
	return m.hits, m.err
}

func TestScreenCommunityText(t *testing.T) {
	ctx := context.Background()
	m := &fakeMatcher{}
	if blocked, flag, err := screenCommunityText(ctx, m, "名字", "简介"); blocked || flag != nil || err != nil || m.text != "名字\n简介" {
		t.Fatal(blocked, flag, err, m.text)
	}
	m.hits = []SensitiveHit{{Pattern: "加V", Level: SensitiveReview}, {Pattern: "加V", Level: SensitiveReview}, {Pattern: "代练", Level: SensitiveReview}}
	if blocked, flag, err := screenCommunityText(ctx, m, "代练加V"); blocked || err != nil || flag == nil || *flag != "命中敏感词：「加V」「代练」" {
		t.Fatal(blocked, flag, err)
	}
	m.hits = append(m.hits, SensitiveHit{Pattern: "赌博", Level: SensitiveBlock})
	if blocked, flag, err := screenCommunityText(ctx, m, "x"); !blocked || flag != nil || err != nil {
		t.Fatal(blocked, flag, err)
	}
	m.err = errors.New("down")
	if _, _, err := screenCommunityText(ctx, m, "x"); err == nil {
		t.Fatal("matcher failure ignored")
	}
	if got := resourceScreenText(ResourceContent{Entries: []SharedWord{{Word: "防抖"}, {Word: "水合"}}}); got != "防抖\n水合" {
		t.Fatal(got)
	}
	if got := resourceScreenText(ResourceContent{Prompt: "提示"}); got != "提示" {
		t.Fatal(got)
	}
}

func TestCommunityReport(t *testing.T) {
	db, _, owner, reader, call := moderationFixture(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/community/reports", Route(a, "POST /v1/community/reports", (*Service).CommunityReport))
	body := `{"kind":"skins","item_id":"skin-a","reason":"商标侵权","detail":"素材来自官方宣传图"}`
	since := lastNotificationID(t, db)
	apiRequest(t, mux, "POST", "/v1/community/reports", body, "", 401)
	apiRequest(t, mux, "POST", "/v1/community/reports", body, reader.AccessToken, 201)
	// A repeat report from the same account is accepted without a second record.
	apiRequest(t, mux, "POST", "/v1/community/reports", strings.Replace(body, "商标侵权", "其他", 1), reader.AccessToken, 200)
	apiRequest(t, mux, "POST", "/v1/community/reports", body, owner.AccessToken, 201)
	var count int
	if err := db.pool.QueryRow(context.Background(), `SELECT count(*) FROM community_reports WHERE kind='skins' AND item_id='skin-a'`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	// Each new report, and not the repeat, reaches the console bell and opens the item on the community page.
	if got := notificationTargets(t, db, NotifyReport, since); !slices.Equal(got, []string{"community skins/skin-a", "community skins/skin-a"}) {
		t.Fatal("report notifications", got)
	}
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{`{"kind":"users","item_id":"skin-a","reason":"x"}`, "invalid_report_kind", 400},
		{`{"kind":"skins","item_id":"","reason":"x"}`, "invalid_id", 400},
		{`{"kind":"skins","item_id":"a/b","reason":"x"}`, "invalid_id", 400},
		{`{"kind":"skins","item_id":"skin-a","reason":" "}`, "invalid_report_reason", 400},
		{`{"kind":"skins","item_id":"skin-a","reason":"` + strings.Repeat("长", 65) + `"}`, "invalid_report_reason", 400},
		{`{"kind":"skins","item_id":"skin-a","reason":"x","detail":"` + strings.Repeat("长", 1001) + `"}`, "invalid_report_detail", 400},
		{`{"kind":"skins","item_id":"skin-a","reason":"x","extra":1}`, "invalid_json", 400},
		{`{"kind":"skins","item_id":"skin-missing","reason":"x"}`, "item_not_found", 404},
		{`{"kind":"dictionaries","item_id":"reply-a","reason":"x"}`, "item_not_found", 404},
	} {
		if w := apiRequest(t, mux, "POST", "/v1/community/reports", tc.body, reader.AccessToken, tc.status); !strings.Contains(w.Body.String(), tc.code) {
			t.Fatal(tc.body, w.Body.String())
		}
	}
	// Removed items and private candidate skins cannot be reported.
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"replies","id":"reply-a","reason":"内容低俗"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	apiRequest(t, mux, "POST", "/v1/community/reports", `{"kind":"replies","item_id":"reply-a","reason":"x"}`, reader.AccessToken, 404)
	private := "b1334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, private, owner.User.ID, "Private")
	if _, err := db.pool.Exec(context.Background(), `UPDATE community_candidate_skins SET visibility='private' WHERE id=$1`, private); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "POST", "/v1/community/reports", `{"kind":"candidate-skins","item_id":"`+private+`","reason":"x"}`, reader.AccessToken, 404)
	// The list and counts surface the reports.
	if w := call("GET", "/api/skins?status=pending", ""); !strings.Contains(w.Body.String(), `"reports":2`) {
		t.Fatal(w.Body.String())
	}
	// Reporting is rate limited per account.
	for i := 0; i < communityReportsPerHour; i++ {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, jsonRequest("POST", "/v1/community/reports", body, reader.AccessToken))
		if w.Code == 429 {
			return
		}
	}
	t.Fatal("reports are not rate limited")
}

func TestRemovedCandidateSkinsAndPluginsAreHidden(t *testing.T) {
	db, _, owner, reader, call := moderationFixture(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	id := "b2334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, id, owner.User.ID, "Gallery")
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id, "", reader.AccessToken, 200)
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"candidate-skins","id":"`+id+`","reason":"侵犯版权或商标"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id, "", reader.AccessToken, 404)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id+"/preview", "", reader.AccessToken, 404)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+id+"/download", `{}`, reader.AccessToken, 404)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+id+"/rating", `{"stars":5}`, reader.AccessToken, 404)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id+"/preview", "", owner.AccessToken, 200)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+id+"/download", `{}`, owner.AccessToken, 200)
	if w := apiRequest(t, mux, "GET", "/v1/community/candidate-skins", "", reader.AccessToken, 200); strings.Contains(w.Body.String(), id) {
		t.Fatal(w.Body.String())
	}
	// A private library row that goes public enters the review queue.
	private := "b3334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, private, owner.User.ID, "Library")
	if _, err := db.pool.Exec(context.Background(), `UPDATE community_candidate_skins SET visibility='private' WHERE id=$1`, private); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+private, `{"visibility":"public"}`, owner.AccessToken, 200)
	if state, _, _, _ := moderationState(t, db, "community_candidate_skins", private); state != "pending" {
		t.Fatal(state)
	}
	// Plugins follow the same rule.
	if _, err := db.pool.Exec(context.Background(), `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256,moderation) VALUES('b4334455-1234-4234-8234-123456789abc',$1,'sound','com.example.pack','Pack','1.0','MIT','{}','zip',repeat('a',64),'removed')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "GET", "/v1/community/plugins/b4334455-1234-4234-8234-123456789abc", "", reader.AccessToken, 404)
	apiRequest(t, mux, "GET", "/v1/community/plugins/b4334455-1234-4234-8234-123456789abc", "", owner.AccessToken, 200)
	if w := apiRequest(t, mux, "GET", "/v1/community/plugins", "", "", 200); strings.Contains(w.Body.String(), "b4334455") {
		t.Fatal(w.Body.String())
	}
}

func TestModerationUndoEdgesAndBannedOwners(t *testing.T) {
	db, _, owner, _, call := moderationFixture(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `UPDATE community_skins SET moderation_reason='命中敏感词：「加V」' WHERE id='skin-a'`); err != nil {
		t.Fatal(err)
	}
	// Approving a pending row keeps its automatic flag, so undoing the approval brings the warning back; the audit names the single item for the activity text.
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"skin-a"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var detail map[string]any
	if err := db.pool.QueryRow(ctx, `SELECT detail FROM admin_audit WHERE action='approve_content' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail["name"] != "春日樱" || detail["count"] != float64(1) || len(detail["ids"].([]any)) != 1 || detail["ids"].([]any)[0] != "skin-a" {
		t.Fatal(detail)
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"skins","id":"skin-a","value":{"to":"pending"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, reason, _ := moderationState(t, db, "community_skins", "skin-a"); state != "pending" || reason == nil || *reason != "命中敏感词：「加V」" {
		t.Fatal(state, reason)
	}
	// Undoing an approval never brings back a row someone removed in the meantime.
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"内容低俗"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"skins","id":"skin-a","value":{"to":"approved"}}`); w.Code != 409 || !strings.Contains(w.Body.String(), `"conflict"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, _, _ := moderationState(t, db, "community_skins", "skin-a"); state != "removed" {
		t.Fatal(state)
	}
	// A removed row of a banned author comes back only through the unban, not through restore or approve.
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"action":"restore_content","section":"skins","id":"skin-a"}`,
		`{"action":"approve_content","section":"skins","ids":["skin-a","skin-b"]}`,
		// Any row of a banned author is refused, not only removed ones.
		`{"action":"approve_content","section":"dictionaries","id":"dict-a"}`,
	} {
		if w := call("POST", "/api/actions", body); w.Code != 409 || !strings.Contains(w.Body.String(), "owner_banned") {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if state, _, _, _ := moderationState(t, db, "community_skins", "skin-a"); state != "removed" {
		t.Fatal(state)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=NULL WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"skins","id":"skin-a"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestEditOfRemovedContentRestoresToPending(t *testing.T) {
	db, _, owner, _, call := moderationFixture(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	resource := "b5334455-1234-4234-8234-123456789abc"
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+resource+`","kind":"reply","name":"模板","description":"","content":{"prompt":"礼貌回复"}}`, owner.AccessToken, 201)
	for _, body := range []string{
		`{"action":"approve_content","section":"replies","id":"` + resource + `"}`,
		`{"action":"remove_content","section":"replies","id":"` + resource + `","reason":"质量不达标"}`,
	} {
		if w := call("POST", "/api/actions", body); w.Code != 200 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	// The edit happens after the removal, so a later restore must not publish it as approved without a review.
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+resource+`","kind":"reply","name":"模板","description":"","content":{"prompt":"改过的内容"},"revision":1}`, owner.AccessToken, 200)
	if state, previous, reason, _ := moderationState(t, db, "community_resources", resource); state != "removed" || previous == nil || *previous != "pending" || *reason != "质量不达标" {
		t.Fatal(state, previous, reason)
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"replies","id":"`+resource+`"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, _, _ := moderationState(t, db, "community_resources", resource); state != "pending" {
		t.Fatal(state)
	}
}

func TestModerationUndoKeepsFlagsAndNeverRevivesRemovals(t *testing.T) {
	db, _, _, _, call := moderationFixture(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `UPDATE community_skins SET moderation_reason='命中敏感词：「加V」' WHERE id='skin-a'`); err != nil {
		t.Fatal(err)
	}
	// Approving a flagged pending row keeps the automatic flag, so undoing the approval shows the warning again.
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"skin-a"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, reason, _ := moderationState(t, db, "community_skins", "skin-a"); state != "approved" || reason == nil || *reason != "命中敏感词：「加V」" {
		t.Fatal(state, reason)
	}
	var list struct{ Items []struct{ Flag *string } }
	if w := call("GET", "/api/skins?status=approved&q=skin-a", ""); json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Items) != 1 || list.Items[0].Flag != nil {
		t.Fatal("an approved row still shows the flag", w.Body.String())
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"skins","id":"skin-a","value":{"to":"pending"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, reason, _ := moderationState(t, db, "community_skins", "skin-a"); state != "pending" || reason == nil || *reason != "命中敏感词：「加V」" {
		t.Fatal(state, reason)
	}
	// Approving a removed row clears the removal reason.
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-b","reason":"内容低俗"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"skin-b"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, previous, reason, _ := moderationState(t, db, "community_skins", "skin-b"); state != "approved" || previous != nil || reason != nil {
		t.Fatal(state, previous, reason)
	}
	// An approval undo that arrives after another moderator removed the item does not bring it back.
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-b","reason":"含导流或广告"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"skins","id":"skin-b","value":{"to":"approved"}}`); w.Code != 409 || !strings.Contains(w.Body.String(), `"conflict"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, reason, _ := moderationState(t, db, "community_skins", "skin-b"); state != "removed" || *reason != "含导流或广告" {
		t.Fatal(state, reason)
	}
	// The audit names the changed ids, and the item name when there is one, for the console's activity text.
	var detail map[string]any
	if err := db.pool.QueryRow(ctx, `SELECT detail FROM admin_audit WHERE action='approve_content' ORDER BY id LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if ids, _ := detail["ids"].([]any); len(ids) != 1 || ids[0] != "skin-a" || detail["name"] != "春日樱" || detail["count"] != float64(1) {
		t.Fatal(detail)
	}
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","ids":["skin-a","skin-b","skin-a"]}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := db.pool.QueryRow(ctx, `SELECT detail FROM admin_audit WHERE action='approve_content' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if ids, _ := detail["ids"].([]any); len(ids) != 2 || ids[0] != "skin-a" || ids[1] != "skin-b" || detail["name"] != nil {
		t.Fatal(detail)
	}
}

// The page search matches what a moderator sees, not the shape of the row: field names and a skin's design never match, names and other values do.
func TestAdminListSearchMatchesValuesOnly(t *testing.T) {
	_, _, _, _, call := moderationFixture(t)
	total := func(path string) int {
		w := call("GET", path, "")
		var page struct {
			Total int `json:"total"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(path, w.Code, w.Body.String())
		}
		return page.Total
	}
	for path, want := range map[string]int{
		"/api/skins?q=keyBackground":  0, // a design key
		"/api/skins?q=15266027":       0, // a design value
		"/api/skins?q=moderation":     0, // a column name
		"/api/skins?q=樱":              1,
		"/api/skins?q=skin-b":         1,
		"/api/skins?q=pending":        1,
		"/api/dictionaries?q=entries": 0,
		"/api/dictionaries?q=防抖":      1, // the preview entries stay searchable
		"/api/replies?q=加班":           1,
	} {
		if got := total(path); got != want {
			t.Errorf("%s: %d want %d", path, got, want)
		}
	}
}

// Restoring a removed item that goes back to review, by restore_content or by unbanning its author, puts its automatic flag back; an approved item gets none.
func TestRestoredPendingItemsAreScreenedAgain(t *testing.T) {
	db, a, owner, _, call := moderationFixture(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_sensitive_words,admin_sensitive_hits`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `TRUNCATE admin_sensitive_words,admin_sensitive_hits`)
	})
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_sensitive_words(pattern,category,level,created_by) VALUES('春日','ad','review','test'),('墨竹','ad','review','test'),('加班','ad','review','test')`); err != nil {
		t.Fatal(err)
	}
	a.sensitive.invalidate()
	reason := func(table, id string) string {
		_, _, r, _ := moderationState(t, db, table, id)
		if r == nil {
			return ""
		}
		return *r
	}
	for _, body := range []string{
		`{"action":"remove_content","section":"skins","ids":["skin-a","skin-b"],"reason":"内容低俗"}`,
		`{"action":"restore_content","section":"skins","ids":["skin-a","skin-b"]}`,
	} {
		if w := call("POST", "/api/actions", body); w.Code != 200 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if got := reason("community_skins", "skin-a"); got != "命中敏感词：「春日」" {
		t.Fatal("pending skin lost its flag", got)
	}
	if got := reason("community_skins", "skin-b"); got != "" {
		t.Fatal("approved skin got a flag", got)
	}
	// A ban removes the pending reply and overwrites its reason; the unban restores it to review with its flag.
	if w := call("POST", "/api/actions", `{"action":"ban_user","id":"`+owner.User.ID+`","reason":"spam"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := reason("community_resources", "reply-a"); got != banModerationReason {
		t.Fatal(got)
	}
	if w := call("POST", "/api/actions", `{"action":"unban_user","id":"`+owner.User.ID+`"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := reason("community_resources", "reply-a"); got != "命中敏感词：「加班」" {
		t.Fatal("reply lost its flag after unban", got)
	}
	// Restoring is not a submission, so it counts no hits.
	a.sensitive.mu.Lock()
	pending := len(a.sensitive.pending)
	a.sensitive.mu.Unlock()
	if pending != 0 {
		t.Fatal("restores counted sensitive hits", pending)
	}
}

// 审核通过等待并发封禁持有的作者行锁，封禁提交后拒绝重新公开作品。
func TestApproveWaitsForConcurrentBan(t *testing.T) {
	db, _, owner, _, call := moderationFixture(t)
	ctx := t.Context()
	ban, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ban.Rollback(ctx)
	if _, err = ban.Exec(ctx, `SELECT 1 FROM auth_users WHERE id=$1 FOR UPDATE`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ban.Exec(ctx, `UPDATE auth_users SET banned_at=now(),ban_reason='race' WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ban.Exec(ctx, `UPDATE community_skins SET previous_moderation=moderation,moderation='removed',moderation_reason='owner_banned' WHERE owner_id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- call("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"skin-a"}`)
	}()
	waitForAdminDatabaseLock(t, db, ban.Conn().PgConn().PID())
	if err = ban.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if w := <-result; w.Code != 409 || !strings.Contains(w.Body.String(), "owner_banned") {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, reason, _ := moderationState(t, db, "community_skins", "skin-a"); state != "removed" || reason == nil || *reason != "owner_banned" {
		t.Fatal(state, reason)
	}
}

// A publish whose session was checked before the ban cannot write once the ban has committed: the write transaction reads banned_at under the author's row lock.
func TestUserDataTransactionRefusesBannedUser(t *testing.T) {
	db, _, owner, _, _ := moderationFixture(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.pool.Exec(context.Background(), `UPDATE auth_users SET banned_at=NULL WHERE id=$1`, owner.User.ID)
	})
	if tx, err := db.userDataTransaction(ctx, owner.User.ID); !errors.Is(err, ErrBanned) {
		if tx != nil {
			tx.Rollback(ctx)
		}
		t.Fatal(err)
	}
}

// A stale approval pinned to the state and version the moderator saw changes nothing once another moderator removed the item or the author edited it.
func TestPinnedApprovalRefusesStaleTargets(t *testing.T) {
	db, a, owner, _, call := moderationFixture(t)
	ctx := context.Background()
	// Another moderator rejected skin-a while this one still saw it as pending.
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"侵犯版权或商标"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"skin-a","value":{"from":"pending"}}`); w.Code != 409 || !strings.Contains(w.Body.String(), `"conflict"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, reason, _ := moderationState(t, db, "community_skins", "skin-a"); state != "removed" || reason == nil || *reason != "侵犯版权或商标" {
		t.Fatal("a stale approval republished a removed item", state, reason)
	}
	// Approving the removal deliberately, from the state it is in, still works.
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"skin-a","value":{"from":"removed"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	// The moderator reviewed version 1 of a reply; the author then republished new content.
	mux := http.NewServeMux()
	Mount(mux, a)
	resource := "b6334455-1234-4234-8234-123456789abc"
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+resource+`","kind":"reply","name":"模板","description":"","content":{"prompt":"礼貌回复"}}`, owner.AccessToken, 201)
	var list struct {
		Items []struct {
			UpdatedAt string `json:"updated_at"`
		}
	}
	if w := call("GET", "/api/replies?q="+resource, ""); json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Items) != 1 || list.Items[0].UpdatedAt == "" {
		t.Fatal(w.Body.String())
	}
	reviewed := list.Items[0].UpdatedAt
	time.Sleep(2 * time.Millisecond)
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+resource+`","kind":"reply","name":"模板","description":"","content":{"prompt":"没人审过的新内容"},"revision":1}`, owner.AccessToken, 200)
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"replies","id":"`+resource+`","value":{"from":"pending","updated_at":"`+reviewed+`"}}`); w.Code != 409 {
		t.Fatal("an unreviewed edit was approved", w.Code, w.Body.String())
	}
	if state, _, _, _ := moderationState(t, db, "community_resources", resource); state != "pending" {
		t.Fatal(state)
	}
	// The detail's updated_at is the same value the list carries, so a pin from either matches the current version.
	var detail struct {
		UpdatedAt string `json:"updated_at"`
	}
	if w := call("GET", "/api/replies/"+resource, ""); json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.UpdatedAt == "" {
		t.Fatal(w.Body.String())
	}
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"replies","id":"`+resource+`","value":{"from":"pending","updated_at":"`+detail.UpdatedAt+`"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	// A pinned batch is all or nothing.
	if _, err := db.pool.Exec(ctx, `UPDATE community_skins SET moderation='pending' WHERE id='skin-a'`); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","ids":["skin-a","skin-b"],"value":{"from":"pending"}}`); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, _, _ := moderationState(t, db, "community_skins", "skin-a"); state != "pending" {
		t.Fatal("a refused batch approved part of its items", state)
	}
	for _, body := range []string{
		`{"action":"approve_content","section":"skins","id":"skin-a","value":{"from":"approved"}}`,
		`{"action":"approve_content","section":"skins","id":"skin-a","value":{"from":"pending","updated_at":"2026-01-01T00:00:00Z"}}`,
		`{"action":"approve_content","section":"skins","id":"skin-a","value":{"from":"pending","extra":1}}`,
	} {
		if w := call("POST", "/api/actions", body); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_value") {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
}

// A stale approval pinned to a row's created_at changes nothing once the author deleted the item and published different content under the same id.
func TestPinnedApprovalRefusesRepublishedItem(t *testing.T) {
	_, a, owner, _, call := moderationFixture(t)
	mux := http.NewServeMux()
	Mount(mux, a)
	skin := "b9334455-1234-4234-8234-123456789abc"
	apiRequest(t, mux, "POST", "/v1/community/skins", `{"id":"`+skin+`","name":"晚霞","description":"示例","design":`+communityFixture+`}`, owner.AccessToken, 201)
	var list struct {
		Items []struct {
			CreatedAt string `json:"created_at"`
		}
	}
	if w := call("GET", "/api/skins?q="+skin, ""); json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Items) != 1 || list.Items[0].CreatedAt == "" {
		t.Fatal(w.Body.String())
	}
	reviewed := list.Items[0].CreatedAt
	time.Sleep(2 * time.Millisecond)
	apiRequest(t, mux, "DELETE", "/v1/community/skins/"+skin, "", owner.AccessToken, 200)
	apiRequest(t, mux, "POST", "/v1/community/skins", `{"id":"`+skin+`","name":"没人审过","description":"新内容","design":`+communityFixture+`}`, owner.AccessToken, 201)
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"`+skin+`","value":{"from":"pending","created_at":"`+reviewed+`"}}`); w.Code != 409 {
		t.Fatal("a republished item nobody reviewed was approved", w.Code, w.Body.String())
	}
	var detail struct {
		CreatedAt string `json:"created_at"`
	}
	if w := call("GET", "/api/skins/"+skin, ""); json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.CreatedAt == "" {
		t.Fatal(w.Body.String())
	}
	if w := call("POST", "/api/actions", `{"action":"approve_content","section":"skins","id":"`+skin+`","value":{"from":"pending","created_at":"`+detail.CreatedAt+`"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

// Undoing the approval of a rejected item puts it back with the restore state it had, so a later restore returns it to review instead of publishing it.
func TestUndoingApprovalOfRemovedItemKeepsItsRestoreState(t *testing.T) {
	db, _, _, _, call := moderationFixture(t)
	for _, body := range []string{
		`{"action":"remove_content","section":"skins","id":"skin-a","reason":"内容低俗"}`,
		`{"action":"approve_content","section":"skins","id":"skin-a","value":{"from":"removed"}}`,
		`{"action":"remove_content","section":"skins","id":"skin-a","reason":"内容低俗","value":{"previous":"pending"}}`,
	} {
		if w := call("POST", "/api/actions", body); w.Code != 200 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if state, previous, _, _ := moderationState(t, db, "community_skins", "skin-a"); state != "removed" || previous == nil || *previous != "pending" {
		t.Fatal(state, previous)
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"skins","id":"skin-a"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if state, _, _, _ := moderationState(t, db, "community_skins", "skin-a"); state != "pending" {
		t.Fatal("a restore published an item that was never approved", state)
	}
	// Removing an already removed item again keeps the first removal's restore state, whatever previous says.
	for _, body := range []string{
		`{"action":"remove_content","section":"skins","id":"skin-b","reason":"内容低俗"}`,
		`{"action":"remove_content","section":"skins","id":"skin-b","reason":"质量不达标","value":{"previous":"pending"}}`,
	} {
		if w := call("POST", "/api/actions", body); w.Code != 200 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if _, previous, _, _ := moderationState(t, db, "community_skins", "skin-b"); previous == nil || *previous != "approved" {
		t.Fatal(previous)
	}
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"内容低俗","value":{"previous":"removed"}}`); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

// A retry of a publication that is already live succeeds even after a block-level word that matches it was added, and counts no hits.
func TestPublishRetryIsAnsweredBeforeScreening(t *testing.T) {
	db, a, owner, _, _ := moderationFixture(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_sensitive_words,admin_sensitive_hits`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `TRUNCATE admin_sensitive_words,admin_sensitive_hits`)
	})
	mux := http.NewServeMux()
	Mount(mux, a)
	skin := "b7334455-1234-4234-8234-123456789abc"
	resource := "b8334455-1234-4234-8234-123456789abc"
	skinBody := `{"id":"` + skin + `","name":"晚霞","description":"示例","design":` + communityFixture + `}`
	resourceBody := `{"id":"` + resource + `","kind":"reply","name":"晚霞模板","description":"","content":{"prompt":"礼貌回复"}}`
	apiRequest(t, mux, "POST", "/v1/community/skins", skinBody, owner.AccessToken, 201)
	apiRequest(t, mux, "POST", "/v1/community/resources", resourceBody, owner.AccessToken, 201)
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_sensitive_words(pattern,category,level,created_by) VALUES('晚霞','ad','block','test')`); err != nil {
		t.Fatal(err)
	}
	a.sensitive.invalidate()
	apiRequest(t, mux, "POST", "/v1/community/skins", skinBody, owner.AccessToken, 200)
	apiRequest(t, mux, "POST", "/v1/community/resources", resourceBody, owner.AccessToken, 200)
	a.sensitive.mu.Lock()
	pending := len(a.sensitive.pending)
	a.sensitive.mu.Unlock()
	if pending != 0 {
		t.Fatal("retries counted sensitive hits", pending)
	}
	// A changed upload is still screened.
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+resource+`","kind":"reply","name":"晚霞模板","description":"","content":{"prompt":"新内容"},"revision":1}`, owner.AccessToken, 422)
}

// 作者只有在客户端用 `fields=moderation` 显式请求时才能看到自己作品的审核状态，其他人永远看不到；不带该参数的请求响应与以前完全相同，因为已发布的客户端拒绝未知字段。
func TestAuthorSeesModerationStateOnlyWhenAsked(t *testing.T) {
	db, _, owner, reader, call := moderationFixture(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"内容低俗"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	candidate, plugin, readerPlugin := "c1334455-1234-4234-8234-123456789abc", "c2334455-1234-4234-8234-123456789abc", "c3334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, candidate, owner.User.ID, "Owned candidate")
	for _, row := range []struct{ id, owner, state string }{{plugin, owner.User.ID, "removed"}, {readerPlugin, reader.User.ID, "approved"}} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256,moderation) VALUES($1,$2,'sound','com.example.`+row.id[:2]+`','Pack','1.0','MIT','{}','zip',repeat('a',64),$3)`, row.id, row.owner, row.state); err != nil {
			t.Fatal(err)
		}
	}
	states := func(body []byte, list string) map[string]any {
		t.Helper()
		var page map[string]json.RawMessage
		var items []map[string]any
		if err := json.Unmarshal(body, &page); err != nil || json.Unmarshal(page[list], &items) != nil {
			t.Fatal(string(body), err)
		}
		out := map[string]any{}
		for _, item := range items {
			out[item["id"].(string)] = item["moderation"]
		}
		return out
	}
	// 不带开关时什么都不变，作者本人也一样。
	for _, path := range []string{"/v1/community/skins", "/v1/community/skins/skin-a", "/v1/community/plugins", "/v1/community/plugins/" + plugin, "/v1/community/candidate-skins", "/v1/community/candidate-skins/" + candidate, "/v1/community/resources?kind=dictionary", "/v1/community/resources/dict-a"} {
		if w := apiRequest(t, mux, "GET", path, "", owner.AccessToken, 200); strings.Contains(w.Body.String(), `"moderation"`) {
			t.Fatal("moderation sent without fields=moderation", path, w.Body.String())
		}
	}
	// 作者自己的作品带状态，其他用户的作品永远不带。
	got := states(apiRequest(t, mux, "GET", "/v1/community/skins?fields=moderation", "", owner.AccessToken, 200).Body.Bytes(), "skins")
	if got["skin-a"] != "removed" || got["skin-b"] != "approved" {
		t.Fatal("owner skin states", got)
	}
	if w := apiRequest(t, mux, "GET", "/v1/community/skins?fields=moderation", "", reader.AccessToken, 200); strings.Contains(w.Body.String(), `"moderation"`) || strings.Contains(w.Body.String(), "skin-a") {
		t.Fatal("another user saw a review state or the removed skin", w.Body.String())
	}
	if w := apiRequest(t, mux, "GET", "/v1/community/skins/skin-b?fields=moderation", "", "", 200); strings.Contains(w.Body.String(), `"moderation"`) {
		t.Fatal("anonymous viewer saw a review state", w.Body.String())
	}
	if w := apiRequest(t, mux, "GET", "/v1/community/skins/skin-a?fields=moderation", "", owner.AccessToken, 200); !strings.Contains(w.Body.String(), `"moderation":"removed"`) || strings.Contains(w.Body.String(), "内容低俗") {
		t.Fatal("owner detail", w.Body.String())
	}
	got = states(apiRequest(t, mux, "GET", "/v1/community/plugins?fields=moderation", "", owner.AccessToken, 200).Body.Bytes(), "plugins")
	if got[plugin] != "removed" || got[readerPlugin] != nil {
		t.Fatal("plugin states", got)
	}
	if w := apiRequest(t, mux, "GET", "/v1/community/plugins/"+plugin+"?fields=moderation", "", owner.AccessToken, 200); !strings.Contains(w.Body.String(), `"moderation":"removed"`) {
		t.Fatal("owner plugin detail", w.Body.String())
	}
	// 候选皮肤可以单独请求 moderation，也可以和 sync 一起请求；单独请求 moderation 时仍不带同步字段。
	w := apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+candidate+"?fields=moderation", "", owner.AccessToken, 200)
	if !strings.Contains(w.Body.String(), `"moderation":"approved"`) || strings.Contains(w.Body.String(), `"visibility"`) {
		t.Fatal("candidate detail", w.Body.String())
	}
	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins?scope=mine&fields=sync,moderation", "", owner.AccessToken, 200)
	if !strings.Contains(w.Body.String(), `"moderation":"approved"`) || !strings.Contains(w.Body.String(), `"visibility":"public"`) {
		t.Fatal("candidate list with both opt-ins", w.Body.String())
	}
	got = states(apiRequest(t, mux, "GET", "/v1/community/resources?kind=dictionary&scope=mine&fields=moderation", "", owner.AccessToken, 200).Body.Bytes(), "items")
	if got["dict-a"] != "pending" {
		t.Fatal("resource states", got)
	}
	if w = apiRequest(t, mux, "GET", "/v1/community/resources/dict-a?fields=moderation", "", reader.AccessToken, 200); strings.Contains(w.Body.String(), `"moderation"`) {
		t.Fatal("reader saw a resource state", w.Body.String())
	}
	for _, path := range []string{"/v1/community/skins?fields=reason", "/v1/community/skins/skin-a?fields=moderation,reason", "/v1/community/plugins?fields=sync", "/v1/community/plugins/" + plugin + "?fields=x", "/v1/community/candidate-skins?fields=reason", "/v1/community/resources?kind=reply&fields=sync", "/v1/community/resources/dict-a?fields=MODERATION"} {
		if w = apiRequest(t, mux, "GET", path, "", owner.AccessToken, 400); !strings.Contains(w.Body.String(), "invalid_fields") {
			t.Fatal(path, w.Body.String())
		}
	}
}

// 皮肤和插件用 `scope=mine` 列出作者自己的上传（含已下架的），需要用户会话。
func TestSkinAndPluginScopeMine(t *testing.T) {
	db, _, owner, reader, call := moderationFixture(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-a","reason":"内容低俗"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,description,design) VALUES('skin-r',$1,'读者的皮肤','','`+communityFixture+`')`, reader.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, owner string }{{"d1334455-1234-4234-8234-123456789abc", owner.User.ID}, {"d2334455-1234-4234-8234-123456789abc", reader.User.ID}} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES($1,$2,'music','com.example.`+row.id[:2]+`','Pack','1.0','MIT','{}','zip',repeat('a',64))`, row.id, row.owner); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(body []byte, list string) []string {
		t.Helper()
		var page map[string]json.RawMessage
		var items []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &page); err != nil || json.Unmarshal(page[list], &items) != nil {
			t.Fatal(string(body), err)
		}
		var out []string
		for _, item := range items {
			out = append(out, item.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(apiRequest(t, mux, "GET", "/v1/community/skins?scope=mine", "", owner.AccessToken, 200).Body.Bytes(), "skins"); !slices.Equal(got, []string{"skin-a", "skin-b"}) {
		t.Fatal("owner skins", got)
	}
	if got := ids(apiRequest(t, mux, "GET", "/v1/community/skins?scope=mine", "", reader.AccessToken, 200).Body.Bytes(), "skins"); !slices.Equal(got, []string{"skin-r"}) {
		t.Fatal("reader skins", got)
	}
	if got := ids(apiRequest(t, mux, "GET", "/v1/community/plugins?scope=mine", "", owner.AccessToken, 200).Body.Bytes(), "plugins"); !slices.Equal(got, []string{"d1334455-1234-4234-8234-123456789abc"}) {
		t.Fatal("owner plugins", got)
	}
	if got := ids(apiRequest(t, mux, "GET", "/v1/community/plugins?scope=mine&kind=sound", "", owner.AccessToken, 200).Body.Bytes(), "plugins"); len(got) != 0 {
		t.Fatal("kind filter ignored with scope=mine", got)
	}
	for _, path := range []string{"/v1/community/skins?scope=mine", "/v1/community/plugins?scope=mine"} {
		if w := apiRequest(t, mux, "GET", path, "", "", 401); !strings.Contains(w.Body.String(), "user_session_required") {
			t.Fatal(path, w.Body.String())
		}
	}
	for _, path := range []string{"/v1/community/skins?scope=all", "/v1/community/plugins?scope=all"} {
		if w := apiRequest(t, mux, "GET", path, "", owner.AccessToken, 400); !strings.Contains(w.Body.String(), "invalid_scope") {
			t.Fatal(path, w.Body.String())
		}
	}
}

// 敏感词库无法加载时，上传以 503 `screening_unavailable` 和 `Retry-After` 拒绝，而不是笼统的 `auth_unavailable`。
func TestScreenUploadReportsScreeningOutage(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	if _, ok := a.screenUpload(w, httptest.NewRequest("POST", "/v1/community/skins", nil).WithContext(ctx), "名字"); ok {
		t.Fatal("upload screened without a word list")
	}
	if w.Code != 503 || w.Header().Get("Retry-After") != "30" || !strings.Contains(w.Body.String(), `"screening_unavailable"`) {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
}
