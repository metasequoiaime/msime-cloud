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
	"github.com/metasequoiaime/MSIME-Backend/internal/httpquery"
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

// normalizeNotification 校验通知并补齐默认标题；来自 GitHub 或用户的标题会合并为单行并截到 300 字。
func normalizeNotification(n Notification) (Notification, error) {
	if _, ok := notificationDefaultTitles[n.Kind]; !ok {
		return n, errInvalidNotification
	}
	if !resourceText(n.TargetPage, 0, 32, false) || !resourceText(n.TargetID, 0, 200, false) {
		return n, errInvalidNotification
	}
	title := normalizeNotificationTitle(n.Title)
	if title == "" {
		title = notificationDefaultTitles[n.Kind]
	}
	n.Title = title
	return n, nil
}

// normalizeNotificationTitle 删除非法编码，合并空白和控制字符，只构造长度上限内的标题。
func normalizeNotificationTitle(text string) string {
	var buffer [utf8.UTFMax * maxNotificationTitle]byte
	length, count, cut := 0, 0, 0
	pendingSpace := false
	for offset := 0; offset < len(text); {
		c, size := utf8.DecodeRuneInString(text[offset:])
		start := offset
		offset += size
		if c == utf8.RuneError && size == 1 {
			continue
		}
		if c < utf8.RuneSelf {
			if c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r' || c < 0x20 || c == 0x7f {
				pendingSpace = count > 0
				continue
			}
		} else if unicode.IsSpace(c) || unicode.IsControl(c) {
			pendingSpace = count > 0
			continue
		}
		// 只有后面还有正文才输出空格或省略号，避免尾部空白导致误截断。
		if count == maxNotificationTitle || pendingSpace && count == maxNotificationTitle-1 {
			length = cut + copy(buffer[cut:], "…")
			return string(buffer[:length])
		}
		if pendingSpace {
			buffer[length] = ' '
			length++
			count++
			if count == maxNotificationTitle-1 {
				cut = length
			}
			pendingSpace = false
		}
		length += copy(buffer[length:], text[start:offset])
		count++
		if count == maxNotificationTitle-1 {
			cut = length
		}
	}
	if length == len(text) && string(buffer[:length]) == text {
		return text
	}
	return string(buffer[:length])
}

// insertNotification 使用实际写入时间，避免长事务中的通知按事务开始时间排序。
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
	// 批次的隐式事务先锁定通知目标，再用独立语句检查，读取等待期间其他副本提交的通知。
	batch := &pgx.Batch{}
	batch.Queue(`SELECT pg_advisory_xact_lock(hashtextextended('admin_notifications:'||$1||':'||$2,0))`, n.Kind, n.TargetID)
	batch.Queue(`INSERT INTO admin_notifications(kind,title,target_page,target_id,created_at) SELECT $1,$2,$3,$4,clock_timestamp() WHERE NOT EXISTS(SELECT 1 FROM admin_notifications WHERE kind=$1 AND target_id=$4)`, n.Kind, n.Title, n.TargetPage, n.TargetID)
	return a.store.pool.SendBatch(ctx, batch).Close()
}

// notificationVisible is the condition shared by the list and the unread count, with $1 the admin's email: a kind is hidden when the admin switched off its personal preference (the admin_preferences.prefs key "notify_"+kind that POST /api/me set_pref writes, see adminPrefDefaults; a missing key means on). Kinds without a preference are always shown.
const notificationVisible = `COALESCE((SELECT prefs FROM admin_preferences WHERE email=$1)->>(CASE WHEN n.kind IN ('` + NotifyDictPR + `','` + NotifyReport + `','` + NotifyCrashSpike + `') THEN 'notify_'||n.kind END),'true')<>'false'`

// notificationRead 同时识别逐条已读记录和旧版本留下的全部已读水位。
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
	limit, validLimit := httpquery.Int(r.URL.Query(), "limit", 20, 1, maxNotificationLimit)
	if !validLimit {
		writeError(w, 400, "invalid_limit")
		return
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
		var item notificationItem
		_, err := pgx.ForEachRow(rows, []any{&item.ID, &item.Kind, &item.Title, &item.TargetPage, &item.TargetID, &item.CreatedAt, &item.Read}, func() error {
			items = append(items, item)
			return nil
		})
		return err
	})
	batch.Queue(`SELECT count(*) FROM admin_notifications n WHERE `+notificationVisible+` AND NOT `+notificationRead, email).QueryRow(func(row pgx.Row) error { return row.Scan(&unread) })
	if err := a.store.pool.SendBatch(r.Context(), batch).Close(); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "unread": unread})
}

// adminNotificationsRead 处理 POST /api/notifications/read {ids}|{all:true,up_to_id?}。
// 全部已读只记录本次语句可见、创建时间不晚于 up_to_id 的通知；省略 up_to_id 时使用当前时间。
// 未提交的通知不能提前标为已读。操作只修改本人标记，不需要额外权限，也不写审计；旧令牌没有邮箱时直接返回成功。
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
		// 时间水位会把稍后提交的旧通知一起吞掉，改为只给当前快照内尚未读过的通知写标记。
		_, err = a.store.pool.Exec(ctx, `INSERT INTO admin_notification_reads(email,notification_id)
SELECT $1,n.id FROM admin_notifications n
WHERE n.created_at<=CASE WHEN $2::bigint IS NULL THEN now() ELSE (SELECT created_at FROM admin_notifications WHERE id=$2) END
 AND NOT `+notificationRead+` ON CONFLICT DO NOTHING`, email, upTo)
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
