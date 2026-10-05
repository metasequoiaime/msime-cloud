package account

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var errClipboardDisabled = errors.New("clipboard_sync_disabled")
var errDataNotFound = errors.New("user_data_not_found")

type ClipboardItem struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	UpdatedAt time.Time `json:"updated_at"`
	// Pinned 的条目排在最前，不受保留期清理，也不会被五十条上限挤掉。
	Pinned bool `json:"pinned"`
	// Device 是写入时所用会话的设备名（由登录时记录的 User-Agent 解析），认不出时为空字符串。
	Device string `json:"device"`
}

// clipboardRetentionDays 是保留时长允许的取值；0 表示一直保留。
var clipboardRetentionDays = map[int]bool{0: true, 1: true, 7: true, 30: true}

// 所有写入锁定同一用户行，保证并发去重和每用户五十条的上限。
// The ban check reads banned_at under the same row lock that ban_user takes, so a write whose session was checked before a ban either commits before the ban (which then hides what it wrote) or sees the ban and fails with ErrBanned; it can no longer publish after the ban has committed.
func (s *Store) userDataTransaction(ctx context.Context, user string) (pgx.Tx, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	var banned bool
	if e = tx.QueryRow(ctx, "SELECT banned_at IS NOT NULL FROM auth_users WHERE id=$1 FOR UPDATE", user).Scan(&banned); e != nil {
		tx.Rollback(ctx)
		return nil, e
	}
	if banned {
		tx.Rollback(ctx)
		return nil, ErrBanned
	}
	return tx, nil
}
func (s *Store) ClipboardEnabled(ctx context.Context, user string) (bool, error) {
	var enabled bool
	e := s.pool.QueryRow(ctx, "SELECT COALESCE((SELECT enabled FROM user_clipboard_settings WHERE user_id=$1),false)", user).Scan(&enabled)
	return enabled, e
}
func (s *Store) SetClipboardEnabled(ctx context.Context, user string, enabled bool) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "INSERT INTO user_clipboard_settings(user_id,enabled) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET enabled=excluded.enabled", user, enabled); e != nil {
		return e
	}
	// 撤销同步时清空云端历史；不会操作用户设备的系统剪贴板。
	if !enabled {
		if _, e = tx.Exec(ctx, "DELETE FROM user_clipboard WHERE user_id=$1", user); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) AddClipboard(ctx context.Context, user, text string) (ClipboardItem, error) {
	return s.AddClipboardFrom(ctx, user, "", text)
}

// ClipboardRetention 返回用户设置的保留天数，没有设置时为 0。
func (s *Store) ClipboardRetention(ctx context.Context, user string) (int, error) {
	var days int
	e := s.pool.QueryRow(ctx, "SELECT COALESCE((SELECT retention_days FROM user_clipboard_settings WHERE user_id=$1),0)", user).Scan(&days)
	return days, e
}

// SetClipboardRetention 写入保留天数，不改变同步开关；超过保留期的条目由 Store.Prune 删除。
func (s *Store) SetClipboardRetention(ctx context.Context, user string, days int) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "INSERT INTO user_clipboard_settings(user_id,retention_days) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET retention_days=excluded.retention_days", user, days); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// PinClipboard 置顶或取消置顶本人的一条剪贴板条目；不是本人的或不存在时返回 errDataNotFound。
func (s *Store) PinClipboard(ctx context.Context, user, id string, pinned bool) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	result, e := tx.Exec(ctx, "UPDATE user_clipboard SET pinned=$3 WHERE user_id=$1 AND id=$2", user, id, pinned)
	if e != nil {
		return e
	}
	if result.RowsAffected() == 0 {
		return errDataNotFound
	}
	return tx.Commit(ctx)
}

// sessionDevice 返回会话登录时记录的设备名，认不出时为空字符串。
func (s *Store) sessionDevice(ctx context.Context, session string) (string, error) {
	var agent string
	e := s.pool.QueryRow(ctx, "SELECT COALESCE((SELECT user_agent FROM auth_sessions WHERE id=$1),'')", session).Scan(&agent)
	_, name, _ := describeSessionAgent(agent)
	return name, e
}

