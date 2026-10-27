package account

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"

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

type crashStatusChange struct {
	Status string `json:"status"`
	Token  string `json:"token"`
}

// actionCrashGroupStatus 在同一条语句内锁定、校验版本并修改状态；字符串值保留直接设置的兼容性。
func actionCrashGroupStatus(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if !ValidCrashSignature(v.ID) {
		return actionResult{}, actionFail(400, "invalid_id")
	}
	var change crashStatusChange
	if json.Unmarshal(v.Value, &change.Status) != nil {
		if httpjson.DecodeBytes(v.Value, &change) != nil || !textutil.ValidHex(change.Token, 16) {
			return actionResult{}, actionFail(400, "invalid_value")
		}
	}
	if !crashGroupStatuses[change.Status] {
		return actionResult{}, actionFail(400, "invalid_value")
	}
	var previous string
	var token *string
	err := tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT signature,status,status_token FROM admin_crash_groups WHERE signature=$1 FOR UPDATE
), updated AS (
 UPDATE admin_crash_groups g SET status=$2 FROM locked l
 WHERE g.signature=l.signature AND ($3='' OR l.status_token=$3) RETURNING g.status_token
)
SELECT l.status,u.status_token FROM locked l LEFT JOIN updated u ON true`, v.ID, change.Status, change.Token).Scan(&previous, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		return actionResult{}, actionFail(404, "not_found")
	}
	if err != nil {
		return actionResult{}, err
	}
	if token == nil {
		return actionResult{}, actionFail(409, "conflict")
	}
	detail := map[string]any{"from": previous, "to": change.Status}
	if v.Reason != "" {
		detail["reason"] = v.Reason
	}
	undo := map[string]any{"action": "crash_group_status", "id": v.ID, "value": crashStatusChange{Status: previous, Token: *token}}
	return actionResult{Affected: 1, Detail: detail, Extra: map[string]any{"previous": previous, "undo": undo}}, nil
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

// crashSummary 是崩溃页顶部的统计。安装和会话来自可选的 active、session 遥测，客户端未上报时为 null 而不是 0。CrashFreeRate 是近 7 天的无崩溃会话率，与概览的 crash_free_rate 同一规则（见 admin_crash_free.go），只合计会话数据充分的版本；Versions 列出近 7 天有会话的各版本及是否数据不足。ActiveDevices7d 是近 7 天去重活跃设备；Devices7d 统计全部 `crash` 事件（与分组的影响设备一致），CrashDevices7d 只统计真正的崩溃，ForcedTerminationDevices7d 统计被系统强制终止的设备（见 crashForcedTerminationSQL）；DeviceCrashRate 是 CrashDevices7d 除以 ActiveDevices7d，没有活跃设备时为 null。统计查询列出所有事件类型，让查询计划使用 admin_events_kind_time 而不是扫描整张表。
type crashSummary struct {
	Groups          int64                `json:"groups"`
	Crashes7d       int64                `json:"crashes_7d"`
	Devices7d       *int64               `json:"devices_7d"`
	InstallsToday   *int64               `json:"installs_today"`
	CrashFreeRate   *float64             `json:"crash_free_rate"`
	ActiveDevices7d *int64               `json:"active_devices_7d"`
	DeviceCrashRate *float64             `json:"device_crash_rate"`
	Versions        []sessionVersionStat `json:"versions"`
	// CrashDevices7d 和 ForcedTerminationDevices7d 把 Devices7d 拆成真正崩溃和系统强制终止两部分；一台设备两种都有时各算一次。
	CrashDevices7d             int64 `json:"crash_devices_7d"`
	ForcedTerminationDevices7d int64 `json:"forced_termination_devices_7d"`
}

type crashPlatformCount struct {
	Platform string `json:"platform"`
	Count    int64  `json:"count"`
}

// crashSummaryQuery 在初始化时构建固定统计查询，规范化后的平台仍按请求绑定。
var crashSummaryQuery = `WITH session_versions AS (` + sessionVersionsSQL(`created_at>=now()-interval '7 days' AND ($1='' OR `+overviewPlatform("platform")+`=$1)`) + `)
SELECT
 (SELECT count(*) FROM admin_crash_groups WHERE $1='' OR platform=$1),
 count(*) FILTER (WHERE kind='crash'),
 CASE WHEN count(install_id) FILTER (WHERE kind='crash')=0 THEN NULL ELSE count(DISTINCT install_id) FILTER (WHERE kind='crash') END,
 CASE WHEN count(install_id)=0 THEN NULL ELSE count(DISTINCT install_id) FILTER (WHERE created_at>=date_trunc('day',now(),'UTC')) END,
 (SELECT ` + crashFreeRateSQL + ` FROM session_versions),
 CASE WHEN count(install_id) FILTER (WHERE kind='active')=0 THEN NULL ELSE count(DISTINCT install_id) FILTER (WHERE kind='active') END,
 round(count(DISTINCT install_id) FILTER (WHERE kind='crash' AND NOT ` + crashForcedTerminationSQL("message") + `)::numeric/NULLIF(count(DISTINCT install_id) FILTER (WHERE kind='active'),0),4)::float8,
 (SELECT COALESCE(json_agg(` + sessionVersionJSON + ` ORDER BY platform,sessions+crashed DESC,version),'[]'::json) FROM session_versions WHERE sessions+crashed>0),
 count(DISTINCT install_id) FILTER (WHERE kind='crash' AND NOT ` + crashForcedTerminationSQL("message") + `),
 count(DISTINCT install_id) FILTER (WHERE kind='crash' AND ` + crashForcedTerminationSQL("message") + `)
	FROM admin_events WHERE kind IN ('download','crash','active','session','session_crash') AND created_at>=now()-interval '7 days' AND ($1='' OR ` + overviewPlatform("platform") + `=$1)`

// 列表查询的 $4 非空时只统计该版本的崩溃，并只保留近 14 天在该版本出现过的分组。
const (
	crashGroupListQueryAll = `WITH c AS (` + crashGroupCountsBase + ` AND ($4='' OR version=$4) GROUP BY signature)
