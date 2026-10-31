package account

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/bits"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpdeadline"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

// 输入习惯同步（learning 域）的服务端部分，协议见 msime 主仓库的 docs/learning-sync.md。它是云剪贴板端到端加密的又一个实例：设备身份、公钥编号和封装的格式与剪贴板相同（共用 e2e_format.go 的校验），但设备、密钥、封装和密文都在自己的表里。每台设备在服务端只有一份密文信封（MSL1），服务端只检查格式和代次，验证不了密码学内容，也读不到明文。
//
// 写入和删除密文、批准设备都只认调用会话绑定的那台设备：设备在登记时绑定会话（`Principal.SessionID`，刷新令牌不换会话），会话被吊销或过期时设备随之删除。

var (
	errLearningRequiresAccount  = errors.New("learning_requires_account")
	errLearningDeviceMismatch   = errors.New("learning_device_mismatch")
	errLearningKeyConflict      = errors.New("learning_key_conflict")
	errLearningKeyInvalid       = errors.New("invalid_learning_key")
	errLearningDeviceLimit      = errors.New("learning_device_limit")
	errLearningRevisionConflict = errors.New("revision_conflict")
	errLearningQuota            = errors.New("learning_quota")
)

const (
	// learningDeviceLimit 是每个账号最多登记的设备数，也是一次换钥最多移除的设备数。
	learningDeviceLimit = 20
	// learningBlobMax 是一台设备的密文信封（明文头加密文）的上限，8 MiB。
	learningBlobMax = 8 << 20
	// learningQuota 是一个账号全部密文信封合计的上限，64 MiB。
	learningQuota = 64 << 20
	// learningHeaderBytes 是信封的明文头：4 字节 "MSL1"、16 字节 key_id、16 字节 device_id、8 字节代次和 24 字节 nonce。
	learningHeaderBytes = 4 + 16 + 16 + 8 + 24
	// learningTagBytes 是 AEAD 的 tag 长度；信封总长减去明文头和 tag 就是补齐后的明文长度。
	learningTagBytes = 16
	// learningRevisionMax 是代次的上限 2^53-1，JSON 数字和 bigint 都能精确表示。
	learningRevisionMax = 1<<53 - 1
	// learningWritesPerHour、learningReadsPerHour 是每个用户每小时的写入（登记、批准、换钥、上传和删除）和读取（元数据和密文下载）次数，按 auth_rates 在所有副本间共享。额度按设备上限定：20 台设备时，一次换钥后的重新批准要 1 次换钥、19 次批准和 20 次上传，每台设备每小时自动上传一次又是 20 次写入；每台设备每轮自动同步读一次元数据、下载其余 19 台的密文，20 台合计每小时 400 次读取。协议最初定的每小时 PUT 30 次、GET 240 次在设备接近上限时不够，这里按上面的用量留出手动同步和等待批准时轮询的余量，协议文档同步修改。
	learningWritesPerHour = 120
	learningReadsPerHour  = 960
	// learningTransferTimeout 是上传和下载密文的路由时限，也是这两条路由的连接读写期限。
	learningTransferTimeout = 60 * time.Second
	// learningInactiveDays 是设备多久没有请求就被清理：设备行、封装和密文一起删除，见 Store.Prune。设备绑定会话，会话最长 30 天，过期清理时设备随外键删除，所以按现在的会话时长这一条不会先触发；它是协议独立于会话时长的规则，会话时长以后放宽时仍然生效。
	learningInactiveDays = 180
)

// learningTransferSlots 限制每个副本同时上传或下载的密文数，每份最多 8 MiB，整份放在内存里。
var learningTransferSlots = make(chan struct{}, 8)