// AddClipboardFrom 写入一条剪贴板文本并记下写入设备；相同文本只保留一条，移到最前并更新设备名。
func (s *Store) AddClipboardFrom(ctx context.Context, user, device, text string) (ClipboardItem, error) {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return ClipboardItem{}, e
	}
	defer tx.Rollback(ctx)
	var enabled bool
	if e = tx.QueryRow(ctx, "SELECT COALESCE((SELECT enabled FROM user_clipboard_settings WHERE user_id=$1),false)", user).Scan(&enabled); e != nil {
		return ClipboardItem{}, e
	}
	if !enabled {
		return ClipboardItem{}, errClipboardDisabled
	}
	var item ClipboardItem
	e = tx.QueryRow(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,sequence,device) VALUES($1,$2,$3,$4,(SELECT COALESCE(max(sequence),0)+1 FROM user_clipboard WHERE user_id=$2),$5)
 ON CONFLICT(user_id,text_hash) DO UPDATE SET sequence=excluded.sequence,updated_at=now(),device=excluded.device
 RETURNING id,text,updated_at,pinned,device`, randomToken(), user, text, hash(text), device).Scan(&item.ID, &item.Text, &item.UpdatedAt, &item.Pinned, &item.Device)
	if e != nil {
		return item, e
	}
	_, e = tx.Exec(ctx, `DELETE FROM user_clipboard WHERE user_id=$1 AND id NOT IN
 (SELECT id FROM user_clipboard WHERE user_id=$1 ORDER BY pinned DESC,sequence DESC LIMIT 50)`, user)
	if e != nil {
		return item, e
	}
	return item, tx.Commit(ctx)
}
func (s *Store) ListClipboard(ctx context.Context, user, search string) ([]ClipboardItem, error) {
	rows, e := s.pool.Query(ctx, `SELECT id,text,updated_at,pinned,device FROM user_clipboard WHERE user_id=$1
 AND strpos(lower(text),lower($2))>0 ORDER BY pinned DESC,sequence DESC LIMIT 50`, user, search)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []ClipboardItem{}
	for rows.Next() {
		var x ClipboardItem
		if e = rows.Scan(&x.ID, &x.Text, &x.UpdatedAt, &x.Pinned, &x.Device); e != nil {
			return nil, e
		}
		items = append(items, x)
	}
	return items, rows.Err()
}
func (s *Store) DeleteClipboard(ctx context.Context, user, id string) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	result, e := tx.Exec(ctx, "DELETE FROM user_clipboard WHERE user_id=$1 AND ($2='' OR id=$2)", user, id)
	if e != nil {
		return e
	}
	if id != "" && result.RowsAffected() == 0 {
		return errDataNotFound
	}
	return tx.Commit(ctx)
}
func (a *Service) clipboardSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		Enabled *bool `json:"enabled"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.Enabled == nil {
		writeError(w, 400, "enabled_required")
		return
	}
	if e := a.store.SetClipboardEnabled(r.Context(), p.UserID, *v.Enabled); e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, map[string]bool{"enabled": *v.Enabled})
}
func (a *Service) clipboard(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	switch r.Method {
	case "GET":
		search := r.URL.Query().Get("q")
		if !utf8.ValidString(search) || len(search) > 1024 || strings.ContainsRune(search, 0) {
			writeError(w, 400, "invalid_search")
			return
		}
		enabled, e := a.store.ClipboardEnabled(r.Context(), p.UserID)
		if e != nil {
			a.error(w, e)
			return
		}
		days, e := a.store.ClipboardRetention(r.Context(), p.UserID)
		if e != nil {
			a.error(w, e)
			return
		}
		items, e := a.store.ListClipboard(r.Context(), p.UserID, search)
		if e != nil {
			a.error(w, e)
			return
		}
		write(w, 200, map[string]any{"enabled": enabled, "retention_days": days, "items": items})
	case "POST":
		var v struct {
			Text string `json:"text"`
		}
		if !readSized(w, r, &v, 32768) {
			return
		}
		if !utf8.ValidString(v.Text) || strings.TrimSpace(v.Text) == "" || strings.ContainsRune(v.Text, 0) || len(utf16.Encode([]rune(v.Text))) > 4000 {
			writeError(w, 400, "invalid_clipboard_text")
			return
		}
		device, e := a.store.sessionDevice(r.Context(), p.SessionID)
		if e != nil {
			a.error(w, e)
			return
		}
		item, e := a.store.AddClipboardFrom(r.Context(), p.UserID, device, v.Text)
		if errors.Is(e, errClipboardDisabled) {
			writeError(w, 403, "clipboard_sync_disabled")
			return
		}
		if e != nil {
			a.error(w, e)
			return
		}
		write(w, 200, item)
	case "DELETE":
		id := r.PathValue("id")
		if e := a.store.DeleteClipboard(r.Context(), p.UserID, id); errors.Is(e, errDataNotFound) {
			writeError(w, 404, "not_found")
			return
		} else if e != nil {
			a.error(w, e)
			return
		}
		w.WriteHeader(204)
	}
}

// clipboardRetention 处理 PUT /v1/users/me/clipboard/retention：设置云剪贴板的保留天数（0、1、7、30，0 表示一直保留）。不改 PUT /clipboard/settings 的请求体，旧客户端照常工作。
func (a *Service) clipboardRetention(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		Days *int `json:"days"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.Days == nil || !clipboardRetentionDays[*v.Days] {
		writeError(w, 400, "invalid_retention")
		return
	}
	if e := a.store.SetClipboardRetention(r.Context(), p.UserID, *v.Days); e != nil {
		a.error(w, e)
		return
	}
	w.WriteHeader(204)
}

// clipboardPin 处理 PUT /v1/users/me/clipboard/{id}/pin：置顶或取消置顶一条条目，别人的或不存在的条目返回 404。
func (a *Service) clipboardPin(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		Pinned *bool `json:"pinned"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.Pinned == nil {
		writeError(w, 400, "pinned_required")
		return
	}
	id := r.PathValue("id")
	if len(id) != 64 {
		writeError(w, 404, "not_found")
		return
	}
	if e := a.store.PinClipboard(r.Context(), p.UserID, id, *v.Pinned); errors.Is(e, errDataNotFound) {
		writeError(w, 404, "not_found")
		return
	} else if e != nil {
		a.error(w, e)
		return
	}
	w.WriteHeader(204)
}