SELECT ` + crashGroupColumns + ` FROM admin_crash_groups g LEFT JOIN c USING(signature)
WHERE ($2='' OR g.platform=$2) AND ($4='' OR c.c7 IS NOT NULL)
ORDER BY COALESCE(c.c7,0) DESC,g.last_seen DESC,g.signature LIMIT $3`
	crashGroupListQueryPlatform = `WITH c AS (` + crashGroupCountsBase + ` AND signature IN (SELECT signature FROM admin_crash_groups WHERE platform=$2) AND ($4='' OR version=$4) GROUP BY signature)
SELECT ` + crashGroupColumns + ` FROM admin_crash_groups g LEFT JOIN c USING(signature)
WHERE ($2='' OR g.platform=$2) AND ($4='' OR c.c7 IS NOT NULL)
ORDER BY COALESCE(c.c7,0) DESC,g.last_seen DESC,g.signature LIMIT $3`
	// crashVersionsQuery 列出所选平台近 14 天有崩溃的版本及各自涉及的分组数，窗口与分组统计一致。
	crashVersionsQuery = `SELECT e.version,count(DISTINCT e.signature) FROM admin_events e JOIN admin_crash_groups g USING(signature)
WHERE e.kind='crash' AND e.created_at>=now()-interval '14 days' AND ($1='' OR g.platform=$1)
GROUP BY e.version`
	crashGroupDetailQuery = `WITH c AS (` + crashGroupCounts + `)
SELECT ` + crashGroupColumns + `,
 s.id IS NOT NULL,COALESCE(s.id,''),COALESCE(s.platform,''),COALESCE(s.version,''),
 COALESCE(s.message,''),COALESCE(s.stack,''),COALESCE(s.resolved,false),COALESCE(s.created_at,g.last_seen)
FROM admin_crash_groups g LEFT JOIN c USING(signature)
LEFT JOIN LATERAL (
 SELECT id,platform,version,message,stack,resolved,created_at FROM admin_events
 WHERE kind='crash' AND signature=$1 ORDER BY created_at DESC,id DESC LIMIT $2
) s ON true
WHERE g.signature=$1 ORDER BY s.created_at DESC,s.id DESC`
	crashSpikeQuery = `WITH c AS (` + crashGroupCounts + `)
SELECT g.signature,g.platform,g.title,c.c7,c.p7 FROM admin_crash_groups g JOIN c USING(signature)
WHERE g.status<>'fixed' AND c.c7>c.p7*$3::float8 AND (c.c7>=$2 OR (c.p7=0 AND g.first_seen>=now()-interval '7 days'))
 AND NOT EXISTS (SELECT 1 FROM admin_notifications n WHERE n.kind=$4 AND n.target_id=g.signature AND n.created_at>now()-$5::interval)
