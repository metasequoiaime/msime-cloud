package account

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpquery"
	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"
)

// overviewPlatform 统一客户端平台别名，让近七天和每日活跃统计使用相同的平台键；未知平台保留小写名称。
func overviewPlatform(column string) string {
	return `(CASE lower(` + column + `) WHEN 'win' THEN 'windows' WHEN 'mac' THEN 'macos' WHEN 'darwin' THEN 'macos' WHEN 'ipados' THEN 'ios' WHEN 'harmony' THEN 'harmonyos' WHEN 'ohos' THEN 'harmonyos' ELSE lower(` + column + `) END)`
}

// platformAliases 把客户端对平台的其他叫法映射到规范的客户端平台 ID（windows、macos、linux、android、ios、harmony），公告投放和下载分组都用这套 ID。
var platformAliases = map[string]string{"win": "windows", "mac": "macos", "darwin": "macos", "ipados": "ios", "harmonyos": "harmony", "ohos": "harmony"}

// canonicalPlatform 把客户端上报的平台名转为小写，已知别名映射到规范 ID；其他名称原样以小写返回。
func canonicalPlatform(platform string) string {
	platform = strings.TrimSpace(platform)
	if textutil.IsASCII(platform) {
		if canonical, ok := canonicalPlatformASCII(platform); ok {
			return canonical
		}
		if isASCIILower(platform) {
			return platform
		}
		lower := strings.ToLower(platform)
		if canonical, ok := platformAliases[lower]; ok {
			return canonical
		}
		return lower
	}
	platform = strings.ToLower(platform)
	if canonical, ok := platformAliases[platform]; ok {
		return canonical
	}
	return platform
}

func canonicalPlatformASCII(platform string) (string, bool) {
	if len(platform) == 0 {
		return "", false
	}
	first := platform[0]
	if first >= 'A' && first <= 'Z' {
		first += 'a' - 'A'
	}
	switch first {
	case 'a':
		if textutil.EqualFoldASCII(platform, "android") {
			return "android", true
		}
	case 'd':
		if textutil.EqualFoldASCII(platform, "darwin") {
			return "macos", true
		}
	case 'h':
		if textutil.EqualFoldASCII(platform, "harmony") || textutil.EqualFoldASCII(platform, "harmonyos") {
			return "harmony", true
		}
	case 'i':
		if textutil.EqualFoldASCII(platform, "ios") || textutil.EqualFoldASCII(platform, "ipados") {
			return "ios", true
		}
	case 'l':
		if textutil.EqualFoldASCII(platform, "linux") {
			return "linux", true
		}
	case 'm':
		if textutil.EqualFoldASCII(platform, "macos") || textutil.EqualFoldASCII(platform, "mac") {
			return "macos", true
		}
	case 'o':
		if textutil.EqualFoldASCII(platform, "ohos") {
			return "harmony", true
		}
	case 'w':
		if textutil.EqualFoldASCII(platform, "windows") || textutil.EqualFoldASCII(platform, "win") {
			return "windows", true
		}
	}
	return "", false
}

func isASCIILower(platform string) bool {
	for i := range platform {
		if platform[i] >= 'A' && platform[i] <= 'Z' {
			return false
		}
	}
	return true
}

// canonicalPlatformSQL 是 canonicalPlatform 对 column 的 SQL 表达式版本，让以别名上报的行与规范名称归为一组。
func canonicalPlatformSQL(column string) string {
	var cases strings.Builder
	for _, alias := range slices.Sorted(maps.Keys(platformAliases)) {
		cases.WriteString(" WHEN '" + alias + "' THEN '" + platformAliases[alias] + "'")
	}
	return `(CASE lower(btrim(` + column + `))` + cases.String() + ` ELSE lower(btrim(` + column + `)) END)`
}

