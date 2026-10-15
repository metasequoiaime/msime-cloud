package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Console notifications (unit U11). Other units only call Notify or NotifyNow.

// Notification kinds.
const (
	NotifyReport     = "report"      // a community report was filed
	NotifyDictPR     = "dict_pr"     // a new dictionary pull request was found
	NotifyCrashSpike = "crash_spike" // a crash group rose more than 20% week over week
	NotifyIncident   = "incident"    // an incident opened
	NotifyRelease    = "release"     // a release changed state
	NotifyIssue      = "issue"       // a new GitHub issue
)

// Notification is one console notification. TargetPage is the console page key (for example "community") and TargetID the item to open there.
type Notification struct {
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	TargetPage string `json:"target_page"`
	TargetID   string `json:"target_id"`
}

// notificationDefaultTitles is the title used when a caller leaves Title empty, so every notification has readable text in the bell. Its keys are the valid kinds.
var notificationDefaultTitles = map[string]string{
	NotifyReport:     "社区内容被举报",
	NotifyDictPR:     "新的词库 PR 等待审核",
	NotifyCrashSpike: "崩溃分组环比上升超过 20%",
	NotifyIncident:   "服务出现异常事件",
	NotifyRelease:    "发布状态有变化",
	NotifyIssue:      "新的 Issue 等待分诊",
}

// errInvalidNotification is returned for a notification with an unknown kind or an oversized target, which is a programming error in the caller.
var errInvalidNotification = errors.New("invalid notification")

const (
	maxNotificationTitle = 300
	maxNotificationLimit = 50
	maxNotificationReads = 100
)

// normalizeNotification validates n and fills in the default title. A title is flattened to one line and cut to 300 characters rather than rejected, because callers pass text that comes from GitHub or from users.
func normalizeNotification(n Notification) (Notification, error) {
	if _, ok := notificationDefaultTitles[n.Kind]; !ok {
		return n, errInvalidNotification
	}
	if !resourceText(n.TargetPage, 0, 32, false) || !resourceText(n.TargetID, 0, 200, false) {
		return n, errInvalidNotification
	}
	title := strings.Join(strings.FieldsFunc(strings.ToValidUTF8(n.Title, ""), func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }), " ")
	if title == "" {
		title = notificationDefaultTitles[n.Kind]
	}
	if utf8.RuneCountInString(title) > maxNotificationTitle {
		title = string([]rune(title)[:maxNotificationTitle-1]) + "…"
	}
	n.Title = title
	return n, nil
}

// insertNotification stamps created_at with clock_timestamp() rather than the transaction start, so a notification written late in a long transaction is not already covered by a 全部已读 watermark set while that transaction was running.
const insertNotification = `INSERT INTO admin_notifications(kind,title,target_page,target_id,created_at) VALUES($1,$2,$3,$4,clock_timestamp())`

// Notify records n inside tx, so it exists exactly when the write that caused it commits.
func (a *Service) Notify(ctx context.Context, tx pgx.Tx, n Notification) error {
	n, err := normalizeNotification(n)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, insertNotification, n.Kind, n.Title, n.TargetPage, n.TargetID)
	return err
}

// NotifyNow records n on its own, for causes outside the database such as a GitHub poll.
func (a *Service) NotifyNow(ctx context.Context, n Notification) error {
	n, err := normalizeNotification(n)
	if err != nil {
		return err
	}
	_, err = a.store.pool.Exec(ctx, insertNotification, n.Kind, n.Title, n.TargetPage, n.TargetID)
	return err
}