ORDER BY c.c7 DESC,g.signature`
)

func crashGroupListQuery(platform bool) string {
	if platform {
		return crashGroupListQueryPlatform
	}
	return crashGroupListQueryAll
}

// crashVersionCount 是某个版本近 14 天有崩溃的分组数，供崩溃页的版本筛选。
type crashVersionCount struct {
	Version string `json:"version"`
	Count   int64  `json:"count"`
}

// compareCrashVersions 按版本号比较：先逐段比较 `-`、`+` 之前以点分隔的部分，纯数字段按数值比较；主体相同时正式版高于预发布版；仍相同时按原字符串比较，保证顺序稳定。
func compareCrashVersions(a, b string) int {
	coreA, preA := splitCrashVersion(a)
	coreB, preB := splitCrashVersion(b)
	if c := compareCrashVersionParts(coreA, coreB); c != 0 {
		return c
	}
	switch {
	case preA == "" && preB != "":
		return 1
	case preA != "" && preB == "":
		return -1
	}
	if c := compareCrashVersionParts(strings.Split(preA, "."), strings.Split(preB, ".")); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// splitCrashVersion 去掉开头的 v，把版本拆成点分隔的主体和 `-` 或 `+` 之后的预发布部分。
func splitCrashVersion(v string) ([]string, string) {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	return strings.Split(v, "."), pre
}

// compareCrashVersionParts 逐段比较；纯数字段高于非数字段，让 `unknown` 这类上报值排在真实版本之后；两个数字段去掉前导零后先比长度再比字典序，避免整数溢出；缺少的段视为更小。
func compareCrashVersionParts(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		digitsX, digitsY := crashVersionDigits(x), crashVersionDigits(y)
		if digitsX != digitsY {
			if digitsX {
				return 1
			}
			return -1
		}
		if digitsX {
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if c := cmp.Compare(len(x), len(y)); c != 0 {
				return c
			}
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

func crashVersionDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// adminCrashGroups 处理 GET /api/crash-groups?platform=&version=；version 只筛选分组列表和分组的次数，顶部统计仍按平台计算。
func (a *Service) adminCrashGroups(w http.ResponseWriter, r *http.Request, _ string) {
	query := r.URL.Query()
	for key := range query {
		if key != "platform" && key != "version" {
			writeError(w, 400, "invalid_filter")
			return
		}
	}
	platform, version := query.Get("platform"), query.Get("version")
	if len(query["platform"]) > 1 || !resourceText(platform, 0, 32, false) || len(query["version"]) > 1 || !resourceText(version, 0, 64, false) {
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
	listQuery := crashGroupListQueryAll
	if platform != "" && selectedGroups <= totalGroups/2 {
		// 复用平台计数，只为至多半数分组的平台提前限定签名，避免多数平台的额外关联开销。
		// 按分组自身的平台选签名，不能用可能与分组不同的事件平台代替。
		listQuery = crashGroupListQueryPlatform
	}
	var items []crashGroupRow
	batch := &pgx.Batch{}
	batch.Queue(listQuery, "", platform, crashGroupsLimit+1, version).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		var err error
		items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (crashGroupRow, error) { return scanCrashGroup(row) })
		return err
	})
	var versions []crashVersionCount
	batch.Queue(crashVersionsQuery, platform).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		var err error
		versions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (crashVersionCount, error) {
			var v crashVersionCount
			return v, row.Scan(&v.Version, &v.Count)
		})
		return err
	})
	var summary crashSummary
	batch.Queue(crashSummaryQuery, platform).QueryRow(func(row pgx.Row) error {
		return row.Scan(&summary.Groups, &summary.Crashes7d, &summary.Devices7d, &summary.InstallsToday, &summary.CrashFreeRate, &summary.ActiveDevices7d, &summary.DeviceCrashRate, &summary.Versions, &summary.CrashDevices7d, &summary.ForcedTerminationDevices7d)
	})
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		a.error(w, err)
		return
	}
	hasMore := len(items) > crashGroupsLimit
	if hasMore {
		items = items[:crashGroupsLimit]
	}
	// 最新的版本排在最前，供页面的版本下拉框直接使用。
	slices.SortFunc(versions, func(x, y crashVersionCount) int { return compareCrashVersions(y.Version, x.Version) })
	write(w, 200, map[string]any{"items": items, "has_more": hasMore, "platforms": platforms, "crash_versions": versions, "summary": summary})
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
	rows, err := q.Query(ctx, crashGroupDetailQuery, signature, samples)
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

// CrashIssueDestination 是探测 Issue 目标所需的最小分组信息。
type CrashIssueDestination struct {
	Platform string
	IssueURL string
}

// CrashGroupIssueDestination 只按主键读取平台与已有链接，不聚合事件或读取堆栈。
func (a *Service) CrashGroupIssueDestination(ctx context.Context, signature string) (CrashIssueDestination, error) {
	var destination CrashIssueDestination
	err := a.store.pool.QueryRow(ctx, `SELECT platform,COALESCE(issue_url,'') FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&destination.Platform, &destination.IssueURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return CrashIssueDestination{}, ErrCrashGroupNotFound
	}
	return destination, err
}

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
	conn       *pgxpool.Conn
	signature  string
	startToken string
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

