package account

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// 打字统计的云端汇总和 QQ 群友的账号绑定。客户端在用户打开「上传打字统计」后按天上传计数（PUT /v1/users/me/typing-statistics），关掉时清空（DELETE）；群友在 QQ 群里 @机器人 发 /bind 拿到一次性绑定码，在已登录的客户端里输入（POST /v1/users/me/qq-links），之后 /stats 能看到自己的打字数据。绑定码由机器人发、在 App 里输入，而不是反过来：群聊是公开的，App 出的码发进群里会被别人抢先拿去，把别人的 QQ 绑到自己的账号上，读到自己的数据；现在这个方向上，看到码的人最多是把别人的 QQ 绑到自己账号上，看到的仍是自己的数据。

const (
	// typingStatisticsRetentionDays 是每日汇总和设备行的保留天数，到期由 Store.Prune 删除；上传时也只接受这个窗口里的日期。
	typingStatisticsRetentionDays = 62
	// typingStatisticsDevices 是每个用户最多保留的设备数，新设备挤掉最久没上传的那台。
	typingStatisticsDevices = 8
	// typingStatisticsUploadDays 是一次上传最多的天数。
	typingStatisticsUploadDays = 31
	typingStatisticsBodyBytes  = 64 << 10
	// typingStatisticsUploadsPerHour 是每个用户每小时能上传的次数（auth_rates，所有副本共享）。
	typingStatisticsUploadsPerHour = 60
	// typingStatisticsMaxCount 是任何一个计数的上限，一天打不出一千万字。
	typingStatisticsMaxCount = 10_000_000
	// typingStatisticsMaxKeys 是字符类别和输入来源各自最多的种类数，typingStatisticsMaxRanks 是候选位次最多的个数。
	typingStatisticsMaxKeys  = 32
	typingStatisticsMaxRanks = 16
	// QQLinkCodeTTL 是绑定码的有效期。
	QQLinkCodeTTL = 10 * time.Minute
	// qqLinkAttemptsPerHour 是每个用户每小时能提交绑定码的次数，失败的也算：码只有 8 位数字，不限次数就能猜。
	qqLinkAttemptsPerHour = 10
	// qqLinksPerUser 是一个账号最多绑定的群友数。同一个人在不同的群里是不同的 member_openid。
	qqLinksPerUser = 10
)

var (
	typingStatisticsDeviceID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	typingStatisticsPlatforms = []string{"", "windows", "macos", "linux", "android", "ios", "harmony"}
	qqLinkCodePattern         = regexp.MustCompile(`^[0-9]{8}$`)
	qqLinkID                  = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
)

// ErrQQLinkCodeNotFound 表示绑定码不存在、已过期或已经用过。ErrQQLinkLimit 表示账号绑定的群友已经到了 qqLinksPerUser 个。
var (
	ErrQQLinkCodeNotFound = errors.New("qq_link_code_not_found")
	ErrQQLinkLimit        = errors.New("qq_link_limit")
)

// TypingDay 是一台设备一天的计数。Day 是用户本地的日期；Hours 有就是 24 个，第 0 个是零点到一点；Categories 是各字符类别（han、latin、number、punctuation……）的字数，Sources 是各输入来源（quanpin、wubi、voice……）的字数。
type TypingDay struct {
	Day           string           `json:"day"`
	Characters    int64            `json:"characters"`
	ActiveSeconds int64            `json:"active_seconds"`
	Hours         []int64          `json:"hours,omitempty"`
	Categories    map[string]int64 `json:"categories,omitempty"`
	Sources       map[string]int64 `json:"sources,omitempty"`
}

// TypingSelections 是候选位次的计数：Ranks[0] 是选第一个候选的次数，Beyond 是翻页以后才选中的次数。
type TypingSelections struct {
	Ranks  []int64 `json:"ranks"`
	Beyond int64   `json:"beyond"`
}

// typingUpload 是 PUT /v1/users/me/typing-statistics 的请求体。Selections 是这台设备全部历史的累计，不传时保留上次的。
type typingUpload struct {
	Device     string            `json:"device"`
	Platform   string            `json:"platform"`
	Days       []TypingDay       `json:"days"`
	Selections *TypingSelections `json:"selections"`
}

