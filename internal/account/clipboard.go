package account

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

var errClipboardDisabled = errors.New("clipboard_sync_disabled")
var errDataNotFound = errors.New("user_data_not_found")

// ClipboardItem 是云剪贴板里的一条密文条目。服务端看不到明文，`Ciphertext` 由客户端用 `KeyID` 对应的剪贴板密钥加密。
type ClipboardItem struct {
	ID         string    `json:"id"`
	Ciphertext string    `json:"ciphertext"`
	KeyID      string    `json:"key_id"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Pinned 的条目排在最前，不受保留期清理，也不会被五十条上限挤掉。
	Pinned bool `json:"pinned"`
	// Device 是写入时所用会话的设备名（由登录时记录的 User-Agent 解析），认不出时为空字符串。
	Device string `json:"device"`
}

// ClipboardUpload 是客户端上传的一条密文条目：`Dedup` 是明文的 HMAC 标签，相同内容只保留一条。
type ClipboardUpload struct {
	Ciphertext string `json:"ciphertext"`
	KeyID      string `json:"key_id"`
	Dedup      string `json:"dedup"`
}

// ClipboardSettings 是一个账号的云剪贴板设置：同步开关、保留天数和当前密钥编号（还没有密钥时为 nil）。
type ClipboardSettings struct {
	Enabled       bool
	RetentionDays int
	KeyID         *string
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

// ClipboardSettings 用一条语句读出同步开关、保留天数和当前密钥编号；没有设置行时是关闭、一直保留、没有密钥。
func (s *Store) ClipboardSettings(ctx context.Context, user string) (ClipboardSettings, error) {
	var v ClipboardSettings
	e := s.pool.QueryRow(ctx, "SELECT COALESCE(bool_or(enabled),false),COALESCE(max(retention_days),0),max(key_id) FROM user_clipboard_settings WHERE user_id=$1", user).Scan(&v.Enabled, &v.RetentionDays, &v.KeyID)
	return v, e
}

// clipboardState 在用户行锁内读出同步开关和当前密钥编号，供写入路径判断 403 和 409。
func clipboardState(ctx context.Context, tx pgx.Tx, user string) (bool, *string, error) {
	var enabled bool
	var key *string
	e := tx.QueryRow(ctx, "SELECT COALESCE(bool_or(enabled),false),max(key_id) FROM user_clipboard_settings WHERE user_id=$1", user).Scan(&enabled, &key)
	return enabled, key, e
}

// SetClipboardEnabled 打开或关闭云剪贴板。关闭时在同一个事务里删除全部条目、当前密钥、封装和设备登记，再次打开时由第一台设备重新生成密钥。
func (s *Store) SetClipboardEnabled(ctx context.Context, user string, enabled bool) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "INSERT INTO user_clipboard_settings(user_id,enabled) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET enabled=excluded.enabled,key_id=CASE WHEN excluded.enabled THEN user_clipboard_settings.key_id END", user, enabled); e != nil {
		return e
	}
	// 撤销同步时清空云端历史和密钥材料；不会操作用户设备的系统剪贴板。封装随设备的外键一起删除。
	if !enabled {
		for _, q := range []string{"DELETE FROM user_clipboard WHERE user_id=$1", "DELETE FROM user_clipboard_devices WHERE user_id=$1"} {
			if _, e = tx.Exec(ctx, q, user); e != nil {
				return e
			}
		}
	}
	return tx.Commit(ctx)
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

// AddClipboard 写入一条密文条目并记下写入设备（会话的设备名）。云剪贴板关闭时返回 errClipboardDisabled，`KeyID` 不是当前密钥（包括还没有密钥）时返回 errClipboardKeyConflict。`Dedup` 相同的条目只保留一条，移到最前并换成新的密文和设备名。
func (s *Store) AddClipboard(ctx context.Context, user, device string, v ClipboardUpload) (ClipboardItem, error) {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return ClipboardItem{}, e
	}
	defer tx.Rollback(ctx)
	enabled, key, e := clipboardState(ctx, tx, user)
	if e != nil {
		return ClipboardItem{}, e
	}
	if !enabled {
		return ClipboardItem{}, errClipboardDisabled
	}
	if key == nil || *key != v.KeyID {
		return ClipboardItem{}, errClipboardKeyConflict
	}
	// 滚动升级期间旧版本副本写入的明文条目，以及不属于当前密钥的条目，都不会再出现在列表里；趁持有用户行锁时删掉，免得它们占用五十条的名额。
	if _, e = tx.Exec(ctx, "DELETE FROM user_clipboard WHERE user_id=$1 AND (ciphertext='' OR key_id<>$2)", user, v.KeyID); e != nil {
		return ClipboardItem{}, e
	}
	var item ClipboardItem
	e = tx.QueryRow(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence,device) VALUES($1,$2,'',$3,$4,$5,(SELECT COALESCE(max(sequence),0)+1 FROM user_clipboard WHERE user_id=$2),$6)
 ON CONFLICT(user_id,text_hash) DO UPDATE SET sequence=excluded.sequence,updated_at=now(),device=excluded.device,ciphertext=excluded.ciphertext,key_id=excluded.key_id
 RETURNING id,ciphertext,key_id,updated_at,pinned,device`, randomToken(), user, v.Dedup, v.Ciphertext, v.KeyID, device).Scan(&item.ID, &item.Ciphertext, &item.KeyID, &item.UpdatedAt, &item.Pinned, &item.Device)
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