// adminOverviewQuery 在初始化时构建固定总览查询；日期范围和服务配置按请求绑定。
var adminOverviewQuery = `WITH session_versions AS (` + sessionVersionsSQL(`created_at>=now()-interval '7 days'`) + `),
 session_versions_prev AS (` + sessionVersionsSQL(`created_at>=now()-interval '14 days' AND created_at<now()-interval '7 days'`) + `),
 crash_device_platforms AS (` + crashDevicePlatformsSQL + `)
SELECT json_build_object(
   'users',(SELECT count(*) FROM auth_users),
   'new_users_30d',(SELECT count(*) FROM auth_users WHERE created_at>=now()-interval '30 days'),
   'session_users',(SELECT count(DISTINCT user_id) FROM auth_sessions WHERE NOT revoked AND expires_at>now()),
   'downloads',(SELECT count(*) FROM admin_events WHERE kind='download'),
   'crashes',crash_counts.crashes,
   'open_crashes',crash_counts.open_crashes,
   'skins',(SELECT count(*) FROM community_skins WHERE moderation<>'removed'),
   'skin_downloads',(` + communitySkinDownloadsQuery + `),
   'plugins',(SELECT count(*) FROM community_plugins WHERE moderation<>'removed'),
   'plugin_downloads',(SELECT count(*) FROM community_plugin_downloads),
   'dictionaries',resource_counts.dictionaries,
   'replies',resource_counts.replies,
   'resource_saves',resource_counts.resource_saves,
   'daily',(WITH overview_days AS (
     SELECT d FROM generate_series(date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - ($1::int - 1) * interval '1 day',date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',interval '1 day') d
   ), user_daily AS (
     SELECT date_trunc('day',created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS d,count(*) AS users
     FROM auth_users
     WHERE created_at>= (SELECT min(d) FROM overview_days) AND created_at < (SELECT max(d) FROM overview_days)+interval '1 day'
     GROUP BY 1
   ), event_daily AS (
     SELECT date_trunc('day',created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS d,
       count(*) FILTER (WHERE kind='download') AS downloads,
       count(*) FILTER (WHERE kind='crash') AS crashes
     FROM admin_events
     WHERE created_at>= (SELECT min(d) FROM overview_days) AND created_at < (SELECT max(d) FROM overview_days)+interval '1 day'
       AND kind IN ('download','crash')
     GROUP BY 1
   ) SELECT json_agg(json_build_object('day',to_char(overview_days.d AT TIME ZONE 'UTC','YYYY-MM-DD'),'users',COALESCE(user_daily.users,0),'downloads',COALESCE(event_daily.downloads,0),'crashes',COALESCE(event_daily.crashes,0)) ORDER BY overview_days.d)
   FROM overview_days LEFT JOIN user_daily USING(d) LEFT JOIN event_daily USING(d)),
   'email_delivery',(WITH email_days AS (
     SELECT d::date AS day FROM generate_series((date_trunc('day',now() AT TIME ZONE 'UTC')::date-($1-1)),date_trunc('day',now() AT TIME ZONE 'UTC')::date,interval '1 day') d
   ) SELECT json_agg(json_build_object('day',to_char(email_days.day,'YYYY-MM-DD'),'attempted',COALESCE(stats.attempted,0),'accepted',COALESCE(stats.accepted,0),'rejected',COALESCE(stats.rejected,0),'failed',COALESCE(stats.failed,0)) ORDER BY email_days.day)
   FROM email_days LEFT JOIN admin_email_delivery_daily stats ON stats.day=email_days.day), 'range_days',$1,
   'downloads_30d',(SELECT count(*) FROM admin_events WHERE kind='download' AND created_at>=now()-interval '30 days'),
   'downloads_prev_30d',(SELECT count(*) FROM admin_events WHERE kind='download' AND created_at>=now()-interval '60 days' AND created_at<now()-interval '30 days'),
   'telemetry',json_build_object(
     'active',EXISTS(SELECT 1 FROM admin_events WHERE kind='active' AND created_at>=now()-interval '60 days'),
     'sessions',EXISTS(SELECT 1 FROM admin_events WHERE kind IN ('session','session_crash') AND created_at>=now()-interval '60 days')),
   'active_devices_daily',(WITH active_days AS (
     SELECT d FROM generate_series(date_trunc('day',now() AT TIME ZONE 'UTC') - ($1::int - 1) * interval '1 day',date_trunc('day',now() AT TIME ZONE 'UTC'),interval '1 day') d
   ), active_devices AS (
     SELECT date_trunc('day',e.created_at AT TIME ZONE 'UTC') AS d,e.install_id,
       ` + overviewPlatform("e.platform") + ` AS platform
     FROM admin_events e
     WHERE e.kind='active' AND e.install_id IS NOT NULL
       AND e.created_at>= (SELECT min(d) AT TIME ZONE 'UTC' FROM active_days)
       AND e.created_at< (SELECT (max(d)+interval '1 day') AT TIME ZONE 'UTC' FROM active_days)
     GROUP BY 1,2,3
   ), active_daily AS (
     SELECT d,count(*) FILTER (WHERE platform='windows') AS windows,
       count(*) FILTER (WHERE platform='macos') AS macos,
       count(*) FILTER (WHERE platform='linux') AS linux,
       count(*) FILTER (WHERE platform='android') AS android,
       count(*) FILTER (WHERE platform='ios') AS ios,
       count(*) FILTER (WHERE platform='harmonyos') AS harmonyos,
       count(DISTINCT install_id) FILTER (WHERE platform IN ('macos','linux')) AS mac_linux,
       count(DISTINCT install_id) FILTER (WHERE platform IN ('android','ios','harmonyos')) AS mobile
     FROM active_devices GROUP BY d
   ) SELECT json_agg(json_build_object('day',to_char(active_days.d,'YYYY-MM-DD'),
     'windows',COALESCE(active_daily.windows,0),'macos',COALESCE(active_daily.macos,0),'linux',COALESCE(active_daily.linux,0),
     'android',COALESCE(active_daily.android,0),'ios',COALESCE(active_daily.ios,0),'harmonyos',COALESCE(active_daily.harmonyos,0),
     'mac_linux',COALESCE(active_daily.mac_linux,0),'mobile',COALESCE(active_daily.mobile,0)) ORDER BY active_days.d)
     FROM active_days LEFT JOIN active_daily USING(d)),
   'active_devices_7d',(SELECT count(DISTINCT install_id) FROM admin_events WHERE kind='active' AND created_at>=now()-interval '7 days'),
   'active_devices_prev_7d',(SELECT count(DISTINCT install_id) FROM admin_events WHERE kind='active' AND created_at>=now()-interval '14 days' AND created_at<now()-interval '7 days'),
   'platform_active_7d',(SELECT COALESCE(json_object_agg(platform,devices ORDER BY devices DESC),'{}'::json) FROM (
     SELECT ` + overviewPlatform("platform") + ` AS platform,count(DISTINCT install_id) AS devices FROM admin_events WHERE kind='active' AND install_id IS NOT NULL AND created_at>=now()-interval '7 days' GROUP BY 1
   ) p),
   'crash_free_rate',(SELECT ` + crashFreeRateSQL + ` FROM session_versions),
   'crash_free_rate_prev',(SELECT ` + crashFreeRateSQL + ` FROM session_versions_prev),
   'crash_top',(SELECT json_build_object('platform',platform,'version',version,'crashes',crashed) FROM session_versions
     WHERE crashed>0 AND NOT insufficient ORDER BY crashed DESC,platform,version LIMIT 1),
   'crash_devices_7d',(SELECT COALESCE(sum(crashed),0) FROM crash_device_platforms),
   'device_crash_rate',(SELECT round(sum(crashed)/NULLIF(sum(active),0),4)::float8 FROM crash_device_platforms),
   'crash_platforms',(SELECT COALESCE(json_agg(json_build_object('platform',platform,'crash_free_rate',s.rate,
       'sessions',COALESCE(s.sessions,0),'crashed_sessions',COALESCE(s.crashed,0),'versions',COALESCE(s.versions,0),'insufficient_versions',COALESCE(s.insufficient,0),
       'active_devices_7d',COALESCE(d.active,0),'crash_devices_7d',COALESCE(d.crashed,0),'device_crash_rate',round(d.crashed::numeric/NULLIF(d.active,0),4)::float8) ORDER BY platform),'[]'::json)
     FROM (SELECT platform,` + crashFreeRateSQL + ` AS rate,
         sum(sessions) FILTER (WHERE NOT insufficient) AS sessions,sum(crashed) FILTER (WHERE NOT insufficient) AS crashed,
         count(*) AS versions,count(*) FILTER (WHERE insufficient) AS insufficient
       FROM session_versions WHERE sessions+crashed>0 GROUP BY platform) s
     FULL JOIN crash_device_platforms d USING(platform)),
   'crash_group_latest',(SELECT json_build_object('signature',signature,'platform',` + overviewPlatform("platform") + `,'title',title) FROM admin_crash_groups WHERE status='open' AND first_seen>=now()-interval '7 days' ORDER BY first_seen DESC,signature LIMIT 1),
   'pending',json_build_object(
   'community',pending_community.total,
     'reports_7d',(SELECT count(*) FROM community_reports WHERE created_at>=now()-interval '7 days'),
     'crash_groups',(SELECT count(*) FROM admin_crash_groups WHERE status='open' AND first_seen>=now()-interval '7 days')),
   'services_configured',$5::bool,
   'services',(SELECT COALESCE(json_agg(json_build_object('key',s.key,'name',s.name,'provider',s.provider,
       'state',CASE WHEN today.total_minutes>0 AND today.ok_minutes=0 THEN 'down'
         WHEN EXISTS(SELECT 1 FROM admin_incidents i WHERE i.service=s.key AND i.state='open') THEN 'degraded'
         WHEN today.total_minutes IS NULL OR today.total_minutes=0 THEN 'unknown'
         WHEN today.degraded THEN 'degraded' ELSE 'ok' END,
       'uptime_60d',(SELECT round(sum(ok_minutes)::numeric*100/NULLIF(sum(total_minutes),0),2) FROM admin_service_daily WHERE service=s.key AND day>=(now() AT TIME ZONE 'UTC')::date-59),
       'p95_ms',today.p95_ms) ORDER BY s.ord),'[]'::json)
     FROM (
       SELECT key,name,provider,ord FROM unnest($2::text[],$3::text[],$4::text[]) WITH ORDINALITY AS c(key,name,provider,ord)
       UNION ALL
       SELECT service,service,'',1000+row_number() OVER (ORDER BY service) FROM (SELECT DISTINCT service FROM admin_service_daily WHERE day>=(now() AT TIME ZONE 'UTC')::date-59) d WHERE cardinality($2::text[])=0
     ) s
     LEFT JOIN LATERAL (SELECT ok_minutes,total_minutes,degraded,p95_ms FROM admin_service_daily WHERE service=s.key AND day>=(now() AT TIME ZONE 'UTC')::date-1 ORDER BY day DESC LIMIT 1) today ON true)) FROM (` + communityResourceCountsQuery + `) resource_counts
 CROSS JOIN (SELECT (SELECT count(*) FROM community_skins WHERE moderation='pending')+
                    (SELECT count(*) FROM community_candidate_skins WHERE moderation='pending' AND visibility='public')+
                    (SELECT count(*) FROM community_plugins WHERE moderation='pending')+
                    (SELECT count(*) FROM community_resources WHERE moderation='pending') AS total) pending_community
 CROSS JOIN (SELECT count(*) AS crashes,count(*) FILTER (WHERE NOT resolved) AS open_crashes FROM admin_events WHERE kind='crash') crash_counts`