// LearningDevice 是登记过输入习惯同步的一台设备。Approved 表示它持有当前密钥的封装。
type LearningDevice struct {
	DeviceID  string    `json:"device_id"`
	PublicKey string    `json:"public_key"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	Approved  bool      `json:"approved"`
}

// LearningWrap 是当前密钥给一台设备的封装，以及封装它的批准者。
type LearningWrap struct {
	Wrap             string `json:"wrap"`
	ApproverDeviceID string `json:"approver_device_id"`
}

// LearningBlob 是一台设备的密文信封的元数据。
type LearningBlob struct {
	DeviceID  string    `json:"device_id"`
	KeyID     string    `json:"key_id"`
	Revision  int64     `json:"revision"`
	Bytes     int64     `json:"bytes"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LearningState 是 GET /v1/users/me/learning 的响应：当前密钥编号和给请求设备的封装（都可能为 null）、全部设备和全部密文的元数据。
type LearningState struct {
	KeyID   *string          `json:"key_id"`
	Wrap    *LearningWrap    `json:"wrap"`
	Devices []LearningDevice `json:"devices"`
	Blobs   []LearningBlob   `json:"blobs"`
}

// learningCaller 是调用会话在 learning 域里的状态：账号当前的密钥编号（没有密钥时为 nil）、会话绑定的设备（没有时为空字符串）和这台设备是否持有当前密钥的封装。
type learningCaller struct {
	key      *string
	device   string
	approved bool
}

// learningCallerQuery 在一条语句里锁住用户行和调用会话，读出封禁状态、会话是否仍有效、是否只有匿名身份、当前密钥编号和会话绑定的设备，并刷新这台设备的最后在线时间。%s 是用户行的锁（写入 FOR UPDATE，读取 FOR SHARE）。会话行加 FOR SHARE，与吊销会话的 UPDATE 互斥：吊销要么等这个事务提交后由触发器删掉设备，要么先提交，这里就读不到会话，不会把设备留给已吊销的会话。锁的顺序是用户、会话、设备，与吊销触发器的会话、设备一致。
const learningCallerQuery = `WITH locked AS MATERIALIZED (
 SELECT id,banned_at IS NOT NULL AS banned FROM auth_users WHERE id=$1 %s
), live AS MATERIALIZED (
 SELECT id FROM auth_sessions WHERE id=$2 AND user_id=$1 AND NOT revoked AND EXISTS(SELECT 1 FROM locked) FOR SHARE
), bound AS (
 UPDATE user_learning_devices SET last_seen=now() WHERE user_id=$1 AND session_id=$2 AND EXISTS(SELECT 1 FROM live)
 RETURNING device_id
)
SELECT l.banned,EXISTS(SELECT 1 FROM live),
 EXISTS(SELECT 1 FROM auth_identities WHERE user_id=$1 AND provider='anonymous') AND NOT EXISTS(SELECT 1 FROM auth_identities WHERE user_id=$1 AND provider<>'anonymous'),
 s.key_id,COALESCE(b.device_id,''),
 EXISTS(SELECT 1 FROM user_learning_wraps w WHERE w.user_id=$1 AND w.device_id=b.device_id AND w.key_id=s.key_id)
FROM locked l LEFT JOIN user_learning_settings s ON s.user_id=l.id LEFT JOIN bound b ON true`

var (
	learningWriteCallerQuery = formatLearningCallerQuery("FOR UPDATE")
	learningReadCallerQuery  = formatLearningCallerQuery("FOR SHARE")
)

func formatLearningCallerQuery(lock string) string {
	return strings.Replace(learningCallerQuery, "%s", lock, 1)
}

// learningTransaction 开始一个 learning 域的事务并读出调用会话的状态。write 为 true 时锁定用户行并拒绝被封禁的账号，与其他用户数据写入共用同一把行锁；为 false 时只加共享锁。用户不存在或会话已失效返回 errMissingUser，只有匿名身份的账号返回 errLearningRequiresAccount。
func (s *Store) learningTransaction(ctx context.Context, user, session string, write bool) (pgx.Tx, learningCaller, error) {
	query := learningReadCallerQuery
	if write {
		query = learningWriteCallerQuery
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return nil, learningCaller{}, e
	}
	var caller learningCaller
	var banned, live, anonymous bool
	e = tx.QueryRow(ctx, query, user, session).Scan(&banned, &live, &anonymous, &caller.key, &caller.device, &caller.approved)
	switch {
	case errors.Is(e, pgx.ErrNoRows):
		e = errMissingUser
	case e != nil:
	case write && banned:
		e = ErrBanned
	case !live:
		e = errMissingUser
	case anonymous:
		e = errLearningRequiresAccount
	}
	if e != nil {
		tx.Rollback(ctx)
		return nil, learningCaller{}, e
	}
	return tx, caller, nil
}

// LearningState 在同一快照里读出当前密钥编号、给 device 的封装、全部设备和全部密文的元数据，并刷新调用会话绑定设备的最后在线时间。
func (s *Store) LearningState(ctx context.Context, user, session, device string) (LearningState, error) {
	tx, caller, e := s.learningTransaction(ctx, user, session, false)
	if e != nil {
		return LearningState{}, e
	}
	defer tx.Rollback(ctx)
	v := LearningState{KeyID: caller.key, Devices: []LearningDevice{}, Blobs: []LearningBlob{}}
	batch := &pgx.Batch{}
	batch.Queue(`SELECT d.device_id,d.public_key,d.name,d.created_at,d.last_seen,
 EXISTS(SELECT 1 FROM user_learning_wraps w WHERE w.user_id=d.user_id AND w.device_id=d.device_id AND w.key_id=$2)
FROM user_learning_devices d WHERE d.user_id=$1 ORDER BY d.created_at,d.device_id`, user, caller.key).Query(func(rows pgx.Rows) error {
		var err error
		v.Devices, err = pgx.AppendRows(v.Devices, rows, func(row pgx.CollectableRow) (LearningDevice, error) {
			var d LearningDevice
			return d, row.Scan(&d.DeviceID, &d.PublicKey, &d.Name, &d.CreatedAt, &d.LastSeen, &d.Approved)
		})
		return err
	})
	batch.Queue(`SELECT wrap,approver_device_id FROM user_learning_wraps WHERE user_id=$1 AND device_id=$2 AND key_id=$3`, user, device, caller.key).Query(func(rows pgx.Rows) error {
		wraps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LearningWrap, error) {
			var w LearningWrap
			return w, row.Scan(&w.Wrap, &w.ApproverDeviceID)
		})
		if len(wraps) == 1 {
			v.Wrap = &wraps[0]
		}
		return err
	})
	batch.Queue(`SELECT device_id,key_id,revision,bytes,updated_at FROM user_learning_blobs WHERE user_id=$1 ORDER BY device_id`, user).Query(func(rows pgx.Rows) error {
		var err error
		v.Blobs, err = pgx.AppendRows(v.Blobs, rows, func(row pgx.CollectableRow) (LearningBlob, error) {
			var b LearningBlob
			return b, row.Scan(&b.DeviceID, &b.KeyID, &b.Revision, &b.Bytes, &b.UpdatedAt)
		})
		return err
	})
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return LearningState{}, e
	}
	return v, tx.Commit(ctx)
}