// ErrCrashIssueNotOpen 表示分组已知、已修复或已有 Issue，不能再次创建。
var ErrCrashIssueNotOpen = errors.New("crash group is not open")

// Start 在联系 GitHub 前持久标记已知并审计创建尝试，响应丢失或进程退出后仍阻止重复创建。
func (l *CrashIssueLock) Start(ctx context.Context, repo string) error {
	token, err := l.changeAttemptState(ctx, "open", "known", "crash_group_issue_attempt", repo, "")
	if err == nil && token == "" {
		return ErrCrashIssueNotOpen
	}
	if err == nil {
		l.startToken = token
	}
	return err
}

// Reject 仅回滚本次创建尝试；即便后续分诊又回到已知状态，也不能覆盖。
func (l *CrashIssueLock) Reject(ctx context.Context, repo string) error {
	if l.startToken == "" {
		return nil
	}
	_, err := l.changeAttemptState(ctx, "known", "open", "crash_group_issue_rejected", repo, l.startToken)
	return err
}

func (l *CrashIssueLock) changeAttemptState(ctx context.Context, from, to, action, repo, expected string) (string, error) {
	detail, err := auditDetail(map[string]any{"repo": repo, "from": from, "to": to})
	if err != nil {
		return "", err
	}
	var token string
	err = l.conn.QueryRow(ctx, `WITH updated AS (
 UPDATE admin_crash_groups SET status=$3
 WHERE signature=$1 AND status=$2 AND issue_url IS NULL AND ($7='' OR status_token=$7) RETURNING signature,status_token
), audited AS (
 INSERT INTO admin_audit(action,target,actor,detail)
 SELECT $4,signature,$5,$6::jsonb FROM updated RETURNING 1
)
SELECT status_token FROM updated`, l.signature, from, to, action, adminActor(ctx), detail, expected).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return token, err
}

// Record 使用持锁连接原子登记 Issue 与审计；等待期间已有后续分诊时只登记链接，保留最新状态。
func (l *CrashIssueLock) Record(ctx context.Context, repo string, number int64, url string) (string, error) {
	return recordCrashGroupIssue(ctx, l.conn, l.signature, repo, number, url, l.startToken)
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
	_, err := recordCrashGroupIssue(ctx, a.store.pool, signature, repo, number, url, "")
	return err
}

