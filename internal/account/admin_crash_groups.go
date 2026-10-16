package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Crash page (unit U9): the crash event list, crash groups, their signatures and status.

// crashesList serves GET /api/crashes.
var crashesList = adminList{
	query: `SELECT id,platform,version,message,stack,resolved,created_at FROM admin_events WHERE kind='crash'`,
	filters: []listFilter{
		{param: "platform", field: "platform", max: 32},
		{param: "version", field: "version", max: 64},
		{param: "status", field: "resolved", max: 8, values: map[string]string{"open": "false", "resolved": "true"}},
	},
}

// setCrashResolved marks the crash event id resolved or open.
func setCrashResolved(resolved bool) adminActionFunc {
	return func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		if err := requireActionID(v); err != nil {
			return actionResult{}, err
		}
		tag, err := tx.Exec(ctx, `UPDATE admin_events SET resolved=$2 WHERE id=$1 AND kind='crash'`, v.ID, resolved)
		if err != nil {
			return actionResult{}, err
		}
		if tag.RowsAffected() == 0 {
			return actionResult{}, actionFail(404, "not_found")
		}
		return actionResult{Affected: tag.RowsAffected()}, nil
	}
}

var (
	actionResolveCrash = setCrashResolved(true)
	actionReopenCrash  = setCrashResolved(false)
)

// crashSignaturePattern is the shape of a crash group signature: the first 16 hex digits of a sha256.
var crashSignaturePattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidCrashSignature reports whether s can name a crash group.
func ValidCrashSignature(s string) bool { return crashSignaturePattern.MatchString(s) }

// crashGroupStatuses are the states a crash group can be in.
var crashGroupStatuses = map[string]bool{"open": true, "known": true, "fixed": true}

// actionCrashGroupStatus 设置崩溃分组状态，在同一条语句内锁定并返回旧状态，供控制台撤销。
func actionCrashGroupStatus(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if !ValidCrashSignature(v.ID) {
		return actionResult{}, actionFail(400, "invalid_id")
	}
	var status string
	if json.Unmarshal(v.Value, &status) != nil || !crashGroupStatuses[status] {
		return actionResult{}, actionFail(400, "invalid_value")
	}
	var previous string
	err := tx.QueryRow(ctx, `UPDATE admin_crash_groups g SET status=$2
FROM (SELECT signature,status FROM admin_crash_groups WHERE signature=$1 FOR UPDATE) locked
WHERE g.signature=locked.signature RETURNING locked.status`, v.ID, status).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return actionResult{}, actionFail(404, "not_found")
	}
	if err != nil {
		return actionResult{}, err
	}
	detail := map[string]any{"from": previous, "to": status}
	if v.Reason != "" {
		detail["reason"] = v.Reason
	}
	return actionResult{Affected: 1, Detail: detail, Extra: map[string]any{"previous": previous}}, nil
}

// crashGroupRow is one crash group with its counts, as the crash page shows it.
type crashGroupRow struct {
	Signature string    `json:"signature"`
	Platform  string    `json:"platform"`
	Version   string    `json:"version"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	IssueURL  *string   `json:"issue_url"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Count7d and CountPrev7d are the crashes of the last 7 days and of the 7 days before; the console derives the trend from them.
	Count7d     int64 `json:"count_7d"`
	CountPrev7d int64 `json:"count_prev_7d"`
	// Devices7d is the number of distinct installs that crashed in the last 7 days, or null when none of those crashes carried an install id.
	Devices7d *int64 `json:"devices_7d"`
	// New marks a group first seen within the last 7 days.
	New bool `json:"new"`
}

// crashGroupColumns selects a crashGroupRow from admin_crash_groups g joined with the per-signature counts c of crashGroupCounts.
const crashGroupColumns = `g.signature,g.platform,g.version,g.title,g.status,g.issue_url,g.first_seen,g.last_seen,COALESCE(c.c7,0),COALESCE(c.p7,0),CASE WHEN COALESCE(c.reported,0)=0 THEN NULL ELSE c.devices END,g.first_seen>=now()-interval '7 days'`