// PutLearningDevice 登记一台设备并绑定调用会话；这台设备已由同一会话登记时只更新名称。设备已绑定在别的有效会话上返回 errLearningDeviceMismatch。调用会话之前绑定过另一台设备时（例如本机密钥串丢了、重新生成了身份），那台设备连同封装和密文一起被替换，因为一个会话只绑定一台设备。设备已满 20 台时返回 errLearningDeviceLimit。
//
// 登记前先删掉这个账号绑定在已过期会话上的设备，没有设备时一并删除密钥编号，与清理任务删除过期会话时的级联结果相同：过期会话要等每小时的 Store.Prune 才删除，在这之前同一台设备重新登录后登记会被误判为绑定在别的会话上，过期会话的设备也会占着设备名额。删掉的设备不保留批准，重新登记后要重新批准。
func (s *Store) PutLearningDevice(ctx context.Context, user, session, device, publicKey, name string) error {
	tx, caller, e := s.learningTransaction(ctx, user, session, true)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	expired := &pgx.Batch{}
	expired.Queue(`DELETE FROM user_learning_devices d USING auth_sessions s WHERE d.user_id=$1 AND s.id=d.session_id AND s.expires_at<=now()`, user)
	expired.Queue(`DELETE FROM user_learning_settings WHERE user_id=$1 AND NOT EXISTS(SELECT 1 FROM user_learning_devices WHERE user_id=$1)`, user)
	if e = tx.SendBatch(ctx, expired).Close(); e != nil {
		return e
	}
	var owner *string
	var others int
	// 同一条语句读出这台设备绑定的会话和账号里其他设备的数量（不算会被替换掉的调用会话的旧设备）。
	if e = tx.QueryRow(ctx, `SELECT (SELECT session_id FROM user_learning_devices WHERE user_id=$1 AND device_id=$2),
 (SELECT count(*) FROM user_learning_devices WHERE user_id=$1 AND device_id<>$2 AND session_id<>$3)`, user, device, session).Scan(&owner, &others); e != nil {
		return e
	}
	if owner != nil {
		if *owner != session {
			return errLearningDeviceMismatch
		}
		if _, e = tx.Exec(ctx, `UPDATE user_learning_devices SET name=$3 WHERE user_id=$1 AND device_id=$2`, user, device, name); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if others >= learningDeviceLimit {
		return errLearningDeviceLimit
	}
	batch := &pgx.Batch{}
	if caller.device != "" {
		batch.Queue(`DELETE FROM user_learning_devices WHERE user_id=$1 AND device_id=$2`, user, caller.device)
	}
	batch.Queue(`INSERT INTO user_learning_devices(user_id,device_id,public_key,name,session_id) VALUES($1,$2,$3,$4,$5)`, user, device, publicKey, name, session)
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// DeleteLearningDevice 删除一台设备、它的封装和密文；删掉的是最后一台时一并删除密钥编号。设备不存在返回 errDataNotFound。协议里这个接口用于拒绝待批准的设备和本机关闭功能，移除一台已批准的设备由客户端走换钥；服务端不拦已批准的设备，协议的威胁模型写明了任何有效会话都能清空 learning 域。
func (s *Store) DeleteLearningDevice(ctx context.Context, user, session, device string) error {
	tx, _, e := s.learningTransaction(ctx, user, session, true)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var deleted bool
	batch := &pgx.Batch{}
	batch.Queue(`WITH deleted AS (DELETE FROM user_learning_devices WHERE user_id=$1 AND device_id=$2 RETURNING 1)
SELECT EXISTS(SELECT 1 FROM deleted)`, user, device).QueryRow(func(row pgx.Row) error { return row.Scan(&deleted) })
	batch.Queue(`DELETE FROM user_learning_settings WHERE user_id=$1 AND NOT EXISTS(SELECT 1 FROM user_learning_devices WHERE user_id=$1)`, user)
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return e
	}
	if !deleted {
		return errDataNotFound
	}
	return tx.Commit(ctx)
}

// CreateLearningKey 建立账号的第一把密钥：写入密钥编号和给调用会话绑定设备的封装，批准者就是它自己。调用会话没有登记设备返回 errLearningKeyInvalid，账号已有密钥返回 errLearningKeyConflict。
func (s *Store) CreateLearningKey(ctx context.Context, user, session, key, wrap string) error {
	tx, caller, e := s.learningTransaction(ctx, user, session, true)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if caller.device == "" {
		return errLearningKeyInvalid
	}
	if caller.key != nil {
		return errLearningKeyConflict
	}
	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO user_learning_settings(user_id,key_id) VALUES($1,$2)`, user, key)
	batch.Queue(`INSERT INTO user_learning_wraps(user_id,device_id,key_id,wrap,approver_device_id) VALUES($1,$2,$3,$4,$2)
 ON CONFLICT(user_id,device_id) DO UPDATE SET key_id=excluded.key_id,wrap=excluded.wrap,approver_device_id=excluded.approver_device_id,created_at=now()`, user, caller.device, key, wrap)
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// PutLearningWrap 用当前密钥的封装批准一台设备，已有封装时替换。approver 必须是调用会话绑定的设备，否则返回 errLearningDeviceMismatch；key 不是当前密钥、或批准者自己不持有当前密钥的封装时返回 errLearningKeyConflict；被批准的设备没登记返回 errLearningKeyInvalid。
func (s *Store) PutLearningWrap(ctx context.Context, user, session, device, key, wrap, approver string) error {
	tx, caller, e := s.learningTransaction(ctx, user, session, true)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if caller.device == "" || caller.device != approver {
		return errLearningDeviceMismatch
	}
	if caller.key == nil || *caller.key != key || !caller.approved {
		return errLearningKeyConflict
	}
	var written bool
	if e = tx.QueryRow(ctx, `WITH written AS (
 INSERT INTO user_learning_wraps(user_id,device_id,key_id,wrap,approver_device_id)
 SELECT user_id,device_id,$3,$4,$5 FROM user_learning_devices WHERE user_id=$1 AND device_id=$2
 ON CONFLICT(user_id,device_id) DO UPDATE SET key_id=excluded.key_id,wrap=excluded.wrap,approver_device_id=excluded.approver_device_id,created_at=now()
 RETURNING 1
)
SELECT EXISTS(SELECT 1 FROM written)`, user, device, key, wrap, approver).Scan(&written); e != nil {
		return e
	}
	if !written {
		return errLearningKeyInvalid
	}
	return tx.Commit(ctx)
}

// RekeyLearning 换钥：在持有用户行锁的事务里确认当前密钥编号等于 previous（比较后写入，并发换钥只有一个成功），删除 revoke 里的设备、全部密文和全部旧封装，再写入新密钥编号和给调用会话绑定设备的封装。不要求调用设备持有旧密钥的封装：所有设备都丢了时的重置就是从一台没有封装的新设备发起的。调用会话没有登记设备、或它在 revoke 里返回 errLearningKeyInvalid，账号没有密钥或 previous 不是当前密钥返回 errLearningKeyConflict。
func (s *Store) RekeyLearning(ctx context.Context, user, session, previous, key, wrap string, revoke []string) error {
	tx, caller, e := s.learningTransaction(ctx, user, session, true)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if caller.device == "" || slices.Contains(revoke, caller.device) {
		return errLearningKeyInvalid
	}
	if caller.key == nil || *caller.key != previous {
		return errLearningKeyConflict
	}
	if revoke == nil {
		revoke = []string{}
	}
	// 先删设备再删密文和封装，与 DeleteUserData 的顺序一致；被撤销设备的封装和密文随外键一起删除。
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM user_learning_devices WHERE user_id=$1 AND device_id=ANY($2)`, user, revoke)
	batch.Queue(`DELETE FROM user_learning_blobs WHERE user_id=$1`, user)
	batch.Queue(`DELETE FROM user_learning_wraps WHERE user_id=$1`, user)
	batch.Queue(`UPDATE user_learning_settings SET key_id=$2,updated_at=now() WHERE user_id=$1`, user, key)
	batch.Queue(`INSERT INTO user_learning_wraps(user_id,device_id,key_id,wrap,approver_device_id) VALUES($1,$2,$3,$4,$2)`, user, caller.device, key, wrap)
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// LearningBlobData 返回一台设备的密文信封原样字节；没有时返回 errDataNotFound。任何会话都能读，读取方自己解密并核对信封头。
func (s *Store) LearningBlobData(ctx context.Context, user, session, device string) ([]byte, error) {
	tx, _, e := s.learningTransaction(ctx, user, session, false)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	var blob []byte
	e = tx.QueryRow(ctx, `SELECT blob FROM user_learning_blobs WHERE user_id=$1 AND device_id=$2`, user, device).Scan(&blob)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, errDataNotFound
	}
	if e != nil {
		return nil, e
	}
	return blob, tx.Commit(ctx)
}

