package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// fakeBroadcaster records what publish_notice sent and can be told to fail.
type fakeBroadcaster struct {
	sent []Notice
	err  error
}

func (f *fakeBroadcaster) BroadcastNotice(_ context.Context, n Notice) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, n)
	return nil
}

func noticeTestService(t *testing.T) (*Service, *Store) {
	t.Helper()
	db := testStore(t)
	if _, err := db.pool.Exec(context.Background(), `TRUNCATE admin_notices,admin_audit`); err != nil {
		t.Fatal(err)
	}
	return &Service{store: db}, db
}

// noticeAction posts one action with ctx's access and returns the status and decoded body.
func noticeAction(t *testing.T, a *Service, ctx context.Context, body string) (int, map[string]any) {
	t.Helper()
	r := jsonRequest("POST", "/api/actions", body, "")
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r.WithContext(ctx))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return w.Code, out
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func auditCount(t *testing.T, db *Store, action string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_audit WHERE action=$1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNoticeDraftPublishArchiveLifecycle(t *testing.T) {
	a, db := noticeTestService(t)
	broadcaster := &fakeBroadcaster{}
	a.ConfigureNoticeBroadcaster(broadcaster)
	ctx := adminTestContext(context.Background(), "google:sub-1:owner@example.test")

	status, out := noticeAction(t, a, ctx, `{"action":"save_notice_draft","value":{"title":"  Windows 10 图标方框  ","body":"临时处理办法","targets":["windows"],"channels":[]}}`)
	if status != 200 || out["id"] == "" {
		t.Fatal(status, out)
	}
	id := out["id"].(string)
	status, out = noticeAction(t, a, ctx, `{"action":"save_notice_draft","id":"`+id+`","value":{"title":"Windows 10 工具栏图标方框的临时处理办法","body":"正文","targets":["windows"],"channels":["app","telegram"]}}`)
	if status != 200 || out["id"] != id {
		t.Fatal(status, out)
	}

	// The draft is not public yet.
	if items := publicNotices(t, a, "/v1/notices"); len(items) != 0 {
		t.Fatal("draft leaked to public feed", items)
	}

	status, out = noticeAction(t, a, ctx, `{"action":"publish_notice","id":"`+id+`"}`)
	if status != 200 || out["id"] != id {
		t.Fatal(status, out)
	}
	if len(broadcaster.sent) != 1 || broadcaster.sent[0].Title != "Windows 10 工具栏图标方框的临时处理办法" || !slices.Equal(broadcaster.sent[0].Channels, []string{"app", "telegram"}) {
		t.Fatal("telegram broadcast", broadcaster.sent)
	}
	// A published notice can no longer be edited or published again.
	if status, out = noticeAction(t, a, ctx, `{"action":"save_notice_draft","id":"`+id+`","value":{"title":"x","targets":["all"]}}`); status != 409 || errorCode(out) != "not_draft" {
		t.Fatal(status, out)
	}
	if status, out = noticeAction(t, a, ctx, `{"action":"publish_notice","id":"`+id+`"}`); status != 409 || errorCode(out) != "not_draft" {
		t.Fatal(status, out)
	}

	// A new notice published directly, for every platform on the website only.
	status, out = noticeAction(t, a, ctx, `{"action":"publish_notice","value":{"title":"词库共建上线","body":"打不出来的词直接提交","targets":["all"],"channels":["site"]}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	direct := out["id"].(string)
	if len(broadcaster.sent) != 1 {
		t.Fatal("a notice without the telegram channel was broadcast")
	}

	if items := publicNotices(t, a, "/v1/notices?platform=windows"); len(items) != 2 {
		t.Fatal("windows feed", items)
	}
	if items := publicNotices(t, a, "/v1/notices?platform=ios"); len(items) != 1 || items[0]["id"] != direct {
		t.Fatal("ios feed only sees the all-platform notice", items)
	}
	if items := publicNotices(t, a, "/v1/notices?platform=windows&channel=app"); len(items) != 1 || items[0]["id"] != id {
		t.Fatal("app channel feed", items)
	}
	// 平台别名拿到的公告与规范 ID 相同。
	for platform, want := range map[string]int{"win": 2, "Windows": 2, "darwin": 1, "mac": 1, "ipados": 1, "harmony": 1, "harmonyos": 1, "ohos": 1} {
		if items := publicNotices(t, a, "/v1/notices?platform="+platform); len(items) != want {
			t.Fatal("platform alias", platform, items)
		}
	}

	r := jsonRequest("GET", "/api/notices", "", "")
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r.WithContext(ctx))
	var list struct {
		Items []struct {
			ID          string  `json:"id"`
			Author      string  `json:"author"`
			CreatedBy   string  `json:"created_by"`
			PublishedAt *string `json:"published_at"`
		} `json:"items"`
		Telegram bool `json:"telegram"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Items) != 2 || !list.Telegram {
		t.Fatal(w.Code, w.Body.String())
	}
	if list.Items[0].ID != direct || list.Items[0].Author != "owner@example.test" || list.Items[0].CreatedBy != "google:sub-1:owner@example.test" || list.Items[0].PublishedAt == nil {
		t.Fatal(list.Items[0])
	}

	status, out = noticeAction(t, a, ctx, `{"action":"archive_notice","id":"`+direct+`","reason":"已过期"}`)
	if status != 200 || out["affected"] != float64(1) {
		t.Fatal(status, out)
	}
	if status, out = noticeAction(t, a, ctx, `{"action":"archive_notice","id":"`+direct+`"}`); status != 409 || errorCode(out) != "already_archived" {
		t.Fatal(status, out)
	}
	if items := publicNotices(t, a, "/v1/notices?platform=ios"); len(items) != 0 {
		t.Fatal("archived notice still public", items)
	}

	var detail string
	if err := db.pool.QueryRow(context.Background(), `SELECT detail::text FROM admin_audit WHERE action='archive_notice' AND target=$1 AND actor='google:sub-1:owner@example.test'`, direct).Scan(&detail); err != nil || !strings.Contains(detail, `"from": "live"`) || !strings.Contains(detail, `"reason": "已过期"`) {
		t.Fatal(detail, err)
	}
	if auditCount(t, db, "save_notice_draft") != 2 || auditCount(t, db, "publish_notice") != 2 || auditCount(t, db, "archive_notice") != 1 {
		t.Fatal("audit rows")
	}
	// The title was trimmed and the draft's update kept its id.
	var title string
	if err := db.pool.QueryRow(context.Background(), `SELECT title FROM admin_notices WHERE id=$1`, id).Scan(&title); err != nil || title != "Windows 10 工具栏图标方框的临时处理办法" {
		t.Fatal(title, err)
	}
}

func publicNotices(t *testing.T, a *Service, path string) []map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	a.PublicNotices(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=60" || w.Header().Get("Vary") != "Origin" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, item := range body.Items {
		if _, leaked := item["created_by"]; leaked {
			t.Fatal("public feed exposes the author")
		}
	}
	return body.Items
}