// crashGroupCountsBase 是最近 14 天崩溃统计的聚合和条件部分；$1 非空时限定单个签名，调用处追加分组。
const crashGroupCountsBase = `SELECT signature,
 count(*) FILTER (WHERE created_at>=now()-interval '7 days') AS c7,
 count(*) FILTER (WHERE created_at<now()-interval '7 days') AS p7,
 count(install_id) FILTER (WHERE created_at>=now()-interval '7 days') AS reported,
 count(DISTINCT install_id) FILTER (WHERE created_at>=now()-interval '7 days') AS devices
 FROM admin_events WHERE kind='crash' AND signature IS NOT NULL AND created_at>=now()-interval '14 days' AND ($1='' OR signature=$1)`

// crashGroupCounts 为所有平台或单个签名统计，供详情和崩溃增长检测共用。
const crashGroupCounts = crashGroupCountsBase + ` GROUP BY signature`

func scanCrashGroup(row pgx.Row, extraDest ...any) (crashGroupRow, error) {
	var g crashGroupRow
	dest := []any{&g.Signature, &g.Platform, &g.Version, &g.Title, &g.Status, &g.IssueURL, &g.FirstSeen, &g.LastSeen, &g.Count7d, &g.CountPrev7d, &g.Devices7d, &g.New}
	err := row.Scan(append(dest, extraDest...)...)
	return g, err
}

// crashGroupsLimit caps the groups one page load returns; the busiest and most recent come first.
const crashGroupsLimit = 500

// crashSummary is the tile row of the crash page. Installs and sessions come from the optional active and session telemetry, so each is null rather than zero while clients do not report it. CrashFreeRate is session/(session+session_crash) over the last 7 days, the same formula and rounding as the overview's crash_free_rate. The summary query names every event kind so the planner can use admin_events_kind_time instead of scanning the whole table.
type crashSummary struct {
	Groups        int64    `json:"groups"`
	Crashes7d     int64    `json:"crashes_7d"`
	Devices7d     *int64   `json:"devices_7d"`
	InstallsToday *int64   `json:"installs_today"`
	CrashFreeRate *float64 `json:"crash_free_rate"`
}

type crashPlatformCount struct {
	Platform string `json:"platform"`
	Count    int64  `json:"count"`
}