// validTypingCount 判断一个计数在 0 到 typingStatisticsMaxCount 之间。
func validTypingCount(n int64) bool { return n >= 0 && n <= typingStatisticsMaxCount }

// validTypingStatisticsKey 校验统计键：1～32 个 ASCII 字符，首字符为字母，其余为字母、数字或下划线。
func validTypingStatisticsKey(key string) bool {
	if len(key) == 0 || len(key) > 32 {
		return false
	}
	for i := range len(key) {
		c := key[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			continue
		}
		if i == 0 || !(c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// validTypingCounts 校验字符类别或输入来源的计数表。
func validTypingCounts(m map[string]int64) bool {
	if len(m) > typingStatisticsMaxKeys {
		return false
	}
	for k, n := range m {
		if !validTypingStatisticsKey(k) || !validTypingCount(n) {
			return false
		}
	}
	return true
}

// validTypingUpload 校验上传：设备标识、平台、1 到 typingStatisticsUploadDays 个不重复的日期，日期在 today 往前 typingStatisticsRetentionDays-1 天到往后 1 天之间（时区差），活跃时间不超过一天，各计数在范围内。
func validTypingUpload(v typingUpload, today time.Time) bool {
	if !typingStatisticsDeviceID.MatchString(v.Device) || !slices.Contains(typingStatisticsPlatforms, v.Platform) || len(v.Days) == 0 || len(v.Days) > typingStatisticsUploadDays {
		return false
	}
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	earliest, latest := today.AddDate(0, 0, -(typingStatisticsRetentionDays-1)), today.AddDate(0, 0, 1)
	seen := map[string]bool{}
	for _, d := range v.Days {
		day, err := time.Parse(time.DateOnly, d.Day)
		if err != nil || seen[d.Day] || day.Before(earliest) || day.After(latest) {
			return false
		}
		seen[d.Day] = true
		if !validTypingCount(d.Characters) || d.ActiveSeconds < 0 || d.ActiveSeconds > 24*3600 || !validTypingCounts(d.Categories) || !validTypingCounts(d.Sources) {
			return false
		}
		if d.Hours != nil && len(d.Hours) != 24 {
			return false
		}
		for _, n := range d.Hours {
			if !validTypingCount(n) {
				return false
			}
		}
	}
	if s := v.Selections; s != nil {
		if s.Ranks == nil || len(s.Ranks) > typingStatisticsMaxRanks || !validTypingCount(s.Beyond) {
			return false
		}
		for _, n := range s.Ranks {
			// 位次是全部历史的累计，比一天的上限宽一些。
			if n < 0 || n > 100*typingStatisticsMaxCount {
				return false
			}
		}
	}
	return true
}

// PutTypingStatistics 写入一台设备的上传：先登记设备（新设备超过 typingStatisticsDevices 台时删掉最久没上传的，它的每日汇总随外键一起删除），再逐天整行替换。行锁取自 userDataTransaction，被封禁的账号写不进来。
func (s *Store) PutTypingStatistics(ctx context.Context, user string, v typingUpload) error {
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM typing_statistics_devices WHERE user_id=$1 AND device IN (
 SELECT device FROM typing_statistics_devices WHERE user_id=$1 AND device<>$2 ORDER BY updated_at DESC,device OFFSET $3)
 AND NOT EXISTS (SELECT 1 FROM typing_statistics_devices WHERE user_id=$1 AND device=$2)`, user, v.Device, typingStatisticsDevices-1); err != nil {
		return err
	}
	var ranks []int64
	var beyond int64
	if v.Selections != nil {
		ranks, beyond = v.Selections.Ranks, v.Selections.Beyond
	}
	if _, err = tx.Exec(ctx, `INSERT INTO typing_statistics_devices(user_id,device,platform,ranks,beyond) VALUES($1,$2,$3,COALESCE($4::bigint[],'{}'),$5)
 ON CONFLICT(user_id,device) DO UPDATE SET platform=excluded.platform,updated_at=now(),
 ranks=CASE WHEN $4::bigint[] IS NULL THEN typing_statistics_devices.ranks ELSE excluded.ranks END,
 beyond=CASE WHEN $4::bigint[] IS NULL THEN typing_statistics_devices.beyond ELSE excluded.beyond END`, user, v.Device, v.Platform, ranks, beyond); err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for _, d := range v.Days {
		categories, _ := json.Marshal(cmpOrEmpty(d.Categories))
		sources, _ := json.Marshal(cmpOrEmpty(d.Sources))
		batch.Queue(`INSERT INTO typing_statistics_days(user_id,device,day,characters,active_seconds,hours,categories,sources) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT(user_id,device,day) DO UPDATE SET characters=excluded.characters,active_seconds=excluded.active_seconds,hours=excluded.hours,categories=excluded.categories,sources=excluded.sources,updated_at=now()`,
			user, v.Device, d.Day, d.Characters, d.ActiveSeconds, d.Hours, categories, sources)
	}
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// cmpOrEmpty 把 nil 换成空表，写进数据库的是 {} 而不是 null。
func cmpOrEmpty(m map[string]int64) map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return m
}

// typingStatistics 处理 PUT 和 DELETE /v1/users/me/typing-statistics。
func (a *Service) typingStatistics(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if r.Method == "DELETE" {
		tx, err := a.store.userDataTransaction(r.Context(), p.UserID)
		if err != nil {
			a.error(w, err)
			return
		}
		defer tx.Rollback(r.Context())
		if _, err = tx.Exec(r.Context(), `DELETE FROM typing_statistics_devices WHERE user_id=$1`, p.UserID); err != nil {
			a.error(w, err)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			a.error(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	var v typingUpload
	if !readSized(w, r, &v, typingStatisticsBodyBytes) {
		return
	}
	if !validTypingUpload(v, time.Now().UTC()) {
		writeError(w, 400, "invalid_typing_statistics")
		return
	}
	if err := a.store.Rate(r.Context(), "typing-statistics:"+p.UserID, typingStatisticsUploadsPerHour, time.Hour); err != nil {
		a.error(w, err)
		return
	}
	if err := a.store.PutTypingStatistics(r.Context(), p.UserID, v); err != nil {
		a.error(w, err)
		return
	}
	w.WriteHeader(204)
}

// QQLink 是一条绑定，返回给用户本人；不含群和成员的标识，服务端也只存摘要。
type QQLink struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// RedeemQQLinkCode 用绑定码把对应的群友绑到 user：码用过即删；这个群友已经绑了别的账号时改绑到 user。账号已经绑了 qqLinksPerUser 个其他群友时返回 ErrQQLinkLimit，码不作废。
func (s *Store) RedeemQQLinkCode(ctx context.Context, user, code string) (QQLink, error) {
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		return QQLink{}, err
	}
	defer tx.Rollback(ctx)
	var subject string
	// 用户行锁和绑定码行锁都可能等待到码已过期，取得码的行锁后再用实时时钟校验。
	err = tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT code_hash,expires_at FROM qq_link_codes WHERE code_hash=$1 FOR UPDATE
)
DELETE FROM qq_link_codes c USING locked x
 WHERE c.code_hash=x.code_hash AND x.expires_at>clock_timestamp()
 RETURNING c.subject`, hash(code)).Scan(&subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return QQLink{}, ErrQQLinkCodeNotFound
	}
	if err != nil {
		return QQLink{}, err
	}
	var others int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM qq_account_links WHERE user_id=$1 AND subject<>$2`, user, subject).Scan(&others); err != nil {
		return QQLink{}, err
	}
	if others >= qqLinksPerUser {
		return QQLink{}, ErrQQLinkLimit
	}
	var link QQLink
	var id int64
	if err = tx.QueryRow(ctx, `INSERT INTO qq_account_links(subject,user_id) VALUES($1,$2)
 ON CONFLICT(subject) DO UPDATE SET user_id=excluded.user_id,created_at=now() RETURNING id,created_at`, subject, user).Scan(&id, &link.CreatedAt); err != nil {
		return QQLink{}, err
	}
	link.ID = strconv.FormatInt(id, 10)
	return link, tx.Commit(ctx)
}

// qqLinks 处理 GET 和 POST /v1/users/me/qq-links：列出本人的绑定，或者用绑定码新增一条。
func (a *Service) qqLinks(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if r.Method == "GET" {
		tx, err := a.store.userReadTransaction(r.Context(), p.UserID)
		if err != nil {
			a.error(w, err)
			return
		}
		defer tx.Rollback(r.Context())
		rows, err := tx.Query(r.Context(), `SELECT id::text,created_at FROM qq_account_links WHERE user_id=$1 ORDER BY created_at DESC,id DESC`, p.UserID)
		if err != nil {
			a.error(w, err)
			return
		}
		links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (QQLink, error) {
			var v QQLink
			return v, row.Scan(&v.ID, &v.CreatedAt)
		})
		if err != nil {
			a.error(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			a.error(w, err)
			return
		}
		write(w, 200, map[string]any{"links": links})
		return
	}
	var v struct {
		Code string `json:"code"`
	}
	if !readSized(w, r, &v, 1024) {
		return
	}
	if !qqLinkCodePattern.MatchString(v.Code) {
		writeError(w, 400, "invalid_qq_link_code")
		return
	}
	if err := a.store.Rate(r.Context(), "qq-link:"+p.UserID, qqLinkAttemptsPerHour, time.Hour); err != nil {
		a.error(w, err)
		return
	}
	link, err := a.store.RedeemQQLinkCode(r.Context(), p.UserID, v.Code)
	switch {
	case errors.Is(err, ErrQQLinkCodeNotFound):
		writeError(w, 404, "qq_link_code_not_found")
	case errors.Is(err, ErrQQLinkLimit):
		writeError(w, 409, "qq_link_limit")
	case err != nil:
		a.error(w, err)
	default:
		write(w, 201, link)
	}
}

// qqLinkDelete 处理 DELETE /v1/users/me/qq-links/{id}：解除本人的一条绑定。
func (a *Service) qqLinkDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	linkID, err := strconv.ParseInt(id, 10, 64)
	if !qqLinkID.MatchString(id) || err != nil {
		writeError(w, 404, "qq_link_not_found")
		return
	}
	tx, err := a.store.userDataTransaction(r.Context(), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `DELETE FROM qq_account_links WHERE id=$1::bigint AND user_id=$2`, linkID, p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "qq_link_not_found")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	w.WriteHeader(204)
}

// CreateQQLinkCode 给群友（subject 是调用方拼的「群/成员」，这里只存摘要）生成一个 8 位数字的绑定码，QQLinkCodeTTL 内有效；这个群友之前要过的码同时作废。
func (a *Service) CreateQQLinkCode(ctx context.Context, subject string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if err != nil {
		return "", err
	}
	code := fmt.Sprintf("%08d", n.Int64())
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// 先按群友摘要串行化删除旧码和插入新码；没有现存行时，普通行锁不能阻止两个副本同时留下两条有效码。
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "qq-link-code:"+hash(subject)); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM qq_link_codes WHERE subject=$1`, hash(subject)); err != nil {
		return "", err
	}
	// 和现存的码撞上（一亿分之几）时什么也不写，返回错误让群友再发一次，不覆盖别人的码。
	tag, err := tx.Exec(ctx, `INSERT INTO qq_link_codes(code_hash,subject,expires_at) VALUES($1,$2,clock_timestamp()+$3::interval) ON CONFLICT(code_hash) DO NOTHING`, hash(code), hash(subject), QQLinkCodeTTL.String())
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", errors.New("qq link code collision")
	}
	return code, tx.Commit(ctx)
}