func recordCrashGroupIssue(ctx context.Context, q queryRower, signature, repo string, number int64, url, expected string) (string, error) {
	detail, err := auditDetail(map[string]any{"repo": repo, "number": number, "issue_url": url})
	if err != nil {
		return "", err
	}
	var status string
	err = q.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT status,status_token FROM admin_crash_groups WHERE signature=$1 FOR UPDATE
), updated AS (
 UPDATE admin_crash_groups g SET status=CASE WHEN $5='' OR l.status_token=$5 THEN 'known' ELSE l.status END,issue_url=$2
 FROM locked l WHERE g.signature=$1 RETURNING g.status
), audited AS (
 INSERT INTO admin_audit(action,target,actor,detail)
 SELECT 'crash_group_issue',$1,$3,$4::jsonb||jsonb_build_object('from',l.status,'to',u.status)
 FROM locked l CROSS JOIN updated u
 RETURNING 1
)
SELECT status FROM updated`, signature, url, adminActor(ctx), detail, expected).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCrashGroupNotFound
	}
	return status, err
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

// notification 按控制台其他告警的样式构造通知，例如“iOS 崩溃分组 EXC_BAD_ACCESS 上升 32%”。
func (s crashSpike) notification() Notification {
	title, suffix := s.Title, ""
	count, cut := 0, 0
	for i := range title {
		if count == 79 {
			cut = i
		}
		if count == 80 {
			title, suffix = title[:cut], "…"
			break
		}
		count++
	}
	// 只检查保留的前缀；原有 rune 转换会将每个非法字节替换为 U+FFFD。
	if !utf8.ValidString(title) {
		title = string([]rune(title))
	}
	platform := crashPlatformName(s.Platform)
	var text string
	if s.CountPrev7d > 0 {
		rise := (s.Count7d*100+s.CountPrev7d/2)/s.CountPrev7d - 100
		text = platform + " 崩溃分组 " + title + suffix + " 上升 " + strconv.FormatInt(rise, 10) + "%"
	} else {
		text = platform + " 新增崩溃分组 " + title + suffix
	}
	return Notification{Kind: NotifyCrashSpike, Title: text, TargetPage: "crash", TargetID: s.Signature}
}

// crashPlatformNames are the product names of the lowercase platform ids telemetry reports, as the console shows them.
var crashPlatformNames = map[string]string{"windows": "Windows", "macos": "macOS", "linux": "Linux", "android": "Android", "ios": "iOS", "harmonyos": "HarmonyOS", "harmony": "HarmonyOS", "ohos": "HarmonyOS"}

func crashPlatformName(platform string) string {
	if name, ok := crashPlatformNames[platform]; ok {
		return name
	}
	if crashPlatformASCIILower(platform) {
		return platform
	}
	if name, ok := crashPlatformNames[strings.ToLower(platform)]; ok {
		return name
	}
	return platform
}

// crashPlatformASCIILower 判断平台名是否只含非大写 ASCII 字节；非 ASCII 输入交给 Unicode 小写转换。
func crashPlatformASCIILower(platform string) bool {
	for i := 0; i < len(platform); i++ {
		if platform[i] >= utf8.RuneSelf || platform[i] >= 'A' && platform[i] <= 'Z' {
			return false
		}
	}
	return true
}

// lockCrashSpikes 先取得跨副本事务锁，再用独立语句筛选待告警分组，排除已修复及静默期内已告警的分组。
func lockCrashSpikes(ctx context.Context, tx pgx.Tx) ([]crashSpike, error) {
	var spikes []crashSpike
	batch := &pgx.Batch{}
	batch.Queue(`SELECT pg_advisory_xact_lock($1)`, crashSpikeLock)
	batch.Queue(crashSpikeQuery, "", crashSpikeMinimum, crashSpikeRise, NotifyCrashSpike, crashSpikeQuiet).Query(func(rows pgx.Rows) error {
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

// normalizeCrashWhitespace 折叠规范化文本中的空白并统一为小写。
func normalizeCrashWhitespace(text string) string {
	text = strings.TrimSpace(text)
	first, size := -1, len(text)
	for i := 0; i < len(text); {
		if !crashWhitespaceByte(text[i]) {
			i++
			continue
		}
		end := i + 1
		for end < len(text) && crashWhitespaceByte(text[end]) {
			end++
		}
		if text[i] != ' ' || end != i+1 {
			if first < 0 {
				first = i
			}
			size -= end - i - 1
		}
		i = end
	}
	if first >= 0 {
		// 按折叠后的实际长度一次分配，避免为长空白段预留原始大小的缓冲。
		var out strings.Builder
		out.Grow(size)
		start := 0
		for i := first; i < len(text); {
			if !crashWhitespaceByte(text[i]) {
				i++
				continue
			}
			end := i + 1
			for end < len(text) && crashWhitespaceByte(text[end]) {
				end++
			}
			out.WriteString(text[start:i])
			out.WriteByte(' ')
			start = end
			i = end
		}
		out.WriteString(text[start:])
		text = out.String()
	}
	return strings.ToLower(text)
}

// 与 Go 正则的 \s 一致，不折叠内部垂直制表符和 Unicode 空白。
func crashWhitespaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

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

// 按首字节索引固定的系统前缀，避免每个字段都扫描其他首字节的候选。
var crashSystemPrefixesByByte = func() [256][]string {
	var prefixes [256][]string
	for _, prefix := range crashSystemPrefixes {
		prefixes[prefix[0]] = append(prefixes[prefix[0]], prefix)
	}
	return prefixes
}()

// crashSystemFrame 判断规范化的小写栈帧是否属于系统：模块或符号的任一字段以系统前缀开头即命中。
func crashSystemFrame(frame string) bool {
	for field := range strings.FieldsSeq(frame) {
		field = strings.TrimLeft(field, "-+[(")
		if field == "" {
			continue
		}
		for _, prefix := range crashSystemPrefixesByByte[field[0]] {
			if strings.HasPrefix(field, prefix) {
				return true
			}
		}
	}
	return false
}

// normalizeCrashFrame 去除栈行中随构建或运行变化的帧序号、地址、偏移量和行号。
func normalizeCrashFrame(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	// 这些替换都要求 ASCII 数字或正则的 ASCII 空白；纯符号帧直接转小写，跳过后续标记扫描。
	if !strings.ContainsAny(line, "0123456789 \t\n\f\r") {
		return strings.ToLower(line)
	}
	if line[0] >= '0' && line[0] <= '9' || line[0] == '#' || strings.HasPrefix(line, "at") {
		line = trimCrashFrameIndex(line)
	}
	// 只在出现地址标记时扫描，未匹配时直接复用原字符串。
	if strings.Contains(line, "0x") || strings.Contains(line, "0X") {
		line = replaceCrashAddresses(line, "")
	}
	if strings.ContainsRune(line, '+') {
		line = trimCrashFrameOffset(line)
	}
	if strings.ContainsRune(line, ':') {
		line = stripCrashFrameLineNumbers(line)
	}
	return normalizeCrashWhitespace(line)
}

// replaceCrashAddresses 替换完整的 ASCII 十六进制地址，不重复处理替换后形成的新标记。
func replaceCrashAddresses(line, replacement string) string {
	zero := strings.IndexByte(line, '0')
	if zero < 0 {
		return line
	}
	first, size := -1, len(line)
	for i := zero; i+2 < len(line); i++ {
		if line[i] != '0' || line[i+1] != 'x' && line[i+1] != 'X' {
			continue
		}
		end := i + 2
		for end < len(line) && crashHexByte(line[end]) {
			end++
		}
		if end == i+2 {
			continue
		}
		if first < 0 {
			first = i
		}
		size += len(replacement) - (end - i)
		i = end - 1
	}
	if first < 0 {
		return line
	}
	// 按替换后的长度一次分配，长地址不会撑大输出缓冲。
	var out strings.Builder
	out.Grow(size)
	start := 0
	for i := first; i+2 < len(line); i++ {
		if line[i] != '0' || line[i+1] != 'x' && line[i+1] != 'X' {
			continue
		}
		end := i + 2
		for end < len(line) && crashHexByte(line[end]) {
			end++
		}
		if end == i+2 {
			continue
		}
		out.WriteString(line[start:i])
		out.WriteString(replacement)
		start = end
		i = end - 1
	}
	out.WriteString(line[start:])
	return out.String()
}

func crashHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// stripCrashFrameLineNumbers 去掉冒号后的连续 ASCII 数字，不改动其他冒号或字节。
func stripCrashFrameLineNumbers(line string) string {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return line
	}
	first, size := -1, len(line)
	for i := colon; i+1 < len(line); i++ {
		if line[i] != ':' || line[i+1] < '0' || line[i+1] > '9' {
			continue
		}
		end := i + 2
		for end < len(line) && line[end] >= '0' && line[end] <= '9' {
			end++
		}
		if first < 0 {
			first = i
		}
		size -= end - i
		i = end - 1
	}
	if first < 0 {
		return line
	}
	// 按删除行号后的长度一次分配，长数字段不会撑大输出缓冲。
	var out strings.Builder
	out.Grow(size)
	start := 0
	for i := first; i+1 < len(line); i++ {
		if line[i] != ':' || line[i+1] < '0' || line[i+1] > '9' {
			continue
		}
		end := i + 2
		for end < len(line) && line[end] >= '0' && line[end] <= '9' {
			end++
		}
		out.WriteString(line[start:i])
		start = end
		i = end - 1
	}
	out.WriteString(line[start:])
	return out.String()
}

// trimCrashFrameIndex 去掉连续的序号或 at 前缀，只接受原正则的 ASCII 数字和空白。
func trimCrashFrameIndex(line string) string {
	start := 0
	for start < len(line) {
		end := start
		if strings.HasPrefix(line[start:], "at") {
			end += 2
		} else {
			if line[end] == '#' {
				end++
			}
			digits := end
			for end < len(line) && line[end] >= '0' && line[end] <= '9' {
				end++
			}
			if end == digits {
				break
			}
			if end < len(line) && (line[end] == ':' || line[end] == '.') {
				end++
			}
		}
		if end == len(line) || !crashWhitespaceByte(line[end]) {
			break
		}
		for end < len(line) && crashWhitespaceByte(line[end]) {
			end++
		}
		// 只推进完整匹配的前缀；后面不完整的序号或 at 保留在原位置。
		start = end
	}
	return line[start:]
}

// trimCrashFrameOffset 从末尾匹配 ASCII 数字和空白，只裁掉完整的偏移后缀。
func trimCrashFrameOffset(line string) string {
	end := len(line)
	for end > 0 && crashWhitespaceByte(line[end-1]) {
		end--
	}
	digitsEnd := end
	for end > 0 && line[end-1] >= '0' && line[end-1] <= '9' {
		end--
	}
	if end == digitsEnd {
		return line
	}
	for end > 0 && crashWhitespaceByte(line[end-1]) {
		end--
	}
	if end == 0 || line[end-1] != '+' {
		return line
	}
	end--
	// 包含 `+` 前的 ASCII 空白，保持原正则的最左匹配起点。
	for end > 0 && crashWhitespaceByte(line[end-1]) {
		end--
	}
	return line[:end]
}

// normalizeCrashMessage 保留消息首个非空行，掩去地址、标识符和数字，让同一故障在不同运行中归为一组。
func normalizeCrashMessage(message string) string {
	line := crashFirstLine(message)
	if !crashMessageMayChange(line) {
		return strings.ToLower(line)
	}
	if strings.Contains(line, "0x") || strings.Contains(line, "0X") {
		line = replaceCrashAddresses(line, "0x?")
	}
	line = maskCrashUUIDs(line)
	line = maskCrashHexRuns(line)
	line = maskCrashDigits(line)
	return normalizeCrashWhitespace(line)
}

// maskCrashUUIDs 掩去原正则的完整 UUID 形状，不要求单词边界，也不限制版本或变体。
func maskCrashUUIDs(line string) string {
	first := nextCrashUUID(line, 0)
	if first < 0 {
		return line
	}
	size := len(line)
	for start := first; start >= 0; start = nextCrashUUID(line, start+36) {
		size -= 35
	}
	if size == 1 {
		return "?"
	}
	var out strings.Builder
	out.Grow(size)
	previous := 0
	for start := first; start >= 0; start = nextCrashUUID(line, start+36) {
		out.WriteString(line[previous:start])
		out.WriteByte('?')
		previous = start + 36
	}
	out.WriteString(line[previous:])
	return out.String()
}

// nextCrashUUID 按首个连字符定位候选，允许在更长十六进制段内部开始或结束。
func nextCrashUUID(line string, from int) int {
	if len(line)-from < 36 {
		return -1
	}
	for search := from + 8; ; {
		dash := strings.IndexByte(line[search:], '-')
		if dash < 0 {
			return -1
		}
		dash += search
		if len(line)-dash < 28 {
			return -1
		}
		start := dash - 8
		if line[dash+5] == '-' && line[dash+10] == '-' && line[dash+15] == '-' &&
			textutil.ValidHex(line[start:dash], 4) && textutil.ValidHex(line[dash+1:dash+5], 2) &&
			textutil.ValidHex(line[dash+6:dash+10], 2) && textutil.ValidHex(line[dash+11:dash+15], 2) &&
			textutil.ValidHex(line[dash+16:dash+28], 6) {
			return start
		}
		search = dash + 1
	}
}

// maskCrashDigits 将每段连续 ASCII 数字替换为单个井号，其余字节保持原样。
func maskCrashDigits(line string) string {
	first := strings.IndexAny(line, "0123456789")
	if first < 0 {
		return line
	}
	size := len(line)
	for i := first; i < len(line); i++ {
		if line[i] < '0' || line[i] > '9' {
			continue
		}
		end := i + 1
		for end < len(line) && line[end] >= '0' && line[end] <= '9' {
			end++
		}
		size -= end - i - 1
		i = end - 1
	}
	// 输出长度为一说明整行都是数字，直接复用固定掩码。
	if size == 1 {
		return "#"
	}
	// 按实际输出长度一次分配，长数字段不会撑大输出缓冲。
	var out strings.Builder
	out.Grow(size)
	start := 0
	for i := first; i < len(line); i++ {
		if line[i] < '0' || line[i] > '9' {
			continue
		}
		end := i + 1
		for end < len(line) && line[end] >= '0' && line[end] <= '9' {
			end++
		}
		out.WriteString(line[start:i])
		out.WriteByte('#')
		start = end
		i = end - 1
	}
	out.WriteString(line[start:])
	return out.String()
}

// maskCrashHexRuns 掩去有 ASCII 单词边界、至少八位的十六进制串，不改动其余字节。
func maskCrashHexRuns(line string) string {
	if len(line) < 8 {
		return line
	}
	first, end := nextCrashHexRun(line, 0)
	if first < 0 {
		return line
	}
	size := len(line)
	for start, stop := first, end; start >= 0; start, stop = nextCrashHexRun(line, stop) {
		size -= stop - start - 1
	}
	// 输出长度为一说明整行匹配，直接复用固定掩码。
	if size == 1 {
		return "?"
	}
	var out strings.Builder
	out.Grow(size)
	previous := 0
	for start, stop := first, end; start >= 0; start, stop = nextCrashHexRun(line, stop) {
		out.WriteString(line[previous:start])
		out.WriteByte('?')
		previous = stop
	}
	out.WriteString(line[previous:])
	return out.String()
}

// nextCrashHexRun 查找原输入中的下一个完整匹配，边界沿用标准库的 ASCII 单词字符定义。
func nextCrashHexRun(line string, from int) (int, int) {
	start := from
	for i := from; i < len(line); i++ {
		if crashHexByte(line[i]) {
			continue
		}
		// 非 ASCII 字节与原正则解码出的非 ASCII 字符一样，都不属于单词字符。
		if i-start >= 8 && (start == 0 || !syntax.IsWordChar(rune(line[start-1]))) && !syntax.IsWordChar(rune(line[i])) {
			return start, i
		}
		start = i + 1
	}
	if len(line)-start >= 8 && (start == 0 || !syntax.IsWordChar(rune(line[start-1]))) {
		return start, len(line)
	}
	return -1, 0
}

// crashMessageMayChange 保守筛选可能被正则替换的行；数字和控制空白先用字节查找，八个连续十六进制字母也覆盖不含数字、嵌在单词中的 UUID，单个空格无需替换。
func crashMessageMayChange(line string) bool {
	if strings.IndexAny(line, "0123456789\t\n\f\r") >= 0 || strings.Contains(line, "  ") {
		return true
	}
	hexRun := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c >= utf8.RuneSelf {
			hexRun = 0
			continue
		}
		switch {
		case c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
			hexRun++
			if hexRun == 8 {
				return true
			}
		default:
			hexRun = 0
		}
	}
	return false
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
	switch platform {
	case "win":
		return "windows"
	case "mac", "darwin":
		return "macos"
	case "ipados":
		return "ios"
	case "harmony", "ohos":
		return "harmonyos"
	}
	if crashPlatformASCIILower(platform) {
		return platform
	}
	platform = strings.ToLower(platform)
	switch platform {
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
	previousLine, previousFrame := "", ""
	previousFrameValid := false
	lineCount := 0
	for line := range strings.SplitSeq(stack, "\n") {
		lineCount++
		candidate := ""
		if previousFrameValid && line == previousLine {
			candidate = previousFrame
		} else {
			candidate = normalizeCrashFrame(line)
			previousLine, previousFrame, previousFrameValid = line, candidate, true
		}
		if candidate != "" && !crashSystemFrame(candidate) {
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
	h := sha256.New()
	// 固定缓冲分段写入，避免拼接或转换长字符串时分配整串副本。
	var block [4 * sha256.BlockSize]byte
	for _, part := range [...]string{crashPlatform(platform), "\n", normalized, "\n", frame} {
		for len(part) > 0 {
			n := copy(block[:], part)
			h.Write(block[:n])
			part = part[n:]
		}
	}
	var sum [sha256.Size]byte
	return hex.EncodeToString(h.Sum(sum[:0])[:8])
}

// crashGroupTitle 供遥测写入与历史回填共用，取消息中首个非空行，去除首尾空白，最多保留 200 个字符。
func crashGroupTitle(message string) string {
	line := crashFirstLine(message)
	if len(line) <= 200 {
		return line
	}
	n := 0
	for i := range line {
		if n == 200 {
			line = line[:i]
			// 原实现仅在截断时逐字节替换非法 UTF-8；合法前缀可以直接复用。
			if !utf8.ValidString(line) {
				line = string([]rune(line))
			}
			return strings.TrimSpace(line)
		}
		n++
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
