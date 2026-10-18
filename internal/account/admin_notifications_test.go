package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func notificationTestService(t *testing.T) *Service {
	t.Helper()
	db := testStore(t)
	if _, err := db.pool.Exec(context.Background(), `TRUNCATE admin_notifications,admin_notification_reads,admin_preferences,admin_sessions,admin_audit CASCADE`); err != nil {
		t.Fatal(err)
	}
	return &Service{store: db}
}

// notificationRequest is a console request from a member with the given email and no permissions at all, since notifications need none.
func notificationRequest(method, path, body, email string) *http.Request {
	r := jsonRequest(method, path, body, "")
	return r.WithContext(WithAdminAccess(r.Context(), AdminAccess{Actor: "google:x:" + email, Email: email, Role: "readonly"}))
}

type notificationsBody struct {
	Items []struct {
		ID         int64  `json:"id"`
		Kind       string `json:"kind"`
		Title      string `json:"title"`
		TargetPage string `json:"target_page"`
		TargetID   string `json:"target_id"`
		CreatedAt  string `json:"created_at"`
		Read       bool   `json:"read"`
	} `json:"items"`
	Unread int `json:"unread"`
}

func listNotifications(t *testing.T, a *Service, email, query string) notificationsBody {
	t.Helper()
	w := httptest.NewRecorder()
	a.AdminHTTP(w, notificationRequest("GET", "/api/notifications"+query, "", email))
	var body notificationsBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	return body
}