// adminOverview serves GET /api/overview?days=7|30 (default 30): totals plus a daily series ending today in UTC, and the console overview additions: active devices (from anonymous `active` telemetry), the crash-free session rate (from `session`/`session_crash` telemetry), pending work kept in the database, and the monitored services' state.
//
// 活跃设备是 `active` 事件去重后的 install_id 数。一个 `session` 事件是一次正常结束的会话，一个 `session_crash` 事件是一次以崩溃结束的会话。crash_free_rate 是近 7 天 session/(session+session_crash)，按 admin_crash_free.go 的规则只合计会话数据充分的版本，没有这样的会话时为 null；crash_free_rate_prev 用同一规则计算再往前 7 天。crash_platforms 按平台给出同口径的无崩溃会话率、计入的会话数、有会话的版本数和其中数据不足的版本数，以及近 7 天活跃设备、崩溃设备和崩溃设备占比；crash_devices_7d、device_crash_rate 是全平台合计。crash_top 是会话数据充分的版本中近 7 天崩溃会话最多的平台和版本（没有时为 null），即比率下方的「拖累」；crash_group_latest 是近 7 天首次出现、仍未处理的最新崩溃分组（没有时为 null）。telemetry 两个标记表示近 60 天是否有客户端上报对应事件，后台据此显示「客户端未上报」而不是 0。
//
// The community counters leave out removed content, like GET /v1/community/stats. services lists admin.services; without them the upstreams the status probe derives from the deployment (DerivedServices), and without those the services the probe has recorded. services_configured says only whether admin.services is set.
func (a *Service) adminOverview(w http.ResponseWriter, r *http.Request, _ string) {
	days, ok := httpquery.Int(r.URL.Query(), "days", 30, 7, 30)
	if !ok || (days != 7 && days != 30) {
		writeError(w, 400, "invalid_days")
		return
	}
	var err error
	keys, names, providers := []string{}, []string{}, []string{}
	services := a.admin.Services
	if len(services) == 0 {
		services = a.admin.DerivedServices
	}
	for _, service := range services {
		keys = append(keys, service.Key)
		names = append(names, service.Name)
		providers = append(providers, service.Provider)
	}
	var result json.RawMessage
	// 每日序列按 UTC 日期聚合后再补齐空日期；活跃设备先按日期、平台组和设备去重，避免重复遍历事件和大范围排序。
	err = a.store.pool.QueryRow(r.Context(), adminOverviewQuery,
		days, keys, names, providers, len(a.admin.Services) > 0).Scan(&result)
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, result)
}