// adminCrashGroups serves GET /api/crash-groups?platform=.
func (a *Service) adminCrashGroups(w http.ResponseWriter, r *http.Request, _ string) {
	query := r.URL.Query()
	for key := range query {
		if key != "platform" {
			writeError(w, 400, "invalid_filter")
			return
		}
	}
	platform := query.Get("platform")
	if len(query["platform"]) > 1 || !resourceText(platform, 0, 32, false) {
		writeError(w, 400, "invalid_filter")
		return
	}
	// Groups store the canonical platform; the tiles map each event's reported platform the same way, so an alias such as darwin counts under macos.
	platform = crashPlatform(platform)
	ctx := r.Context()
	// One snapshot for the rows, the chip counts and the tiles, so they agree with each other.
	tx, err := a.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT platform,count(*) FROM admin_crash_groups GROUP BY platform ORDER BY platform`)
	if err != nil {
		a.error(w, err)
		return
	}
	platforms, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (crashPlatformCount, error) {
		var p crashPlatformCount
		return p, row.Scan(&p.Platform, &p.Count)
	})
	if err != nil {
		a.error(w, err)
		return
	}
	var totalGroups, selectedGroups int64
	for _, p := range platforms {
		totalGroups += p.Count
		if p.Platform == platform {
			selectedGroups = p.Count
		}
	}
	countsQuery := crashGroupCounts
	if platform != "" && selectedGroups <= totalGroups/2 {
		// 复用平台计数，只为至多半数分组的平台提前限定签名，避免多数平台的额外关联开销。
		// 按分组自身的平台选签名，不能用可能与分组不同的事件平台代替。
		countsQuery = crashGroupCountsBase + ` AND signature IN (SELECT signature FROM admin_crash_groups WHERE platform=$2) GROUP BY signature`
	}
	var items []crashGroupRow
	batch := &pgx.Batch{}
	batch.Queue(`WITH c AS (`+countsQuery+`)
SELECT `+crashGroupColumns+` FROM admin_crash_groups g LEFT JOIN c USING(signature)
WHERE ($2='' OR g.platform=$2)
ORDER BY COALESCE(c.c7,0) DESC,g.last_seen DESC,g.signature LIMIT $3`, "", platform, crashGroupsLimit+1).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		var err error
		items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (crashGroupRow, error) { return scanCrashGroup(row) })
		return err
	})
	var summary crashSummary
	batch.Queue(`SELECT
 (SELECT count(*) FROM admin_crash_groups WHERE $1='' OR platform=$1),
 count(*) FILTER (WHERE kind='crash'),
 CASE WHEN count(install_id) FILTER (WHERE kind='crash')=0 THEN NULL ELSE count(DISTINCT install_id) FILTER (WHERE kind='crash') END,
 CASE WHEN count(install_id)=0 THEN NULL ELSE count(DISTINCT install_id) FILTER (WHERE created_at>=date_trunc('day',now(),'UTC')) END,
 round(count(*) FILTER (WHERE kind='session')::numeric/NULLIF(count(*) FILTER (WHERE kind IN ('session','session_crash')),0),4)::float8
	FROM admin_events WHERE kind IN ('download','crash','active','session','session_crash') AND created_at>=now()-interval '7 days' AND ($1='' OR `+overviewPlatform("platform")+`=$1)`, platform).QueryRow(func(row pgx.Row) error {
		return row.Scan(&summary.Groups, &summary.Crashes7d, &summary.Devices7d, &summary.InstallsToday, &summary.CrashFreeRate)
	})
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		a.error(w, err)
		return
	}
	hasMore := len(items) > crashGroupsLimit
	if hasMore {
		items = items[:crashGroupsLimit]
	}
	write(w, 200, map[string]any{"items": items, "has_more": hasMore, "platforms": platforms, "summary": summary})
}

// crashSample is one stored crash of a group, for the stack view.
type crashSample struct {
	ID        string    `json:"id"`
	Platform  string    `json:"platform"`
	Version   string    `json:"version"`
	Message   string    `json:"message"`
	Stack     string    `json:"stack"`
	Resolved  bool      `json:"resolved"`
	CreatedAt time.Time `json:"created_at"`
}

// crashGroupSamples is how many of the latest crashes the group view shows.
const crashGroupSamples = 5

// crashGroup 在同一条查询的快照中读取分组、统计和最新样本；分组不存在时返回 pgx.ErrNoRows。
func (a *Service) crashGroup(ctx context.Context, signature string, samples int) (crashGroupRow, []crashSample, error) {
	return readCrashGroup(ctx, a.store.pool, signature, samples)
}

type crashGroupQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readCrashGroup(ctx context.Context, q crashGroupQuerier, signature string, samples int) (crashGroupRow, []crashSample, error) {
	rows, err := q.Query(ctx, `WITH c AS (`+crashGroupCounts+`)
SELECT `+crashGroupColumns+`,
 s.id IS NOT NULL,COALESCE(s.id,''),COALESCE(s.platform,''),COALESCE(s.version,''),
 COALESCE(s.message,''),COALESCE(s.stack,''),COALESCE(s.resolved,false),COALESCE(s.created_at,g.last_seen)
FROM admin_crash_groups g LEFT JOIN c USING(signature)
LEFT JOIN LATERAL (
 SELECT id,platform,version,message,stack,resolved,created_at FROM admin_events
 WHERE kind='crash' AND signature=$1 ORDER BY created_at DESC,id DESC LIMIT $2
) s ON true
WHERE g.signature=$1 ORDER BY s.created_at DESC,s.id DESC`, signature, samples)
	if err != nil {
		return crashGroupRow{}, nil, err
	}
	defer rows.Close()
	var group crashGroupRow
	items := []crashSample{}
	found := false
	// 左连接保留无样本的分组；只有实际事件行才加入样本数组。
	for rows.Next() {
		var sample crashSample
		var present bool
		group, err = scanCrashGroup(rows, &present, &sample.ID, &sample.Platform, &sample.Version, &sample.Message, &sample.Stack, &sample.Resolved, &sample.CreatedAt)
		if err != nil {
			return crashGroupRow{}, nil, err
		}
		found = true
		if present {
			items = append(items, sample)
		}
	}
	if err := rows.Err(); err != nil {
		return crashGroupRow{}, nil, err
	}
	if !found {
		return crashGroupRow{}, nil, pgx.ErrNoRows
	}
	return group, items, nil
}

// adminCrashGroup serves GET /api/crash-groups/{signature}.
func (a *Service) adminCrashGroup(w http.ResponseWriter, r *http.Request, signature string) {
	if !ValidCrashSignature(signature) {
		writeError(w, 400, "invalid_id")
		return
	}
	group, samples, err := a.crashGroup(r.Context(), signature, crashGroupSamples)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"group": group, "samples": samples})
}

// CrashIssueSource is what a GitHub issue for a crash group is written from.
type CrashIssueSource struct {
	Signature   string
	Platform    string
	Version     string
	Title       string
	Status      string
	IssueURL    string
	FirstSeen   time.Time
	LastSeen    time.Time
	Count7d     int64
	CountPrev7d int64
	// Devices7d is -1 when the crashes carry no install id.
	Devices7d int64
	// Sample is the latest crash, if any is still stored.
	Sample *CrashIssueSample
}

// CrashIssueSample is the crash quoted in an issue.
type CrashIssueSample struct {
	Version   string
	Message   string
	Stack     string
	CreatedAt time.Time
}

// ErrCrashGroupNotFound is returned for a signature without a crash group.
var ErrCrashGroupNotFound = errors.New("crash group not found")

// CrashGroupForIssue reads the crash group signature for a GitHub issue; a missing group is ErrCrashGroupNotFound.
func (a *Service) CrashGroupForIssue(ctx context.Context, signature string) (CrashIssueSource, error) {
	return crashGroupForIssue(ctx, a.store.pool, signature)
}

func crashGroupForIssue(ctx context.Context, q crashGroupQuerier, signature string) (CrashIssueSource, error) {
	group, samples, err := readCrashGroup(ctx, q, signature, 1)
	if errors.Is(err, pgx.ErrNoRows) {
		return CrashIssueSource{}, ErrCrashGroupNotFound
	}
	if err != nil {
		return CrashIssueSource{}, err
	}
	source := CrashIssueSource{Signature: group.Signature, Platform: group.Platform, Version: group.Version, Title: group.Title, Status: group.Status, FirstSeen: group.FirstSeen, LastSeen: group.LastSeen, Count7d: group.Count7d, CountPrev7d: group.CountPrev7d, Devices7d: -1}
	if group.IssueURL != nil {
		source.IssueURL = *group.IssueURL
	}
	if group.Devices7d != nil {
		source.Devices7d = *group.Devices7d
	}
	if len(samples) > 0 {
		s := samples[0]
		source.Sample = &CrashIssueSample{Version: s.Version, Message: s.Message, Stack: s.Stack, CreatedAt: s.CreatedAt}
	}
	return source, nil
}

// ErrCrashIssueBusy is returned while another request is opening an issue for the same crash group.
var ErrCrashIssueBusy = errors.New("crash group issue in progress")

// crashIssueLockSpace is the first key of the per-group advisory lock that serializes issue creation; the second is hashtext(signature).
const crashIssueLockSpace int32 = 0x6d736369

// CrashIssueLock 持有分组的会话锁和连接，供同一个请求顺序重读分组、登记 Issue，避免持锁时再向连接池申请连接。
type CrashIssueLock struct {
	conn      *pgxpool.Conn
	signature string
}

// LockCrashGroupIssue 串行化同一崩溃分组在多个请求和副本上的 Issue 创建；其他请求持锁时返回 ErrCrashIssueBusy。调用方应在锁内重读分组，并在完成后调用 Release。
func (a *Service) LockCrashGroupIssue(ctx context.Context, signature string) (*CrashIssueLock, error) {
	conn, err := a.store.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1,hashtext($2))`, crashIssueLockSpace, signature).Scan(&locked); err != nil || !locked {
		conn.Release()
		if err == nil {
			err = ErrCrashIssueBusy
		}
		return nil, err
	}
	return &CrashIssueLock{conn: conn, signature: signature}, nil
}