// PutLearningBlob 写入调用会话绑定设备的密文信封，代次比较并交换。blob 已经过 learningBlobFormat 检查，key 是信封头里的密钥编号。device 不是调用会话绑定的设备返回 errLearningDeviceMismatch；key 不是当前密钥、或这台设备不持有当前密钥的封装返回 errLearningKeyConflict；revision 不大于已存的代次（没有时为 0）返回 errLearningRevisionConflict；账号其他设备的密文加上这一份超过 64 MiB 返回 errLearningQuota。
func (s *Store) PutLearningBlob(ctx context.Context, user, session, device string, revision int64, key string, blob []byte) error {
	tx, caller, e := s.learningTransaction(ctx, user, session, true)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if caller.device == "" || caller.device != device {
		return errLearningDeviceMismatch
	}
	if caller.key == nil || *caller.key != key || !caller.approved {
		return errLearningKeyConflict
	}
	var stored, others int64
	// 用户行锁让同一账号的写入串行，读出的代次和其他设备的合计在提交前不会变。
	if e = tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM user_learning_blobs WHERE user_id=$1 AND device_id=$2),0),
 COALESCE((SELECT sum(bytes) FROM user_learning_blobs WHERE user_id=$1 AND device_id<>$2),0)`, user, device).Scan(&stored, &others); e != nil {
		return e
	}
	if revision <= stored {
		return errLearningRevisionConflict
	}
	if others+int64(len(blob)) > learningQuota {
		return errLearningQuota
	}
	if _, e = tx.Exec(ctx, `INSERT INTO user_learning_blobs(user_id,device_id,key_id,revision,blob) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(user_id,device_id) DO UPDATE SET key_id=excluded.key_id,revision=excluded.revision,blob=excluded.blob,updated_at=now()`, user, device, key, revision, blob); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// DeleteLearningBlob 删除调用会话绑定设备的密文信封，删除后这台设备的代次从 0 算起。device 不是调用会话绑定的设备返回 errLearningDeviceMismatch，没有密文返回 errDataNotFound。
func (s *Store) DeleteLearningBlob(ctx context.Context, user, session, device string) error {
	tx, caller, e := s.learningTransaction(ctx, user, session, true)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if caller.device == "" || caller.device != device {
		return errLearningDeviceMismatch
	}
	result, e := tx.Exec(ctx, `DELETE FROM user_learning_blobs WHERE user_id=$1 AND device_id=$2`, user, device)
	if e != nil {
		return e
	}
	if result.RowsAffected() == 0 {
		return errDataNotFound
	}
	return tx.Commit(ctx)
}

// padme 是信封补齐后的明文长度：L < 2 时为 L；否则 E = ⌊log₂ L⌋，S = ⌊log₂ E⌋ + 1，m = 2^(E−S) − 1，结果是 (L + m) & ~m。
func padme(l uint64) uint64 {
	if l < 2 {
		return l
	}
	e := uint(bits.Len64(l) - 1)
	s := uint(bits.Len(e))
	m := uint64(1)<<(e-s) - 1
	return (l + m) &^ m
}

// learningBlobFormat 检查密文信封里服务端能检查的部分，通过时返回信封头里的密钥编号（小写十六进制），否则返回错误码：不以 "MSL1" 开头、而以 "{" 或 gzip 魔数开头的是明文，返回 learning_e2e_required；长度不在 84 字节到 8 MiB 之间、补齐后的明文长度不是 padme 的不动点、头里的设备编号与路径不同或代次不等于 revision 时返回 invalid_learning_blob。
func learningBlobFormat(blob []byte, device string, revision uint64) (string, string) {
	if !bytes.HasPrefix(blob, []byte("MSL1")) {
		if bytes.HasPrefix(blob, []byte("{")) || bytes.HasPrefix(blob, []byte{0x1f, 0x8b}) {
			return "", "learning_e2e_required"
		}
		return "", "invalid_learning_blob"
	}
	if len(blob) < learningHeaderBytes+learningTagBytes || len(blob) > learningBlobMax {
		return "", "invalid_learning_blob"
	}
	padded := uint64(len(blob) - learningHeaderBytes - learningTagBytes)
	if padme(padded) != padded || hex.EncodeToString(blob[20:36]) != device || binary.BigEndian.Uint64(blob[36:44]) != revision {
		return "", "invalid_learning_blob"
	}
	return hex.EncodeToString(blob[4:20]), ""
}

// learningError 把 learning 域的错误映射成接口错误；e 为 nil 时返回 false，调用方继续写成功响应。
func (a *Service) learningError(w http.ResponseWriter, e error) bool {
	switch {
	case e == nil:
		return false
	case errors.Is(e, errLearningRequiresAccount):
		writeError(w, 403, "learning_requires_account")
	case errors.Is(e, errLearningDeviceMismatch):
		writeError(w, 403, "learning_device_mismatch")
	case errors.Is(e, errLearningKeyConflict):
		writeError(w, 409, "learning_key_conflict")
	case errors.Is(e, errLearningKeyInvalid):
		writeError(w, 400, "invalid_learning_key")
	case errors.Is(e, errLearningDeviceLimit):
		writeError(w, 409, "learning_device_limit")
	case errors.Is(e, errLearningRevisionConflict):
		writeError(w, 409, "revision_conflict")
	case errors.Is(e, errLearningQuota):
		writeError(w, 413, "learning_quota")
	case errors.Is(e, errDataNotFound):
		writeError(w, 404, "not_found")
	default:
		a.error(w, e)
	}
	return true
}

// learningRate 按用户记一次 learning 域的读取或写入；超出时写 429 并带上当前窗口剩余的秒数，返回 false。
func (a *Service) learningRate(w http.ResponseWriter, r *http.Request, user string, write bool) bool {
	key, limit := "learning-read:"+user, learningReadsPerHour
	if write {
		key, limit = "learning-write:"+user, learningWritesPerHour
	}
	wait, e := a.store.RateUntil(r.Context(), key, limit, time.Hour)
	if errors.Is(e, ErrLimited) {
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		writeError(w, 429, "rate_limit_exceeded")
		return false
	}
	if e != nil {
		a.error(w, e)
		return false
	}
	return true
}

// learningDeviceParam 读出路径里的 device_id，格式不对时写 400 invalid_device_id 并返回 false。
func learningDeviceParam(w http.ResponseWriter, device string) bool {
	if !isLowerHex(device, 32) {
		writeError(w, 400, "invalid_device_id")
		return false
	}
	return true
}

// learningState 处理 GET /v1/users/me/learning?device_id=：当前密钥编号、给这台设备的封装、全部设备和全部密文的元数据。
func (a *Service) learningState(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	device := r.URL.Query().Get("device_id")
	if !learningDeviceParam(w, device) || !a.learningRate(w, r, p.UserID, false) {
		return
	}
	v, e := a.store.LearningState(r.Context(), p.UserID, p.SessionID, device)
	if a.learningError(w, e) {
		return
	}
	write(w, 200, v)
}

// learningDevice 处理 PUT 和 DELETE /v1/users/me/learning/devices/{device_id}：登记设备并绑定调用会话（已登记时改名），或删除一台设备。
func (a *Service) learningDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	device := r.PathValue("device_id")
	if r.Method == "DELETE" {
		if !learningDeviceParam(w, device) || !a.learningRate(w, r, p.UserID, true) {
			return
		}
		if a.learningError(w, a.store.DeleteLearningDevice(r.Context(), p.UserID, p.SessionID, device)) {
			return
		}
		w.WriteHeader(204)
		return
	}
	var v struct {
		PublicKey string `json:"public_key"`
		Name      string `json:"name"`
	}
	if !read(w, r, &v) || !learningDeviceParam(w, device) {
		return
	}
	if !validE2EDevice(device, v.PublicKey) || !validE2EDeviceName(v.Name) {
		writeError(w, 400, "invalid_learning_device")
		return
	}
	if !a.learningRate(w, r, p.UserID, true) {
		return
	}
	if a.learningError(w, a.store.PutLearningDevice(r.Context(), p.UserID, p.SessionID, device, v.PublicKey, v.Name)) {
		return
	}
	w.WriteHeader(204)
}

// learningKey 处理 POST /v1/users/me/learning/key：账号还没有密钥时，建立密钥并写入给调用会话绑定设备的封装。
func (a *Service) learningKey(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		KeyID string `json:"key_id"`
		Wrap  string `json:"wrap"`
	}
	if !read(w, r, &v) {
		return
	}
	if !isLowerHex(v.KeyID, 32) || !validE2EWrap(v.Wrap) {
		writeError(w, 400, "invalid_learning_key")
		return
	}
	if !a.learningRate(w, r, p.UserID, true) {
		return
	}
	if a.learningError(w, a.store.CreateLearningKey(r.Context(), p.UserID, p.SessionID, v.KeyID, v.Wrap)) {
		return
	}
	w.WriteHeader(204)
}

// learningWrap 处理 PUT /v1/users/me/learning/wraps/{device_id}：调用会话绑定的批准者用当前密钥的封装批准一台设备。
func (a *Service) learningWrap(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		KeyID            string `json:"key_id"`
		Wrap             string `json:"wrap"`
		ApproverDeviceID string `json:"approver_device_id"`
	}
	device := r.PathValue("device_id")
	if !read(w, r, &v) || !learningDeviceParam(w, device) {
		return
	}
	if !isLowerHex(v.KeyID, 32) || !validE2EWrap(v.Wrap) || !isLowerHex(v.ApproverDeviceID, 32) {
		writeError(w, 400, "invalid_learning_key")
		return
	}
	if !a.learningRate(w, r, p.UserID, true) {
		return
	}
	if a.learningError(w, a.store.PutLearningWrap(r.Context(), p.UserID, p.SessionID, device, v.KeyID, v.Wrap, v.ApproverDeviceID)) {
		return
	}
	w.WriteHeader(204)
}

// learningRekey 处理 POST /v1/users/me/learning/rekey：换钥，同一事务里删除 revoke 里的设备、全部密文和全部旧封装。
func (a *Service) learningRekey(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		PreviousKeyID string   `json:"previous_key_id"`
		KeyID         string   `json:"key_id"`
		Wrap          string   `json:"wrap"`
		Revoke        []string `json:"revoke"`
	}
	if !read(w, r, &v) {
		return
	}
	valid := isLowerHex(v.PreviousKeyID, 32) && isLowerHex(v.KeyID, 32) && v.KeyID != v.PreviousKeyID && validE2EWrap(v.Wrap) && len(v.Revoke) <= learningDeviceLimit
	for _, id := range v.Revoke {
		valid = valid && isLowerHex(id, 32)
	}
	if !valid {
		writeError(w, 400, "invalid_learning_key")
		return
	}
	if !a.learningRate(w, r, p.UserID, true) {
		return
	}
	if a.learningError(w, a.store.RekeyLearning(r.Context(), p.UserID, p.SessionID, v.PreviousKeyID, v.KeyID, v.Wrap, v.Revoke)) {
		return
	}
	w.WriteHeader(204)
}

// learningBlob 处理 GET、PUT 和 DELETE /v1/users/me/learning/blobs/{device_id}。读取任何会话都可以，写入和删除只允许调用会话绑定的那台设备操作自己的那份。
func (a *Service) learningBlob(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	device := r.PathValue("device_id")
	switch r.Method {
	case "GET":
		if !learningDeviceParam(w, device) || !a.learningRate(w, r, p.UserID, false) {
			return
		}
		release, ok := acquireLearningTransfer(w, r)
		if !ok {
			return
		}
		defer release()
		blob, e := a.store.LearningBlobData(r.Context(), p.UserID, p.SessionID, device)
		if a.learningError(w, e) {
			return
		}
		setPrivateResponseHeaders(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
		w.WriteHeader(200)
		_, _ = w.Write(blob)
	case "PUT":
		a.learningBlobPut(w, r, p, device)
	case "DELETE":
		if !learningDeviceParam(w, device) || !a.learningRate(w, r, p.UserID, true) {
			return
		}
		if a.learningError(w, a.store.DeleteLearningBlob(r.Context(), p.UserID, p.SessionID, device)) {
			return
		}
		w.WriteHeader(204)
	}
}

// learningBlobPut 处理 PUT /v1/users/me/learning/blobs/{device_id}?revision=N：请求体是信封原样字节，最多 8 MiB。
func (a *Service) learningBlobPut(w http.ResponseWriter, r *http.Request, p Principal, device string) {
	values := r.URL.Query()
	revisions := values["revision"]
	if len(values) != 1 || len(revisions) != 1 {
		writeError(w, 400, "invalid_learning_blob")
		return
	}
	// 只接受不带符号和前导零的十进制，保证同一个代次只有一种写法。
	revision, err := strconv.ParseUint(revisions[0], 10, 64)
	if err != nil || revision < 1 || revision > learningRevisionMax || strconv.FormatUint(revision, 10) != revisions[0] {
		writeError(w, 400, "invalid_learning_blob")
		return
	}
	if !learningDeviceParam(w, device) {
		return
	}
	if r.ContentLength > learningBlobMax {
		writeError(w, 413, "learning_too_large")
		return
	}
	if !a.learningRate(w, r, p.UserID, true) {
		return
	}
	release, ok := acquireLearningTransfer(w, r)
	if !ok {
		return
	}
	defer release()
	blob, err := boundedio.ReadAll(http.MaxBytesReader(w, r.Body, learningBlobMax), r.ContentLength, learningBlobMax)
	if httpjson.IsBodyTooLarge(err) || len(blob) > learningBlobMax {
		writeError(w, 413, "learning_too_large")
		return
	}
	if err != nil {
		writeError(w, 400, "invalid_learning_blob")
		return
	}
	key, code := learningBlobFormat(blob, device, revision)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	if a.learningError(w, a.store.PutLearningBlob(r.Context(), p.UserID, p.SessionID, device, int64(revision), key, blob)) {
		return
	}
	w.WriteHeader(204)
}

// acquireLearningTransfer 延长本次请求的连接读写期限，并在路由时限内等一个传输名额；等不到时写 503 server_busy。
func acquireLearningTransfer(w http.ResponseWriter, r *http.Request) (func(), bool) {
	httpdeadline.Extend(w, learningTransferTimeout, learningTransferTimeout+5*time.Second)
	release, ok := acquirePluginSlot(r.Context(), learningTransferSlots)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeError(w, 503, "server_busy")
	}
	return release, ok
}