// UnlinkQQ 解除群友的绑定并作废未使用的绑定码，返回原来有没有绑定。
func (a *Service) UnlinkQQ(ctx context.Context, subject string) (bool, error) {
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// 和发码共用群友锁；锁后用新语句读取，才能清除等待期间刚生成的码。
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "qq-link-code:"+hash(subject)); err != nil {
		return false, err
	}
	// 先删除绑定码，等待正在兑换的事务结束，再用独立语句的新快照删除它刚写入的绑定。
	if _, err := tx.Exec(ctx, `DELETE FROM qq_link_codes WHERE subject=$1`, hash(subject)); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM qq_account_links WHERE subject=$1`, hash(subject))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// QQTypingReport 是机器人 /stats 用的数据：群友绑定的账号、这个账号从 since 起每天的计数（各设备相加，按日期从早到晚），以及各设备候选位次的合计。没有绑定或账号被封禁时 Linked 为 false；Devices 是上传过的设备数，为 0 表示还没打开上传。
type QQTypingReport struct {
	Linked      bool
	DisplayName string
	Devices     int
	Days        []TypingDay
	Selections  TypingSelections
}

// QQTypingReport 读出群友 subject 的打字数据，见 QQTypingReport 类型。
func (a *Service) QQTypingReport(ctx context.Context, subject string, since time.Time) (QQTypingReport, error) {
	var report QQTypingReport
	// 绑定、设备累计和每日数据共用快照，避免并发上传或注销时拼出从未存在过的统计。
	tx, err := a.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	var user string
	err = tx.QueryRow(ctx, `SELECT u.id,COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6)))
 FROM qq_account_links l JOIN auth_users u ON u.id=l.user_id AND u.banned_at IS NULL WHERE l.subject=$1`, hash(subject)).Scan(&user, &report.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	report.Linked = true
	rows, err := tx.Query(ctx, `SELECT ranks,beyond FROM typing_statistics_devices WHERE user_id=$1`, user)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var ranks []int64
		var beyond int64
		if err = rows.Scan(&ranks, &beyond); err != nil {
			rows.Close()
			return report, err
		}
		report.Devices++
		report.Selections.Beyond += beyond
		for i, n := range ranks {
			if i == len(report.Selections.Ranks) {
				report.Selections.Ranks = append(report.Selections.Ranks, 0)
			}
			report.Selections.Ranks[i] += n
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return report, err
	}
	rows, err = tx.Query(ctx, `SELECT to_char(day,'YYYY-MM-DD'),characters,active_seconds,hours,categories,sources FROM typing_statistics_days WHERE user_id=$1 AND day>=$2 ORDER BY day`, user, since.Format(time.DateOnly))
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var d TypingDay
		var categories, sources []byte
		if err = rows.Scan(&d.Day, &d.Characters, &d.ActiveSeconds, &d.Hours, &categories, &sources); err != nil {
			return report, err
		}
		if err = json.Unmarshal(categories, &d.Categories); err != nil {
			return report, err
		}
		if err = json.Unmarshal(sources, &d.Sources); err != nil {
			return report, err
		}
		if n := len(report.Days); n > 0 && report.Days[n-1].Day == d.Day {
			report.Days[n-1] = addTypingDay(report.Days[n-1], d)
		} else {
			report.Days = append(report.Days, d)
		}
	}
	return report, rows.Err()
}

// addTypingDay 把同一天两台设备的计数相加。只有一台设备有每小时的数时用它的，两台都没有时仍然没有。
func addTypingDay(a, b TypingDay) TypingDay {
	out := TypingDay{Day: a.Day, Characters: a.Characters + b.Characters, ActiveSeconds: a.ActiveSeconds + b.ActiveSeconds, Categories: map[string]int64{}, Sources: map[string]int64{}}
	if a.Hours != nil || b.Hours != nil {
		out.Hours = make([]int64, 24)
		for _, hours := range [][]int64{a.Hours, b.Hours} {
			for i, n := range hours {
				out.Hours[i] += n
			}
		}
	}
	for _, m := range []map[string]int64{a.Categories, b.Categories} {
		for k, n := range m {
			out.Categories[k] += n
		}
	}
	for _, m := range []map[string]int64{a.Sources, b.Sources} {
		for k, n := range m {
			out.Sources[k] += n
		}
	}
	return out
}
