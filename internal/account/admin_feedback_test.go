package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 管理后台的反馈列表：按类型、平台、状态筛选，带作者和截图张数，截图经单独的路由读取，标记处理与重新打开写审计。
func TestAdminFeedbackListScreenshotsAndStatus(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE feedback,admin_audit CASCADE`); err != nil {
		t.Fatal(err)
	}
	anonymous := complete(t, db, Identity{"anonymous", "msime-device-0002:hmac"})
	member := complete(t, db, Identity{"email", "feedback-admin@example.test"})
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	shot := feedbackPNG(t, 12, 24)
	var bug, suggestion struct{ ID string }
	if err := json.Unmarshal(postFeedback(t, mux, anonymous.AccessToken, 201, payloadPart(validFeedbackPayload), screenshotPart(shot, "image/png"), screenshotPart(feedbackJPEG(t), "image/jpeg")).Body.Bytes(), &bug); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(postFeedback(t, mux, member.AccessToken, 201, payloadPart(`{"type":"suggestion","text":"希望支持双拼","platform":"ios","app_version":"2.0"}`)).Body.Bytes(), &suggestion); err != nil {
		t.Fatal(err)
	}
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.AdminHTTP(w, r.WithContext(adminTestContext(r.Context(), "test-admin")))
	})
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	list := func(query string) {
		t.Helper()
		if err := json.Unmarshal(apiRequest(t, admin, "GET", "/api/feedback"+query, "", "", 200).Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
	}
	list("")
	if page.Total != 2 {
		t.Fatal("feedback list", page)
	}
	list("?type=bug")
	if page.Total != 1 || page.Items[0]["id"] != bug.ID || page.Items[0]["screenshots"] != float64(2) || page.Items[0]["anonymous"] != true || page.Items[0]["status"] != "new" || page.Items[0]["text"] != "候选栏偶尔不显示\n重启后恢复" {
		t.Fatal("bug row", page.Items)
	}
	if diagnostics, _ := page.Items[0]["diagnostics"].(map[string]any); diagnostics["device"] != "Pixel 8" {
		t.Fatal("diagnostics", page.Items[0])
	}
	list("?platform=ios")
	if page.Total != 1 || page.Items[0]["id"] != suggestion.ID || page.Items[0]["anonymous"] != false || page.Items[0]["screenshots"] != float64(0) {
		t.Fatal("platform filter", page.Items)
	}
	list("?q=双拼")
	if page.Total != 1 || page.Items[0]["id"] != suggestion.ID {
		t.Fatal("search", page.Items)
	}
	for _, query := range []string{"?type=praise", "?platform=web", "?status=pending", "?visibility=public"} {
		apiRequest(t, admin, "GET", "/api/feedback"+query, "", "", 400)
	}
	w := apiRequest(t, admin, "GET", "/api/feedback/"+bug.ID+"/screenshots/0", "", "", 200)
	if w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG")) || bytes.Contains(w.Body.Bytes(), []byte("secret-location")) {
		t.Fatal("screenshot response", w.Header())
	}
	if w = apiRequest(t, admin, "GET", "/api/feedback/"+bug.ID+"/screenshots/1", "", "", 200); w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal(w.Header())
	}
	for _, path := range []string{bug.ID + "/screenshots/2", bug.ID + "/screenshots/3", bug.ID + "/screenshots/01", bug.ID + "/screenshots/-1", bug.ID, "not-an-id/screenshots/0", suggestion.ID + "/screenshots/0", strings.ToUpper(bug.ID[:35]) + "/screenshots/0"} {
		apiRequest(t, admin, "GET", "/api/feedback/"+path, "", "", 404)
	}
	apiRequest(t, admin, "POST", "/api/actions", `{"action":"resolve_feedback","id":"`+bug.ID+`"}`, "", 200)
	list("?status=resolved")
	if page.Total != 1 || page.Items[0]["id"] != bug.ID {
		t.Fatal("resolved filter", page.Items)
	}
	apiRequest(t, admin, "POST", "/api/actions", `{"action":"reopen_feedback","id":"`+bug.ID+`"}`, "", 200)
	list("?status=new")
	if page.Total != 2 {
		t.Fatal("reopened", page.Items)
	}
	apiRequest(t, admin, "POST", "/api/actions", `{"action":"resolve_feedback","id":"missing"}`, "", 404)
	apiRequest(t, admin, "POST", "/api/actions", `{"action":"resolve_feedback","id":""}`, "", 400)
	var audits int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE target=$1 AND action IN ('resolve_feedback','reopen_feedback')`, bug.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatal("audit", audits, err)
	}
	// 截图读取失败时返回 503。
	db.Close()
	apiRequest(t, admin, "GET", "/api/feedback/"+bug.ID+"/screenshots/0", "", "", 503)
}