// Source 使用持锁连接重读分组，以锁内最新状态判断是否已经创建 Issue。
func (l *CrashIssueLock) Source(ctx context.Context) (CrashIssueSource, error) {
	return crashGroupForIssue(ctx, l.conn, l.signature)
}

// Record 使用持锁连接原子写入 Issue 链接、状态和审计，避免 GitHub 创建成功后因等待池中另一连接而登记失败。
func (l *CrashIssueLock) Record(ctx context.Context, repo string, number int64, url string) error {
	return recordCrashGroupIssue(ctx, l.conn, l.signature, repo, number, url)
}

// Release 在独立上下文中解锁；失败时关闭连接，防止把仍持锁的会话交还连接池。
func (l *CrashIssueLock) Release() {
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := l.conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1,hashtext($2))`, crashIssueLockSpace, l.signature); err != nil {
		_ = l.conn.Conn().Close(unlockCtx)
	}
	l.conn.Release()
}

// SetCrashGroupIssue 用一条原子语句登记崩溃分组的 GitHub issue、将分组标记为 `known` 并记录审计。只在 issue 创建成功后调用。
func (a *Service) SetCrashGroupIssue(ctx context.Context, signature, repo string, number int64, url string) error {
	return recordCrashGroupIssue(ctx, a.store.pool, signature, repo, number, url)
}

func recordCrashGroupIssue(ctx context.Context, q queryRower, signature, repo string, number int64, url string) error {
	detail, err := auditDetail(map[string]any{"repo": repo, "number": number, "issue_url": url, "to": "known"})
	if err != nil {
		return err
	}
	var previous string
	err = q.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT status FROM admin_crash_groups WHERE signature=$1 FOR UPDATE
), updated AS (
 UPDATE admin_crash_groups g SET status='known',issue_url=$2
 FROM locked l WHERE g.signature=$1 RETURNING 1
), audited AS (
 INSERT INTO admin_audit(action,target,actor,detail)
 SELECT 'crash_group_issue',$1,$3,jsonb_set($4::jsonb,'{from}',to_jsonb(l.status))
 FROM locked l CROSS JOIN updated
 RETURNING 1
)
SELECT status FROM locked`, signature, url, adminActor(ctx), detail).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCrashGroupNotFound
	}
	return err
}

