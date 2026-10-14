package account

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// 云剪贴板端到端加密的服务端部分，协议见 msime 主仓库的 docs/cloud-clipboard-e2e.md。服务端只能检查格式，验证不了密码学内容：公钥与设备编号是否一致可以算出来，封装和密文能否解开只有设备知道。

var (
	errClipboardKeyConflict         = errors.New("clipboard_key_conflict")
	errClipboardKeyInvalid          = errors.New("invalid_clipboard_key")
	errClipboardDeviceLimit         = errors.New("clipboard_device_limit")
	errClipboardKeyRotationRequired = errors.New("clipboard_key_rotation_required")
)

const (
	// clipboardDeviceLimit 是每个账号最多登记的设备数，也是一次换钥最多带的封装数。
	clipboardDeviceLimit = 20
	// clipboardCiphertextMax 是条目密文的最大字符数（含 "v1." 前缀）。
	clipboardCiphertextMax = 16384
	// clipboardCiphertextMinBytes 是密文解码后的最小字节数：24 字节 nonce、16 字节 tag 和至少 1 字节明文。
	clipboardCiphertextMinBytes = 24 + 16 + 1
	// clipboardWrapBytes 是封装解码后的字节数：32 字节临时公钥、24 字节 nonce、32 字节密钥和 16 字节 tag。
	clipboardWrapBytes = 32 + 24 + 32 + 16
	// clipboardDeviceNameMax 是设备名的最大字符数。
	clipboardDeviceNameMax = 64
)