// ListClipboard 返回用密钥 key 加密的条目，置顶的在前，最多 50 条。明文条目和其他密钥的条目不返回。
func (s *Store) ListClipboard(ctx context.Context, user, key string) ([]ClipboardItem, error) {
	rows, e := s.pool.Query(ctx, `SELECT id,ciphertext,key_id,updated_at,pinned,device FROM user_clipboard WHERE user_id=$1
 AND ciphertext<>'' AND key_id=$2 ORDER BY pinned DESC,sequence DESC LIMIT 50`, user, key)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []ClipboardItem{}
	for rows.Next() {
		var x ClipboardItem
		if e = rows.Scan(&x.ID, &x.Ciphertext, &x.KeyID, &x.UpdatedAt, &x.Pinned, &x.Device); e != nil {
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
		// 服务端看不到明文，不再支持搜索：查询参数 q 被忽略，由客户端解密后自己搜索。
		settings, e := a.store.ClipboardSettings(r.Context(), p.UserID)
		if e != nil {
			a.error(w, e)
			return
		}
		items := []ClipboardItem{}
		if settings.KeyID != nil {
			if items, e = a.store.ListClipboard(r.Context(), p.UserID, *settings.KeyID); e != nil {
				a.error(w, e)
				return
			}
		}
		write(w, 200, map[string]any{"enabled": settings.Enabled, "retention_days": settings.RetentionDays, "key_id": settings.KeyID, "items": items})
	case "POST":
		var v struct {
			// Text 只用来识别旧版本客户端：带非空明文时返回 400 clipboard_e2e_required。
			Text string `json:"text"`
			ClipboardUpload
		}
		if !readSized(w, r, &v, 32768) {
			return
		}
		if v.Text != "" {
			writeError(w, 400, "clipboard_e2e_required")
			return
		}
		if !validClipboardCiphertext(v.Ciphertext) || !isLowerHex(v.Dedup, 64) {
			writeError(w, 400, "invalid_clipboard_ciphertext")
			return
		}
		device, e := a.store.sessionDevice(r.Context(), p.SessionID)
		if e != nil {
			a.error(w, e)
			return
		}
		item, e := a.store.AddClipboard(r.Context(), p.UserID, device, v.ClipboardUpload)
		if a.clipboardError(w, e) {
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