func TestNotifyValidatesAndDefaultsTitles(t *testing.T) {
	a := notificationTestService(t)
	ctx := context.Background()
	if err := a.NotifyNow(ctx, Notification{Kind: "bogus", Title: "x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if err := a.NotifyNow(ctx, Notification{Kind: NotifyIssue, TargetID: strings.Repeat("x", 201)}); err == nil {
		t.Fatal("oversized target accepted")
	}
	if err := a.NotifyNow(ctx, Notification{Kind: NotifyCrashSpike, TargetPage: "crash", TargetID: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("长", 400)
	if err := pgx.BeginFunc(ctx, a.store.pool, func(tx pgx.Tx) error {
		return a.Notify(ctx, tx, Notification{Kind: NotifyDictPR, Title: "line one\nline\ttwo " + long, TargetPage: "dictpr", TargetID: "214"})
	}); err != nil {
		t.Fatal(err)
	}
	// A notification inside a rolled-back transaction must not exist.
	_ = pgx.BeginFunc(ctx, a.store.pool, func(tx pgx.Tx) error {
		if err := a.Notify(ctx, tx, Notification{Kind: NotifyReport, TargetPage: "community", TargetID: "skins/x"}); err != nil {
			t.Fatal(err)
		}
		return context.Canceled
	})
	body := listNotifications(t, a, "member@example.test", "")
	if len(body.Items) != 2 || body.Unread != 2 {
		t.Fatalf("got %+v", body)
	}
	pr, spike := body.Items[0], body.Items[1]
	if pr.Kind != NotifyDictPR || !strings.HasPrefix(pr.Title, "line one line two 长") || len([]rune(pr.Title)) != 300 || strings.ContainsAny(pr.Title, "\n\t") || pr.TargetID != "214" || pr.Read {
		t.Fatalf("dict pr notification %+v", pr)
	}
	if spike.Title != notificationDefaultTitles[NotifyCrashSpike] || spike.TargetPage != "crash" || !strings.HasSuffix(spike.CreatedAt, "Z") {
		t.Fatalf("crash spike notification %+v", spike)
	}
}

func TestNotificationsReadStateIsPerAdmin(t *testing.T) {
	a := notificationTestService(t)
	ctx := context.Background()
	for _, kind := range []string{NotifyIssue, NotifyRelease, NotifyIncident} {
		if err := a.NotifyNow(ctx, Notification{Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	first := listNotifications(t, a, "a@example.test", "?limit=2")
	if len(first.Items) != 2 || first.Unread != 3 {
		t.Fatalf("got %+v", first)
	}
	newest := first.Items[0].ID
	read := func(email, body string, status int) {
		t.Helper()
		w := httptest.NewRecorder()
		a.AdminHTTP(w, notificationRequest("POST", "/api/notifications/read", body, email))
		if w.Code != status {
			t.Fatalf("read %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	// Ids arrive as strings from the console; numbers work too, and unknown ids are ignored.
	read("a@example.test", `{"ids":["`+jsonInt(newest)+`",999999999]}`, 200)
	if got := listNotifications(t, a, "a@example.test", ""); got.Unread != 2 || !got.Items[0].Read || got.Items[1].Read {
		t.Fatalf("after one read: %+v", got)
	}
	if n, err := a.UnreadNotifications(ctx, "B@example.test"); err != nil || n != 3 {
		t.Fatalf("other admin unread %d %v", n, err)
	}
	read("a@example.test", `{"all":true}`, 200)
	if n, err := a.UnreadNotifications(ctx, "a@example.test"); err != nil || n != 0 {
		t.Fatalf("after all: %d %v", n, err)
	}
	var markers int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM admin_notification_reads`).Scan(&markers); err != nil || markers != 3 {
		t.Fatalf("全部已读标记数 %d %v", markers, err)
	}
	if err := a.NotifyNow(ctx, Notification{Kind: NotifyIssue}); err != nil {
		t.Fatal(err)
	}
	if got := listNotifications(t, a, "a@example.test", ""); got.Unread != 1 || got.Items[0].Read || !got.Items[1].Read {
		t.Fatalf("new after all: %+v", got)
	}
	for _, bad := range []string{`{}`, `{"ids":[]}`, `{"ids":["1"],"all":true}`, `{"ids":["x"]}`, `{"ids":[-1]}`, `{"ids":["0"]}`, `{"all":false}`, `{"ids":[1],"extra":1}`, `{"ids":[` + strings.Repeat(`1,`, 100) + `1]}`} {
		read("a@example.test", bad, 400)
	}
	for _, bad := range []string{"?limit=0", "?limit=51", "?limit=x"} {
		w := httptest.NewRecorder()
		a.AdminHTTP(w, notificationRequest("GET", "/api/notifications"+bad, "", "a@example.test"))
		if w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	// Reading notifications is personal state, not an admin action.
	var audits int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("audit rows %d %v", audits, err)
	}
}

// 全部已读 with up_to_id covers only what the console had on screen: a notification inserted after the list was fetched stays unread.
func TestNotificationsReadAllStopsAtShownNewest(t *testing.T) {
	a := notificationTestService(t)
	ctx := context.Background()
	for _, kind := range []string{NotifyIssue, NotifyRelease} {
		if err := a.NotifyNow(ctx, Notification{Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	shown := listNotifications(t, a, "a@example.test", "")
	if len(shown.Items) != 2 || shown.Unread != 2 {
		t.Fatalf("got %+v", shown)
	}
	if err := a.NotifyNow(ctx, Notification{Kind: NotifyIncident}); err != nil {
		t.Fatal(err)
	}
	read := func(body string, status int) {
		t.Helper()
		w := httptest.NewRecorder()
		a.AdminHTTP(w, notificationRequest("POST", "/api/notifications/read", body, "a@example.test"))
		if w.Code != status {
			t.Fatalf("read %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, bad := range []string{`{"up_to_id":"1"}`, `{"ids":["1"],"up_to_id":"1"}`, `{"all":true,"up_to_id":"x"}`, `{"all":true,"up_to_id":0}`, `{"all":true,"up_to_id":null}`} {
		read(bad, 400)
	}
	read(`{"ids":["`+jsonInt(shown.Items[1].ID)+`"]}`, 200)
	read(`{"all":true,"up_to_id":"`+jsonInt(shown.Items[0].ID)+`"}`, 200)
	got := listNotifications(t, a, "a@example.test", "")
	if got.Unread != 1 || got.Items[0].Kind != NotifyIncident || got.Items[0].Read || !got.Items[1].Read || !got.Items[2].Read {
		t.Fatalf("after read all up to the shown newest: %+v", got)
	}
	var markers int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM admin_notification_reads`).Scan(&markers); err != nil || markers != 2 {
		t.Fatalf("截止到已显示通知的标记数 %d %v", markers, err)
	}
	// 更早或已不存在的 up_to_id 不能撤销既有的已读记录。
	read(`{"all":true,"up_to_id":"`+jsonInt(shown.Items[1].ID)+`"}`, 200)
	read(`{"all":true,"up_to_id":"999999999"}`, 200)
	if n, err := a.UnreadNotifications(ctx, "a@example.test"); err != nil || n != 1 {
		t.Fatalf("watermark moved: %d %v", n, err)
	}
	if n, err := a.UnreadNotifications(ctx, "b@example.test"); err != nil || n != 3 {
		t.Fatalf("other admin unread %d %v", n, err)
	}
}

func TestNotificationsLegacyTokenHasNone(t *testing.T) {
	a := notificationTestService(t)
	if err := a.NotifyNow(context.Background(), Notification{Kind: NotifyIssue}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.AdminHTTP(w, adminJSONRequest("GET", "/api/notifications", ""))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"items":[],"unread":0}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.AdminHTTP(w, adminJSONRequest("POST", "/api/notifications/read", `{"all":true}`))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if n, err := a.UnreadNotifications(context.Background(), ""); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestNotificationPreferencesMuteKinds(t *testing.T) {
	a := notificationTestService(t)
	ctx := context.Background()
	for _, kind := range []string{NotifyDictPR, NotifyReport, NotifyCrashSpike, NotifyIncident} {
		if err := a.NotifyNow(ctx, Notification{Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	// The preferences are written the way the personal page writes them, so the reader and the writer agree on the keys.
	access := AdminAccess{Actor: "google:x:a@example.test", Email: "a@example.test", Role: "readonly"}
	for _, body := range []string{`{"action":"set_pref","key":"notify_dict_pr","value":false}`, `{"action":"set_pref","key":"notify_report","value":true}`, `{"action":"set_pref","key":"notify_crash_spike","value":false}`} {
		if w := meRequest(a, access, "POST", body, ""); w.Code != 200 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	got := listNotifications(t, a, "a@example.test", "")
	kinds := []string{}
	for _, item := range got.Items {
		kinds = append(kinds, item.Kind)
	}
	if got.Unread != 2 || strings.Join(kinds, ",") != "incident,report" {
		t.Fatalf("muted list %+v", got)
	}
	if n, _ := a.UnreadNotifications(ctx, "b@example.test"); n != 4 {
		t.Fatalf("default prefs unread %d", n)
	}
}

func TestAdminDisplayName(t *testing.T) {
	a := notificationTestService(t)
	ctx := context.Background()
	if name, err := a.AdminDisplayName(ctx, "a@example.test"); err != nil || name != "" {
		t.Fatal(name, err)
	}
	if _, err := a.store.pool.Exec(ctx, `INSERT INTO admin_sessions(token_hash,subject,email,name,created_at) VALUES('h1','s','a@example.test','Old',now()-interval '1 hour'),('h2','s','a@example.test','Houko',now()),('h3','s','a@example.test','',now()+interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	if name, err := a.AdminDisplayName(ctx, "A@example.test"); err != nil || name != "Houko" {
		t.Fatal(name, err)
	}
	if name, err := a.AdminDisplayName(ctx, ""); err != nil || name != "" {
		t.Fatal(name, err)
	}
}

func jsonInt(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// notificationTargets lists the target_page/target_id of the notifications of kind recorded since the given id, oldest first, so a unit's test can check that its write reached the console bell.
func notificationTargets(t *testing.T, db *Store, kind string, since int64) []string {
	t.Helper()
	rows, err := db.pool.Query(context.Background(), `SELECT target_page||' '||target_id FROM admin_notifications WHERE kind=$1 AND id>$2 ORDER BY id`, kind, since)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// lastNotificationID is the newest notification id, the starting point for notificationTargets.
func lastNotificationID(t *testing.T, db *Store) int64 {
	t.Helper()
	var id int64
	if err := db.pool.QueryRow(context.Background(), `SELECT COALESCE(max(id),0) FROM admin_notifications`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNotifyOnceWritesOneRowPerTarget(t *testing.T) {
	a := notificationTestService(t)
	ctx := context.Background()
	errs := make(chan error, 6)
	for i := range 6 {
		go func() {
			errs <- a.NotifyOnce(ctx, Notification{Kind: NotifyDictPR, TargetPage: "dictpr", TargetID: strconv.Itoa(12 + i%2)})
		}()
	}
	for range 6 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM admin_notifications WHERE kind='dict_pr'`).Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
}