// Crash spikes: a new group (first seen within 7 days), or a group whose last 7 days exceed the 7 days before by more than 20% with at least crashSpikeMinimum crashes so a jump from one report to two does not alert anyone, notifies the console at most once per crashSpikeQuiet. This matches the console's 崩溃告警 preference: 新增崩溃分组或崩溃率上升超过 20%.
const (
	crashSpikeRise    = 1.2
	crashSpikeMinimum = 5
	crashSpikeQuiet   = "7 days"
)

// crashSpikeLock serializes spike checks across replicas, so concurrent jobs cannot both notify the same spike.
const crashSpikeLock int64 = 0x6d73696d65637273

// NotifyCrashSpikes records a crash_spike notification, in one transaction, for every crash group that rose more than 20% week over week and was not notified within the last 7 days, and returns how many it found.
func (a *Service) NotifyCrashSpikes(ctx context.Context) (int, error) {
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	spikes, err := lockCrashSpikes(ctx, tx)
	if err != nil {
		return 0, err
	}
	if len(spikes) == 0 {
		if err = tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}
	kinds := make([]string, len(spikes))
	titles := make([]string, len(spikes))
	pages := make([]string, len(spikes))
	targets := make([]string, len(spikes))
	for i, spike := range spikes {
		n, normalizeErr := normalizeNotification(spike.notification())
		if normalizeErr != nil {
			return 0, normalizeErr
		}
		kinds[i], titles[i], pages[i], targets[i] = n.Kind, n.Title, n.TargetPage, n.TargetID
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_notifications(kind,title,target_page,target_id,created_at)
SELECT kind,title,target_page,target_id,clock_timestamp()
FROM unnest($1::text[],$2::text[],$3::text[],$4::text[]) AS n(kind,title,target_page,target_id)`, kinds, titles, pages, targets)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(spikes), nil
}

// crashSpike is one group that rose past the spike threshold.
type crashSpike struct {
	Signature, Platform, Title string
	Count7d, CountPrev7d       int64
}

// notification phrases the spike like the console's other alerts, for example "iOS 崩溃分组 EXC_BAD_ACCESS 上升 32%".
func (s crashSpike) notification() Notification {
	title := []rune(s.Title)
	if len(title) > 80 {
		title = append(title[:79], '…')
	}
	platform := crashPlatformName(s.Platform)
	text := platform + " 新增崩溃分组 " + string(title)
	if s.CountPrev7d > 0 {
		rise := (s.Count7d*100+s.CountPrev7d/2)/s.CountPrev7d - 100
		text = platform + " 崩溃分组 " + string(title) + " 上升 " + strconv.FormatInt(rise, 10) + "%"
	}
	return Notification{Kind: NotifyCrashSpike, Title: text, TargetPage: "crash", TargetID: s.Signature}
}

// crashPlatformNames are the product names of the lowercase platform ids telemetry reports, as the console shows them.
var crashPlatformNames = map[string]string{"windows": "Windows", "macos": "macOS", "linux": "Linux", "android": "Android", "ios": "iOS", "harmonyos": "HarmonyOS", "harmony": "HarmonyOS", "ohos": "HarmonyOS"}

func crashPlatformName(platform string) string {
	if name, ok := crashPlatformNames[strings.ToLower(platform)]; ok {
		return name
	}
	return platform
}

// lockCrashSpikes 先取得跨副本事务锁，再用独立语句筛选待告警分组，排除已修复及静默期内已告警的分组。
func lockCrashSpikes(ctx context.Context, tx pgx.Tx) ([]crashSpike, error) {
	var spikes []crashSpike
	batch := &pgx.Batch{}
	batch.Queue(`SELECT pg_advisory_xact_lock($1)`, crashSpikeLock)
	batch.Queue(`WITH c AS (`+crashGroupCounts+`)
SELECT g.signature,g.platform,g.title,c.c7,c.p7 FROM admin_crash_groups g JOIN c USING(signature)
WHERE g.status<>'fixed' AND c.c7>c.p7*$3::float8 AND (c.c7>=$2 OR (c.p7=0 AND g.first_seen>=now()-interval '7 days'))
 AND NOT EXISTS (SELECT 1 FROM admin_notifications n WHERE n.kind=$4 AND n.target_id=g.signature AND n.created_at>now()-$5::interval)
ORDER BY c.c7 DESC,g.signature`, "", crashSpikeMinimum, crashSpikeRise, NotifyCrashSpike, crashSpikeQuiet).Query(func(rows pgx.Rows) error {
		var err error
		spikes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (crashSpike, error) {
			var s crashSpike
			return s, row.Scan(&s.Signature, &s.Platform, &s.Title, &s.Count7d, &s.CountPrev7d)
		})
		return err
	})
	err := tx.SendBatch(ctx, batch).Close()
	return spikes, err
}

var (
	crashAddressPattern  = regexp.MustCompile(`0[xX][0-9a-fA-F]+`)
	crashUUIDPattern     = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	crashHexRunPattern   = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	crashDigitsPattern   = regexp.MustCompile(`[0-9]+`)
	crashFrameIndex      = regexp.MustCompile(`^(?:#?[0-9]+[:.]?\s+|at\s+)+`)
	crashFrameOffset     = regexp.MustCompile(`\s*\+\s*[0-9]+\s*$`)
	crashFrameLineNumber = regexp.MustCompile(`(?::[0-9]+)+`)
	crashSpaces          = regexp.MustCompile(`\s+`)
)

// crashSystemPrefixes mark frames of the operating system, language runtimes and standard libraries; the first frame whose fields match none of them identifies where the crash happened in our code.
var crashSystemPrefixes = []string{
	// Apple
	"libsystem_", "libdyld", "libobjc", "dyld", "libdispatch", "libswift", "uikit", "corefoundation", "foundation", "graphicsservices", "appkit", "hitoolbox", "frontboardservices", "coreautolayout",
	// Windows and .NET
	"ntdll", "kernel32", "kernelbase", "ucrtbase", "msvcp", "vcruntime", "user32", "combase", "rpcrt4", "system.", "microsoft.",
	// JVM and Android
	"java.", "javax.", "jdk.", "sun.", "android.", "androidx.", "dalvik.", "com.android.", "kotlin.", "kotlinx.", "libart", "libandroid_runtime", "art::",
	// C and C++ runtimes, Linux, HarmonyOS
	"libc.", "libc++", "libstdc++", "libpthread", "libgcc", "libm.", "ld-linux", "__libc_start", "start_thread", "libace", "libmusl",
	// Rust
	"std::", "core::", "alloc::", "rust_begin_unwind", "__rust", "backtrace::",
	// Unsymbolicated
	"???", "<unknown>",
}

// crashSystemFrame reports whether a normalized, lowercased frame belongs to the system: any of its fields (module, symbol) starts with a system prefix.
func crashSystemFrame(frame string) bool {
	for _, field := range strings.Fields(frame) {
		field = strings.TrimLeft(field, "-+[(")
		for _, prefix := range crashSystemPrefixes {
			if strings.HasPrefix(field, prefix) {
				return true
			}
		}
	}
	return false
}

// normalizeCrashFrame strips what varies between builds and runs from one stack line: frame numbers, addresses, offsets and line numbers.
func normalizeCrashFrame(line string) string {
	line = strings.TrimSpace(line)
	line = crashFrameIndex.ReplaceAllString(line, "")
	line = crashAddressPattern.ReplaceAllString(line, "")
	line = crashFrameOffset.ReplaceAllString(line, "")
	line = crashFrameLineNumber.ReplaceAllString(line, "")
	return strings.ToLower(strings.TrimSpace(crashSpaces.ReplaceAllString(line, " ")))
}

// normalizeCrashMessage keeps the first non-blank line of a crash message with addresses, identifiers and numbers masked, so the same failure groups across runs.
func normalizeCrashMessage(message string) string {
	line := crashFirstLine(message)
	line = crashAddressPattern.ReplaceAllString(line, "0x?")
	line = crashUUIDPattern.ReplaceAllString(line, "?")
	line = crashHexRunPattern.ReplaceAllString(line, "?")
	line = crashDigitsPattern.ReplaceAllString(line, "#")
	return strings.ToLower(strings.TrimSpace(crashSpaces.ReplaceAllString(line, " ")))
}

func crashFirstLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// crashStackLines bounds how many stack lines are searched for the first non-system frame.
const crashStackLines = 256

// crashPlatform maps a client-reported platform name to the canonical key that overviewPlatform computes in SQL: windows (also win), macos (also mac, darwin), linux, android, ios (also ipados), harmonyos (also harmony, ohos); any other name is kept lowercased.
func crashPlatform(platform string) string {
	switch platform = strings.ToLower(platform); platform {
	case "win":
		return "windows"
	case "mac", "darwin":
		return "macos"
	case "ipados":
		return "ios"
	case "harmony", "ohos":
		return "harmonyos"
	}
	return platform
}

// crashSignature 用标准平台、规范化消息和前 crashStackLines 行中的首个非系统栈帧计算 SHA256 的前 16 位十六进制摘要。同一故障在不同平台分别分组，组的平台不会改变；空签名表示不分组。
func crashSignature(platform, message, stack string) string {
	normalized := normalizeCrashMessage(message)
	frame := ""
	lineCount := 0
	for line := range strings.SplitSeq(stack, "\n") {
		lineCount++
		if candidate := normalizeCrashFrame(line); candidate != "" && !crashSystemFrame(candidate) {
			frame = candidate
			break
		}
		if lineCount == crashStackLines {
			break
		}
	}
	if normalized == "" && frame == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(crashPlatform(platform) + "\n" + normalized + "\n" + frame))
	return hex.EncodeToString(sum[:8])
}

// crashGroupTitle is the title of a new crash group, shared by the telemetry insert and the backfill: the first non-blank line of its message, trimmed, at most 200 characters.
func crashGroupTitle(message string) string {
	line := crashFirstLine(message)
	if utf8.RuneCountInString(line) > 200 {
		line = strings.TrimSpace(string([]rune(line)[:200]))
	}
	return line
}

// 回填每批独立读取，事件签名与分组写入同一隐式事务；每次启动最多处理 crashBackfillBatches 批，剩余历史留到下次启动，避免阻塞过久。
const (
	crashBackfillBatch   = 500
	crashBackfillBatches = 40
)

// backfillCrashSignatures computes the signature of stored crashes that have none, with crashSignature, and creates their groups. AdminReady runs it at every startup, so it must be cheap when nothing is left to fill.
func (a *Service) backfillCrashSignatures(ctx context.Context) error {
	// The keyset cursor moves past crashes that cannot be grouped, so they are read once per startup instead of in a loop.
	var afterTime time.Time
	afterID := ""
	for range crashBackfillBatches {
		more, lastTime, lastID, err := a.backfillCrashBatch(ctx, afterTime, afterID)
		if err != nil || !more {
			return err
		}
		afterTime, afterID = lastTime, lastID
	}
	return nil
}

// crashBackfillGroup aggregates the crashes of one signature found in a batch.
type crashBackfillGroup struct {
	platform, version, title string
	first, last              time.Time
}

// backfillCrashBatch signs the next batch of unsigned crashes after the cursor and reports whether more may follow, with the new cursor.
func (a *Service) backfillCrashBatch(ctx context.Context, afterTime time.Time, afterID string) (bool, time.Time, string, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,platform,version,message,stack,created_at FROM admin_events
WHERE kind='crash' AND signature IS NULL AND (created_at,id)>($1,$2) ORDER BY created_at,id LIMIT $3`, afterTime, afterID, crashBackfillBatch)
	if err != nil {
		return false, afterTime, afterID, err
	}
	type event struct {
		id, platform, version, message, stack string
		created                               time.Time
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (event, error) {
		var e event
		return e, row.Scan(&e.id, &e.platform, &e.version, &e.message, &e.stack, &e.created)
	})
	if err != nil || len(events) == 0 {
		return false, afterTime, afterID, err
	}
	var ids, signatures []string
	groups := map[string]*crashBackfillGroup{}
	for _, e := range events {
		signature := crashSignature(e.platform, e.message, e.stack)
		if signature == "" {
			continue
		}
		ids, signatures = append(ids, e.id), append(signatures, signature)
		group := groups[signature]
		if group == nil {
			group = &crashBackfillGroup{platform: crashPlatform(e.platform), title: crashGroupTitle(e.message), first: e.created}
			groups[signature] = group
		}
		// Events arrive oldest first, so the latest version wins.
		group.version, group.last = e.version, e.created
	}
	if len(ids) > 0 {
		batch := &pgx.Batch{}
		batch.Queue(`UPDATE admin_events e SET signature=v.signature FROM unnest($1::text[],$2::text[]) v(id,signature) WHERE e.id=v.id AND e.signature IS NULL`, ids, signatures)
		var keys, platforms, versions, titles []string
		var firsts, lasts []time.Time
		// 按固定顺序更新分组，避免两个副本同时回填时争用分组行而死锁。
		for _, signature := range slices.Sorted(maps.Keys(groups)) {
			group := groups[signature]
			keys, platforms, versions, titles = append(keys, signature), append(platforms, group.platform), append(versions, group.version), append(titles, group.title)
			firsts, lasts = append(firsts, group.first), append(lasts, group.last)
		}
		batch.Queue(`INSERT INTO admin_crash_groups(signature,platform,version,title,first_seen,last_seen)
SELECT * FROM unnest($1::text[],$2::text[],$3::text[],$4::text[],$5::timestamptz[],$6::timestamptz[])
ON CONFLICT(signature) DO UPDATE SET first_seen=LEAST(admin_crash_groups.first_seen,EXCLUDED.first_seen),
 last_seen=GREATEST(admin_crash_groups.last_seen,EXCLUDED.last_seen),
 version=CASE WHEN EXCLUDED.last_seen>admin_crash_groups.last_seen THEN EXCLUDED.version ELSE admin_crash_groups.version END`, keys, platforms, versions, titles, firsts, lasts)
		if err = a.store.pool.SendBatch(ctx, batch).Close(); err != nil {
			return false, afterTime, afterID, err
		}
	}
	last := events[len(events)-1]
	return len(events) == crashBackfillBatch, last.created, last.id, nil
}