func TestNoticeValidationAndPermissions(t *testing.T) {
	a, db := noticeTestService(t)
	owner := adminTestContext(context.Background(), "legacy-token")
	reviewer := WithAdminAccess(context.Background(), AdminAccess{Actor: "google:r:reviewer@example.test", Email: "reviewer@example.test", Role: "reviewer", Permissions: []string{PermReviewCommunity}})

	for _, tc := range []struct{ body, code string }{
		{`{"action":"save_notice_draft"}`, "invalid_value"},
		{`{"action":"save_notice_draft","value":{"title":"t","targets":["all"],"extra":1}}`, "invalid_value"},
		{`{"action":"save_notice_draft","value":{"title":"   ","targets":["all"]}}`, "invalid_title"},
		{`{"action":"save_notice_draft","value":{"title":"a\u0001b","targets":["all"]}}`, "invalid_title"},
		{`{"action":"save_notice_draft","value":{"title":"t","targets":[]}}`, "invalid_targets"},
		{`{"action":"save_notice_draft","value":{"title":"t","targets":["all","windows"]}}`, "invalid_targets"},
		{`{"action":"save_notice_draft","value":{"title":"t","targets":["windows","windows"]}}`, "invalid_targets"},
		{`{"action":"save_notice_draft","value":{"title":"t","targets":["symbian"]}}`, "invalid_targets"},
		{`{"action":"save_notice_draft","value":{"title":"t","targets":["all"],"channels":["qq"]}}`, "invalid_channels"},
		{`{"action":"publish_notice","value":{"title":"t","targets":["all"],"channels":[]}}`, "invalid_channels"},
		{`{"action":"save_notice_draft","id":"abc","value":{"title":"t","targets":["all"]}}`, "invalid_id"},
		{`{"action":"archive_notice"}`, "invalid_id"},
	} {
		if status, out := noticeAction(t, a, owner, tc.body); status != 400 || errorCode(out) != tc.code {
			t.Errorf("%s: %d %v want %s", tc.body, status, out, tc.code)
		}
	}
	if status, out := noticeAction(t, a, owner, `{"action":"archive_notice","id":"999999"}`); status != 404 || errorCode(out) != "not_found" {
		t.Fatal(status, out)
	}
	if status, out := noticeAction(t, a, owner, `{"action":"save_notice_draft","id":"999999","value":{"title":"t","targets":["all"]}}`); status != 404 || errorCode(out) != "not_found" {
		t.Fatal(status, out)
	}

	// Any admin may keep drafts; only publish_notices may publish or archive.
	status, out := noticeAction(t, a, reviewer, `{"action":"save_notice_draft","value":{"title":"志愿者草稿","targets":["all"],"channels":["site"]}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	draft := out["id"].(string)
	for _, body := range []string{
		`{"action":"publish_notice","id":"` + draft + `"}`,
		`{"action":"publish_notice","value":{"title":"t","targets":["all"],"channels":["site"]}}`,
		`{"action":"archive_notice","id":"` + draft + `"}`,
	} {
		if status, out := noticeAction(t, a, reviewer, body); status != 403 || errorCode(out) != "permission_denied" {
			t.Fatal(body, status, out)
		}
	}
	// An operator holds publish_notices.
	operator := WithAdminAccess(context.Background(), AdminAccess{Actor: "pat:ops@example.test", Email: "ops@example.test", Role: "operator", Permissions: []string{PermPublishNotices}})
	if status, out := noticeAction(t, a, operator, `{"action":"publish_notice","id":"`+draft+`"}`); status != 200 {
		t.Fatal(status, out)
	}
	if auditCount(t, db, "publish_notice") != 1 || auditCount(t, db, "save_notice_draft") != 1 || auditCount(t, db, "archive_notice") != 0 {
		t.Fatal("failed actions left audit rows")
	}

	// A draft saved without channels cannot be published as saved.
	status, out = noticeAction(t, a, owner, `{"action":"save_notice_draft","value":{"title":"无渠道","targets":["all"]}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	if status, out := noticeAction(t, a, owner, `{"action":"publish_notice","id":"`+out["id"].(string)+`"}`); status != 400 || errorCode(out) != "invalid_channels" {
		t.Fatal(status, out)
	}

	for _, path := range []string{"/v1/notices?platform=all", "/v1/notices?platform=symbian", "/v1/notices?channel=qq"} {
		w := httptest.NewRecorder()
		a.PublicNotices(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 400 {
			t.Fatal(path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	(*Service)(nil).PublicNotices(w, httptest.NewRequest("GET", "/v1/notices", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestNoticeTelegramFailureRollsBackPublish(t *testing.T) {
	a, db := noticeTestService(t)
	owner := adminTestContext(context.Background(), "legacy-token")
	value := `{"title":"iOS 1.0.0 公开测试开始","body":"欢迎参与","targets":["ios"],"channels":["app","telegram"]}`

	// Without admin.telegram the channel cannot be requested.
	if status, out := noticeAction(t, a, owner, `{"action":"publish_notice","value":`+value+`}`); status != 409 || errorCode(out) != "telegram_disabled" {
		t.Fatal(status, out)
	}
	r := jsonRequest("GET", "/api/notices", "", "")
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r.WithContext(owner))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"telegram":false`) || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal(w.Code, w.Body.String())
	}

	a.ConfigureNoticeBroadcaster(&fakeBroadcaster{err: errors.New("bot blocked")})
	status, out := noticeAction(t, a, owner, `{"action":"save_notice_draft","value":`+value+`}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	draft := out["id"].(string)
	if status, out := noticeAction(t, a, owner, `{"action":"publish_notice","id":"`+draft+`"}`); status != 502 || errorCode(out) != "telegram_failed" {
		t.Fatal(status, out)
	}
	if status, out := noticeAction(t, a, owner, `{"action":"publish_notice","value":`+value+`}`); status != 502 || errorCode(out) != "telegram_failed" {
		t.Fatal(status, out)
	}
	var live, drafts int
	if err := db.pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE status='live'),count(*) FILTER (WHERE status='draft') FROM admin_notices`).Scan(&live, &drafts); err != nil || live != 0 || drafts != 1 {
		t.Fatal(live, drafts, err)
	}
	if auditCount(t, db, "publish_notice") != 0 {
		t.Fatal("failed publish audited")
	}
}

func TestNoticeAuthor(t *testing.T) {
	for actor, want := range map[string]string{"google:123:a@example.test": "a@example.test", "pat:b@example.test": "b@example.test", "legacy-token": "legacy-token"} {
		if got := noticeAuthor(actor); got != want {
			t.Errorf("%s: %s", actor, got)
		}
	}
}

func TestNoticePublishOverwritesDraftAndDiscard(t *testing.T) {
	a, db := noticeTestService(t)
	owner := adminTestContext(context.Background(), "legacy-token")

	status, out := noticeAction(t, a, owner, `{"action":"save_notice_draft","value":{"title":"旧标题","body":"旧正文","targets":["windows"],"channels":[]}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	draft := out["id"].(string)
	// Publishing a draft with a value publishes the console's current form, not what was saved.
	if status, out = noticeAction(t, a, owner, `{"action":"publish_notice","id":"`+draft+`","value":{"title":"新标题","body":"新正文","targets":["macos","linux"],"channels":["app"]}}`); status != 200 || out["id"] != draft {
		t.Fatal(status, out)
	}
	items := publicNotices(t, a, "/v1/notices?platform=linux&channel=app")
	if len(items) != 1 || items[0]["title"] != "新标题" || items[0]["body"] != "新正文" {
		t.Fatal(items)
	}
	if items := publicNotices(t, a, "/v1/notices?platform=windows"); len(items) != 0 {
		t.Fatal("the saved targets were published", items)
	}

	// Archiving a draft discards it and records where it came from.
	status, out = noticeAction(t, a, owner, `{"action":"save_notice_draft","value":{"title":"不要了","targets":["all"]}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	discarded := out["id"].(string)
	if status, out = noticeAction(t, a, owner, `{"action":"archive_notice","id":"`+discarded+`"}`); status != 200 {
		t.Fatal(status, out)
	}
	if status, out = noticeAction(t, a, owner, `{"action":"publish_notice","id":"`+discarded+`"}`); status != 409 || errorCode(out) != "not_draft" {
		t.Fatal(status, out)
	}
	var detail string
	if err := db.pool.QueryRow(context.Background(), `SELECT detail::text FROM admin_audit WHERE action='archive_notice' AND target=$1`, discarded).Scan(&detail); err != nil || !strings.Contains(detail, `"from": "draft"`) {
		t.Fatal(detail, err)
	}

	// A body over the column limit is rejected at the boundary.
	value, _ := json.Marshal(map[string]any{"title": "t", "body": strings.Repeat("a", noticeBodyMax+1), "targets": []string{"all"}})
	if _, err := parseNoticeInput(value, false); err == nil || err.Error() != "invalid_body" {
		t.Fatal(err)
	}
}

func TestAdminNoticesBoundsEachStatusSeparately(t *testing.T) {
	a, db := noticeTestService(t)
	owner := adminTestContext(context.Background(), "legacy-token")
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_notices(title,targets,channels,status,created_by,published_at,updated_at) VALUES('仍在展示','{all}','{site}','live','legacy-token',now()-interval '30 days',now()-interval '30 days')`); err != nil {
		t.Fatal(err)
	}
	// 新草稿和归档各自超过一页时，都不能把仍公开展示的公告挤出后台。
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_notices(title,targets,channels,status,created_by,published_at) SELECT 'archived '||g,'{all}','{site}','archived','legacy-token',now() FROM generate_series(1,$1) g`, noticeAdminLimit+5); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_notices(title,targets,channels,status,created_by) SELECT 'draft '||g,'{all}','{site}','draft','legacy-token' FROM generate_series(1,$1) g`, noticeAdminLimit+5); err != nil {
		t.Fatal(err)
	}
	public := httptest.NewRecorder()
	a.PublicNotices(public, jsonRequest("GET", NoticesPath, "", ""))
	var feed struct {
		Items []publicNotice `json:"items"`
	}
	if public.Code != 200 || json.Unmarshal(public.Body.Bytes(), &feed) != nil || len(feed.Items) != 1 || feed.Items[0].Title != "仍在展示" {
		t.Fatal("公开公告列表", public.Code, public.Body.String())
	}
	r := jsonRequest("GET", "/api/notices", "", "")
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r.WithContext(owner))
	var list struct {
		Items []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	archived, drafts, live := 0, 0, 0
	liveID := ""
	for _, n := range list.Items {
		if n.Status == "archived" {
			archived++
		} else if n.Status == "draft" {
			drafts++
		} else if n.Title == "仍在展示" {
			live++
			liveID = n.ID
		}
	}
	if archived != noticeAdminLimit || drafts != noticeAdminLimit || live != 1 || list.Items[len(list.Items)-1].Title != "仍在展示" {
		t.Fatal("归档、草稿、已发布和总数", archived, drafts, live, len(list.Items))
	}
	if status, out := noticeAction(t, a, owner, `{"action":"archive_notice","id":"`+liveID+`"}`); status != 200 {
		t.Fatal("无法用列表中的编号归档仍展示的公告", status, out)
	}
}

// A notice body may use the whole 20000-character column, sent as plain UTF-8 or as \u escapes; other actions keep the 8 KiB value limit.
func TestNoticeBodyUsesTheFullColumn(t *testing.T) {
	a, db := noticeTestService(t)
	owner := adminTestContext(context.Background(), "legacy-token")
	for name, body := range map[string]string{
		"utf-8":   strings.Repeat("水", noticeBodyMax),
		"escaped": strings.Repeat(`水`, noticeBodyMax),
	} {
		status, out := noticeAction(t, a, owner, `{"action":"save_notice_draft","value":{"title":"长公告","targets":["all"],"channels":["site"],"body":"`+body+`"}}`)
		if status != 200 {
			t.Fatal(name, status, out)
		}
		var stored int
		if err := db.pool.QueryRow(context.Background(), `SELECT length(body) FROM admin_notices WHERE id=$1`, out["id"]).Scan(&stored); err != nil || stored != noticeBodyMax {
			t.Fatal(name, stored, err)
		}
		if status, out := noticeAction(t, a, owner, `{"action":"publish_notice","id":"`+out["id"].(string)+`"}`); status != 200 {
			t.Fatal(name, status, out)
		}
	}
	if status, out := noticeAction(t, a, owner, `{"action":"save_notice_draft","value":{"title":"超长","targets":["all"],"body":"`+strings.Repeat("水", noticeBodyMax+1)+`"}}`); status != 400 || errorCode(out) != "invalid_body" {
		t.Fatal(status, out)
	}
	if status, out := noticeAction(t, a, owner, `{"action":"add_sensitive_word","value":{"pattern":"`+strings.Repeat("a", actionValueMax)+`"}}`); status != 400 || errorCode(out) != "invalid_value" {
		t.Fatal("generic value limit", status, out)
	}
}

// Polling the public feed from behind one proxy must not use up the per-address bucket that login, refresh and the community routes share.
func TestPublicNoticesPollingDoesNotStarveAccountRoutes(t *testing.T) {
	a, db := noticeTestService(t)
	if _, err := db.pool.Exec(context.Background(), `TRUNCATE auth_rates`); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, a)
	mux.HandleFunc("GET "+NoticesPath, Route(a, "GET "+NoticesPath, (*Service).PublicNotices))
	call := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "198.51.100.7:443"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for i := range 130 {
		if w := call("/v1/notices?channel=app"); w.Code != 200 {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	if w := call("/v1/site/download-mirrors"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("/v1/community/resources"); w.Code == 429 {
		t.Fatal("account routes share the feed's bucket", w.Body.String())
	}
	var shared int
	if err := db.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(count),0) FROM auth_rates WHERE key=$1`, "ip:"+hash("198.51.100.7")).Scan(&shared); err != nil || shared != 1 {
		t.Fatal(shared, err)
	}
}