// ClipboardDevice 是登记过云剪贴板的一台设备。Approved 表示它持有当前密钥的封装。
type ClipboardDevice struct {
	DeviceID  string    `json:"device_id"`
	PublicKey string    `json:"public_key"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Approved  bool      `json:"approved"`
}

// ClipboardKeys 是 GET /v1/users/me/clipboard/keys 的响应：当前密钥编号、给请求设备的封装（都可能为 null）和全部设备。
type ClipboardKeys struct {
	Enabled bool              `json:"enabled"`
	KeyID   *string           `json:"key_id"`
	Wrap    *string           `json:"wrap"`
	Devices []ClipboardDevice `json:"devices"`
}

// ClipboardWrap 是当前密钥给一台设备的封装。
type ClipboardWrap struct {
	DeviceID string `json:"device_id"`
	Wrap     string `json:"wrap"`
}

// ClipboardKeyChange 是 POST /v1/users/me/clipboard/keys 的请求体：Previous 必须等于当前密钥编号（没有密钥时为 nil），KeyID 必须与当前编号不同，成功后成为当前密钥。
type ClipboardKeyChange struct {
	Previous *string         `json:"previous_key_id"`
	KeyID    string          `json:"key_id"`
	Wraps    []ClipboardWrap `json:"wraps"`
	Revoke   []string        `json:"revoke"`
}

// isLowerHex 判断 s 是否恰好是 n 个小写十六进制字符。
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// decodeClipboardBase64 按不带填充的 base64url 严格解码。标准库会跳过换行符、接受末尾多余的位，这里先限定字符集并用 Strict 拒绝非规范编码，保证每个值只有一种写法。
func decodeClipboardBase64(s string) ([]byte, bool) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return nil, false
		}
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	return b, e == nil
}

// validClipboardDevice 检查设备编号和公钥：公钥是 32 字节，编号是公钥 SHA-256 前 16 字节的小写十六进制。
func validClipboardDevice(id, publicKey string) bool {
	key, ok := decodeClipboardBase64(publicKey)
	if !ok || len(key) != 32 || !isLowerHex(id, 32) {
		return false
	}
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:16]) == id
}

// validClipboardDeviceName 检查设备名：不超过 64 个字符、不含控制字符，可以为空。
func validClipboardDeviceName(name string) bool {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > clipboardDeviceNameMax {
		return false
	}
	return strings.IndexFunc(name, unicode.IsControl) < 0
}

// validClipboardWrap 检查封装："v1." 加 139 个 base64url 字符，解码后 104 字节。
func validClipboardWrap(wrap string) bool {
	body, ok := strings.CutPrefix(wrap, "v1.")
	if !ok {
		return false
	}
	b, ok := decodeClipboardBase64(body)
	return ok && len(b) == clipboardWrapBytes
}

// validClipboardCiphertext 检查条目密文："v1." 加 base64url，总长不超过 16384 个字符，解码后至少 41 字节。
func validClipboardCiphertext(ciphertext string) bool {
	body, ok := strings.CutPrefix(ciphertext, "v1.")
	if !ok || len(ciphertext) > clipboardCiphertextMax {
		return false
	}
	b, ok := decodeClipboardBase64(body)
	return ok && len(b) >= clipboardCiphertextMinBytes
}

// validClipboardKeyChange 检查换钥请求里不需要查库的部分：新密钥编号的格式，1 到 20 个封装，封装的设备编号不重复、格式正确且不在 revoke 里，revoke 最多 20 项且格式正确。
func validClipboardKeyChange(c ClipboardKeyChange) bool {
	if !isLowerHex(c.KeyID, 32) || len(c.Wraps) == 0 || len(c.Wraps) > clipboardDeviceLimit || len(c.Revoke) > clipboardDeviceLimit {
		return false
	}
	revoked := map[string]bool{}
	for _, id := range c.Revoke {
		if !isLowerHex(id, 32) {
			return false
		}
		revoked[id] = true
	}
	seen := map[string]bool{}
	for _, w := range c.Wraps {
		if !isLowerHex(w.DeviceID, 32) || !validClipboardWrap(w.Wrap) || seen[w.DeviceID] || revoked[w.DeviceID] {
			return false
		}
		seen[w.DeviceID] = true
	}
	return true
}

// ClipboardKeys 返回当前密钥编号、给 device 的封装和全部登记设备，并记下 device 最近一次来取的时间。几次读取在同一个可重复读的只读事务里完成，看到的是同一时刻的状态。
func (s *Store) ClipboardKeys(ctx context.Context, user, device string) (ClipboardKeys, error) {
	if _, e := s.pool.Exec(ctx, "UPDATE user_clipboard_devices SET last_seen=now() WHERE user_id=$1 AND device_id=$2", user, device); e != nil {
		return ClipboardKeys{}, e
	}
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return ClipboardKeys{}, e
	}
	defer tx.Rollback(ctx)
	v := ClipboardKeys{Devices: []ClipboardDevice{}}
	batch := &pgx.Batch{}
	batch.Queue("SELECT COALESCE(bool_or(enabled),false),max(key_id) FROM user_clipboard_settings WHERE user_id=$1", user).QueryRow(func(row pgx.Row) error {
		return row.Scan(&v.Enabled, &v.KeyID)
	})
	batch.Queue("SELECT max(wrap) FROM user_clipboard_wraps w JOIN user_clipboard_settings cs ON cs.user_id=w.user_id AND cs.key_id=w.key_id WHERE w.user_id=$1 AND w.device_id=$2", user, device).QueryRow(func(row pgx.Row) error {
		return row.Scan(&v.Wrap)
	})
	batch.Queue(`SELECT d.device_id,d.public_key,d.name,d.created_at,w.device_id IS NOT NULL FROM user_clipboard_devices d
 LEFT JOIN user_clipboard_settings cs ON cs.user_id=d.user_id
 LEFT JOIN user_clipboard_wraps w ON w.user_id=d.user_id AND w.device_id=d.device_id AND w.key_id=cs.key_id
	 WHERE d.user_id=$1 ORDER BY d.created_at,d.device_id`, user).Query(func(rows pgx.Rows) error {
		var err error
		v.Devices, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClipboardDevice, error) {
			var d ClipboardDevice
			return d, row.Scan(&d.DeviceID, &d.PublicKey, &d.Name, &d.CreatedAt, &d.Approved)
		})
		if v.Devices == nil {
			v.Devices = []ClipboardDevice{}
		}
		return err
	})
	// 三次只读查询共享同一个可重复读快照，批量发送把三次往返合成一次。
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return v, e
	}
	return v, tx.Commit(ctx)
}

// PutClipboardDevice 登记一台设备；已登记时只更新名称。满 20 台时先淘汰最早登记、尚未批准的一台，都已批准时返回 errClipboardDeviceLimit。
func (s *Store) PutClipboardDevice(ctx context.Context, user, device, publicKey, name string) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	result, e := tx.Exec(ctx, "UPDATE user_clipboard_devices SET name=$3,last_seen=now() WHERE user_id=$1 AND device_id=$2", user, device, name)
	if e != nil {
		return e
	}
	if result.RowsAffected() == 1 {
		return tx.Commit(ctx)
	}
	var inserted bool
	if e = tx.QueryRow(ctx, `WITH device_count AS (
 SELECT count(*) AS n FROM user_clipboard_devices WHERE user_id=$1
), evicted AS (
 DELETE FROM user_clipboard_devices d
 WHERE d.user_id=$1 AND (SELECT n FROM device_count) >= $5 AND d.device_id=(
  SELECT d2.device_id FROM user_clipboard_devices d2 LEFT JOIN user_clipboard_settings cs ON cs.user_id=d2.user_id
  WHERE d2.user_id=$1 AND NOT EXISTS(SELECT 1 FROM user_clipboard_wraps w WHERE w.user_id=d2.user_id AND w.device_id=d2.device_id AND w.key_id=cs.key_id)
  ORDER BY d2.created_at,d2.device_id LIMIT 1)
 RETURNING 1
), inserted AS (
 INSERT INTO user_clipboard_devices(user_id,device_id,public_key,name)
 SELECT $1,$2,$3,$4
 WHERE (SELECT n FROM device_count) < $5 OR EXISTS(SELECT 1 FROM evicted)
 RETURNING 1
)
SELECT EXISTS(SELECT 1 FROM inserted)`, user, device, publicKey, name, clipboardDeviceLimit).Scan(&inserted); e != nil {
		return e
	}
	if !inserted {
		return errClipboardDeviceLimit
	}
	return tx.Commit(ctx)
}

// DeleteClipboardDevice 在用户行锁内检查批准状态后删除待批准设备和它的旧封装；不存在时返回 errDataNotFound，持有当前密钥封装时返回 errClipboardKeyRotationRequired。移除已批准的设备要换钥，见 RotateClipboardKey。
func (s *Store) DeleteClipboardDevice(ctx context.Context, user, device string) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var approved, deleted bool
	// 在同一语句内检查当前密钥的封装并删除待批准设备，用户行锁仍覆盖并发批准和换钥。
	e = tx.QueryRow(ctx, `WITH device_state AS (
 SELECT EXISTS(SELECT 1 FROM user_clipboard_wraps w WHERE w.user_id=d.user_id AND w.device_id=d.device_id AND w.key_id=cs.key_id) AS approved
 FROM user_clipboard_devices d LEFT JOIN user_clipboard_settings cs ON cs.user_id=d.user_id
 WHERE d.user_id=$1 AND d.device_id=$2
), deleted AS (
 DELETE FROM user_clipboard_devices WHERE user_id=$1 AND device_id=$2
 AND EXISTS(SELECT 1 FROM device_state WHERE NOT approved)
 RETURNING 1
)
SELECT EXISTS(SELECT 1 FROM device_state WHERE approved),EXISTS(SELECT 1 FROM deleted)`, user, device).Scan(&approved, &deleted)
	if e != nil {
		return e
	}
	if approved {
		return errClipboardKeyRotationRequired
	}
	if !deleted {
		return errDataNotFound
	}
	return tx.Commit(ctx)
}

// RotateClipboardKey 换钥：在持有用户行锁的事务里确认当前密钥编号等于 c.Previous、新编号与当前编号不同（比较后写入，并发换钥只有一个成功），删除 revoke 里的设备、全部条目和全部旧封装，再写入新密钥编号和封装。c 已经过 validClipboardKeyChange 检查。云剪贴板关闭时返回 errClipboardDisabled，旧编号不符返回 errClipboardKeyConflict，新编号未改变或封装给了没登记的设备返回 errClipboardKeyInvalid。
func (s *Store) RotateClipboardKey(ctx context.Context, user string, c ClipboardKeyChange) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	enabled, key, e := clipboardState(ctx, tx, user)
	if e != nil {
		return e
	}
	if !enabled {
		return errClipboardDisabled
	}
	if (key == nil) != (c.Previous == nil) || (key != nil && *key != *c.Previous) {
		return errClipboardKeyConflict
	}
	if key != nil && *key == c.KeyID {
		return errClipboardKeyInvalid
	}
	revoke := c.Revoke
	if revoke == nil {
		revoke = []string{}
	}
	devices := make([]string, len(c.Wraps))
	wraps := make([]string, len(c.Wraps))
	for i, w := range c.Wraps {
		devices[i], wraps[i] = w.DeviceID, w.Wrap
	}
	var registered int
	// 同一语句的计数读取删除前的快照，显式排除撤销设备才能得到删除后的登记数。
	if e = tx.QueryRow(ctx, `WITH revoked AS (
 DELETE FROM user_clipboard_devices WHERE user_id=$1 AND device_id=ANY($3)
)
SELECT count(*) FROM user_clipboard_devices
WHERE user_id=$1 AND device_id=ANY($2) AND NOT (device_id=ANY($3))`, user, devices, revoke).Scan(&registered); e != nil {
		return e
	}
	if registered != len(devices) {
		return errClipboardKeyInvalid
	}
	// 校验通过后，一次发送历史清理和新密钥写入；保留先删旧封装再写新封装的顺序。
	batch := &pgx.Batch{}
	batch.Queue("DELETE FROM user_clipboard WHERE user_id=$1", user)
	batch.Queue("DELETE FROM user_clipboard_wraps WHERE user_id=$1", user)
	batch.Queue("UPDATE user_clipboard_settings SET key_id=$2 WHERE user_id=$1", user, c.KeyID)
	batch.Queue("INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) SELECT $1,d,$2,w FROM unnest($3::text[],$4::text[]) AS t(d,w)", user, c.KeyID, devices, wraps)
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// PutClipboardWrap 写入当前密钥给一台设备的封装（批准这台设备），已有封装时替换。key 不是当前密钥返回 errClipboardKeyConflict，设备没登记返回 errDataNotFound。
func (s *Store) PutClipboardWrap(ctx context.Context, user, key, device, wrap string) error {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var current, written bool
	// 在同一语句内校验当前密钥并写入已登记设备的封装，继续由用户行锁保护并发换钥。
	e = tx.QueryRow(ctx, `WITH current_key AS (
 SELECT key_id FROM user_clipboard_settings WHERE user_id=$1
), written AS (
 INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap)
 SELECT user_id,device_id,$3,$4 FROM user_clipboard_devices WHERE user_id=$1 AND device_id=$2
 AND EXISTS(SELECT 1 FROM current_key WHERE key_id=$3)
 ON CONFLICT(user_id,device_id) DO UPDATE SET key_id=excluded.key_id,wrap=excluded.wrap
 RETURNING 1
)
SELECT EXISTS(SELECT 1 FROM current_key WHERE key_id=$3),EXISTS(SELECT 1 FROM written)`, user, device, key, wrap).Scan(&current, &written)
	if e != nil {
		return e
	}
	if !current {
		return errClipboardKeyConflict
	}
	if !written {
		return errDataNotFound
	}
	return tx.Commit(ctx)
}

// clipboardError 把云剪贴板写入路径的错误映射成接口错误；e 为 nil 时返回 false，调用方继续写成功响应。
func (a *Service) clipboardError(w http.ResponseWriter, e error) bool {
	switch {
	case e == nil:
		return false
	case errors.Is(e, errClipboardDisabled):
		writeError(w, 403, "clipboard_sync_disabled")
	case errors.Is(e, errClipboardKeyConflict):
		writeError(w, 409, "clipboard_key_conflict")
	case errors.Is(e, errClipboardKeyInvalid):
		writeError(w, 400, "invalid_clipboard_key")
	case errors.Is(e, errClipboardDeviceLimit):
		writeError(w, 409, "clipboard_device_limit")
	case errors.Is(e, errClipboardKeyRotationRequired):
		writeError(w, 409, "clipboard_key_rotation_required")
	case errors.Is(e, errDataNotFound):
		writeError(w, 404, "not_found")
	default:
		a.error(w, e)
	}
	return true
}

// clipboardKeys 处理 GET /v1/users/me/clipboard/keys?device_id= 和 POST /v1/users/me/clipboard/keys（换钥）。
func (a *Service) clipboardKeys(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if r.Method == "GET" {
		device := r.URL.Query().Get("device_id")
		if !isLowerHex(device, 32) {
			writeError(w, 400, "invalid_device_id")
			return
		}
		keys, e := a.store.ClipboardKeys(r.Context(), p.UserID, device)
		if e != nil {
			a.error(w, e)
			return
		}
		write(w, 200, keys)
		return
	}
	var c ClipboardKeyChange
	if !read(w, r, &c) {
		return
	}
	if !validClipboardKeyChange(c) {
		writeError(w, 400, "invalid_clipboard_key")
		return
	}
	if a.clipboardError(w, a.store.RotateClipboardKey(r.Context(), p.UserID, c)) {
		return
	}
	w.WriteHeader(204)
}

// clipboardDevice 处理 PUT 和 DELETE /v1/users/me/clipboard/devices/{device_id}：登记或改名一台设备，删除（拒绝）一台设备。
func (a *Service) clipboardDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	device := r.PathValue("device_id")
	if r.Method == "DELETE" {
		if a.clipboardError(w, a.store.DeleteClipboardDevice(r.Context(), p.UserID, device)) {
			return
		}
		w.WriteHeader(204)
		return
	}
	var v struct {
		PublicKey string `json:"public_key"`
		Name      string `json:"name"`
	}
	if !read(w, r, &v) {
		return
	}
	if !validClipboardDevice(device, v.PublicKey) || !validClipboardDeviceName(v.Name) {
		writeError(w, 400, "invalid_clipboard_device")
		return
	}
	if a.clipboardError(w, a.store.PutClipboardDevice(r.Context(), p.UserID, device, v.PublicKey, v.Name)) {
		return
	}
	w.WriteHeader(204)
}

// clipboardWrap 处理 PUT /v1/users/me/clipboard/keys/{key_id}/wraps/{device_id}：用当前密钥的封装批准一台设备。
func (a *Service) clipboardWrap(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		Wrap string `json:"wrap"`
	}
	if !read(w, r, &v) {
		return
	}
	if !validClipboardWrap(v.Wrap) {
		writeError(w, 400, "invalid_clipboard_key")
		return
	}
	if a.clipboardError(w, a.store.PutClipboardWrap(r.Context(), p.UserID, r.PathValue("key_id"), r.PathValue("device_id"), v.Wrap)) {
		return
	}
	w.WriteHeader(204)
}

// clipboardItemPut 分发 PUT /v1/users/me/clipboard/{id}/{action}。PUT /clipboard/{id}/pin 和 PUT /clipboard/devices/{device_id} 都能匹配 /clipboard/devices/pin，ServeMux 认为两个模式冲突、注册时 panic，所以合并成一个模式在这里分开：条目 ID 是 64 位十六进制，不会是 devices。
func (a *Service) clipboardItemPut(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.PathValue("id") == "devices":
		r.SetPathValue("device_id", r.PathValue("action"))
		a.clipboardDevice(w, r)
	case r.PathValue("action") == "pin":
		a.clipboardPin(w, r)
	default:
		writeError(w, 404, "not_found")
	}
}