// NotifyOnce records n unless a notification of the same kind and target already exists, for causes found by polling an outside source (an open dictionary pull request) that every replica and every restart sees again. Concurrent callers for the same kind and target are serialised by an advisory lock, so exactly one row is written.
func (a *Service) NotifyOnce(ctx context.Context, n Notification) error {
	n, err := normalizeNotification(n)
	if err != nil {
		return err
	}
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// The check runs as its own statement after the lock, so its snapshot sees a row another caller committed while this one waited.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin_notifications:'||$1||':'||$2,0))`, n.Kind, n.TargetID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_notifications(kind,title,target_page,target_id,created_at) SELECT $1,$2,$3,$4,clock_timestamp() WHERE NOT EXISTS(SELECT 1 FROM admin_notifications WHERE kind=$1 AND target_id=$4)`, n.Kind, n.Title, n.TargetPage, n.TargetID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// notificationVisible is the condition shared by the list and the unread count, with $1 the admin's email: a kind is hidden when the admin switched off its personal preference (the admin_preferences.prefs key "notify_"+kind that POST /api/me set_pref writes, see adminPrefDefaults; a missing key means on). Kinds without a preference are always shown.
const notificationVisible = `COALESCE((SELECT prefs FROM admin_preferences WHERE email=$1)->>(CASE WHEN n.kind IN ('` + NotifyDictPR + `','` + NotifyReport + `','` + NotifyCrashSpike + `') THEN 'notify_'||n.kind END),'true')<>'false'`

// notificationRead is true when $1 has read n, one by one or through 全部已读 (admin_preferences.read_all_before).
const notificationRead = `(n.created_at<=COALESCE((SELECT read_all_before FROM admin_preferences WHERE email=$1),'-infinity') OR EXISTS(SELECT 1 FROM admin_notification_reads r WHERE r.email=$1 AND r.notification_id=n.id))`

// UnreadNotifications counts the notifications email has not read, for the console shell. The legacy token has no email and so no notifications.
func (a *Service) UnreadNotifications(ctx context.Context, email string) (int, error) {
	email = strings.ToLower(email)
	if email == "" {
		return 0, nil
	}
	var count int
	err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM admin_notifications n WHERE `+notificationVisible+` AND NOT `+notificationRead, email).Scan(&count)
	return count, err
}

type notificationItem struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	TargetPage string `json:"target_page"`
	TargetID   string `json:"target_id"`
	CreatedAt  string `json:"created_at"`
	Read       bool   `json:"read"`
}

// adminNotifications serves GET /api/notifications?limit=20: the caller's newest notifications, read or not, and the unread count.
func (a *Service) adminNotifications(w http.ResponseWriter, r *http.Request, _ string) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxNotificationLimit {
			writeError(w, 400, "invalid_limit")
			return
		}
		limit = parsed
	}
	access, _ := AdminAccessFrom(r.Context())
	items := []notificationItem{}
	if access.Email == "" {
		write(w, 200, map[string]any{"items": items, "unread": 0})
		return
	}
	email := strings.ToLower(access.Email)
	var unread int
	batch := &pgx.Batch{}
	batch.Queue(`SELECT n.id,n.kind,n.title,n.target_page,n.target_id,to_char(n.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),`+notificationRead+` FROM admin_notifications n WHERE `+notificationVisible+` ORDER BY n.created_at DESC,n.id DESC LIMIT $2`, email, limit).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var item notificationItem
			if err := rows.Scan(&item.ID, &item.Kind, &item.Title, &item.TargetPage, &item.TargetID, &item.CreatedAt, &item.Read); err != nil {
				return err
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	batch.Queue(`SELECT count(*) FROM admin_notifications n WHERE `+notificationVisible+` AND NOT `+notificationRead, email).QueryRow(func(row pgx.Row) error { return row.Scan(&unread) })
	if err := a.store.pool.SendBatch(r.Context(), batch).Close(); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "unread": unread})
}

// adminNotificationsRead serves POST /api/notifications/read {ids}|{all:true,up_to_id?}. 全部已读 marks read every notification up to and including up_to_id (by created_at), the newest one the console has on screen, so one that arrived after the list was fetched stays unread; without up_to_id it covers everything up to now(). It only changes the caller's own read markers, so it needs no permission and is not an audited admin action; the legacy token has no email and gets a no-op.
func (a *Service) adminNotificationsRead(w http.ResponseWriter, r *http.Request, _ string) {
	var v struct {
		IDs    []json.RawMessage `json:"ids"`
		All    bool              `json:"all"`
		UpToID json.RawMessage   `json:"up_to_id"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.All == (len(v.IDs) > 0) || len(v.IDs) > maxNotificationReads {
		writeError(w, 400, "invalid_ids")
		return
	}
	var upTo *int64
	if v.UpToID != nil {
		id, ok := notificationID(v.UpToID)
		if !v.All || !ok {
			writeError(w, 400, "invalid_ids")
			return
		}
		upTo = &id
	}
	ids := make([]int64, 0, len(v.IDs))
	for _, raw := range v.IDs {
		id, ok := notificationID(raw)
		if !ok {
			writeError(w, 400, "invalid_ids")
			return
		}
		ids = append(ids, id)
	}
	access, _ := AdminAccessFrom(r.Context())
	if access.Email == "" {
		write(w, 200, map[string]bool{"ok": true})
		return
	}
	email := strings.ToLower(access.Email)
	ctx := r.Context()
	var err error
	if v.All {
		// 用写入返回的水位清理冗余标记，避免读取同语句快照中的旧值；缺失目标产生的 NULL 不回退既有水位。
		_, err = a.store.pool.Exec(ctx, `WITH advanced AS (
 INSERT INTO admin_preferences(email,read_all_before)
 VALUES($1,CASE WHEN $2::bigint IS NULL THEN now() ELSE (SELECT created_at FROM admin_notifications WHERE id=$2) END)
 ON CONFLICT(email) DO UPDATE SET read_all_before=GREATEST(admin_preferences.read_all_before,EXCLUDED.read_all_before)
 RETURNING email,read_all_before
)
DELETE FROM admin_notification_reads r USING admin_notifications n,advanced p
WHERE r.email=p.email AND n.id=r.notification_id AND n.created_at<=p.read_all_before`, email, upTo)
	} else {
		_, err = a.store.pool.Exec(ctx, `INSERT INTO admin_notification_reads(email,notification_id) SELECT $1,n.id FROM admin_notifications n WHERE n.id=ANY($2) ON CONFLICT DO NOTHING`, email, ids)
	}
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]bool{"ok": true})
}

// notificationID accepts a notification id as a JSON number or a decimal string, since the console sends ids as strings.
func notificationID(raw json.RawMessage) (int64, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		text = string(raw)
	}
	if text == "" || len(text) > 19 || strings.TrimLeft(text, "0123456789") != "" {
		return 0, false
	}
	id, err := strconv.ParseInt(text, 10, 64)
	return id, err == nil && id > 0
}

// AdminDisplayName is the name the console shell shows for email: the Google profile name stored on the admin's newest named session, or "" when none was stored (the legacy token, personal access tokens, or sessions created before names were recorded).
func (a *Service) AdminDisplayName(ctx context.Context, email string) (string, error) {
	if email == "" {
		return "", nil
	}
	var name string
	err := a.store.pool.QueryRow(ctx, `SELECT name FROM admin_sessions WHERE email=$1 AND name<>'' AND expires_at>now() ORDER BY created_at DESC LIMIT 1`, strings.ToLower(email)).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, err
}
