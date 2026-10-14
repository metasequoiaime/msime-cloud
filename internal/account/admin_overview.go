package account

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// overviewPlatform maps a client-reported platform name in column to one canonical key, so that platform_active_7d and the daily platform groups agree: windows (also win), macos (also mac, darwin), linux, android, ios (also ipados), harmonyos (also harmony, ohos); any other name is kept lowercased.
func overviewPlatform(column string) string {
	return `(CASE lower(` + column + `) WHEN 'win' THEN 'windows' WHEN 'mac' THEN 'macos' WHEN 'darwin' THEN 'macos' WHEN 'ipados' THEN 'ios' WHEN 'harmony' THEN 'harmonyos' WHEN 'ohos' THEN 'harmonyos' ELSE lower(` + column + `) END)`
}

// platformAliases 把客户端对平台的其他叫法映射到规范的客户端平台 ID（windows、macos、linux、android、ios、harmony），公告投放和下载分组都用这套 ID。
var platformAliases = map[string]string{"win": "windows", "mac": "macos", "darwin": "macos", "ipados": "ios", "harmonyos": "harmony", "ohos": "harmony"}

// canonicalPlatform 把客户端上报的平台名转为小写，已知别名映射到规范 ID；其他名称原样以小写返回。
func canonicalPlatform(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if canonical, ok := platformAliases[platform]; ok {
		return canonical
	}
	return platform
}

// canonicalPlatformSQL 是 canonicalPlatform 对 column 的 SQL 表达式版本，让以别名上报的行与规范名称归为一组。
func canonicalPlatformSQL(column string) string {
	var cases strings.Builder
	for _, alias := range slices.Sorted(maps.Keys(platformAliases)) {
		cases.WriteString(" WHEN '" + alias + "' THEN '" + platformAliases[alias] + "'")
	}
	return `(CASE lower(btrim(` + column + `))` + cases.String() + ` ELSE lower(btrim(` + column + `)) END)`
}

// adminOverview serves GET /api/overview?days=7|30 (default 30): totals plus a daily series ending today in UTC, and the console overview additions: active devices (from anonymous `active` telemetry), the crash-free session rate (from `session`/`session_crash` telemetry), pending work kept in the database, and the monitored services' state.
//
// Active devices count distinct install_id values of `active` events. A `session` event is one session that ended normally and a `session_crash` event one that ended in a crash, so the crash-free rate is session/(session+session_crash) over the last 7 days, null without sessions. crash_top is the platform and version with the most crashed sessions in those 7 days (null without any), the 拖累 line under the rate; crash_group_latest is the newest open crash group first seen in the last 7 days (null without any). The telemetry flags say whether any client reported those kinds in the last 60 days, so the console can show 客户端未上报 instead of zeros.
//
// The community counters leave out removed content, like GET /v1/community/stats. services lists admin.services; without them the upstreams the status probe derives from the deployment (DerivedServices), and without those the services the probe has recorded. services_configured says only whether admin.services is set.
func (a *Service) adminOverview(w http.ResponseWriter, r *http.Request, _ string) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || (parsed != 7 && parsed != 30) {
			writeError(w, 400, "invalid_days")
			return
		}
		days = parsed
	}
	community, err := a.PendingCommunity(r.Context())
	if err != nil {
		a.error(w, err)
		return
	}
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
	err = a.store.pool.QueryRow(r.Context(), `SELECT json_build_object(
   'users',(SELECT count(*) FROM auth_users),
   'new_users_30d',(SELECT count(*) FROM auth_users WHERE created_at>=now()-interval '30 days'),
   'session_users',(SELECT count(DISTINCT user_id) FROM auth_sessions WHERE NOT revoked AND expires_at>now()),
   'downloads',(SELECT count(*) FROM admin_events WHERE kind='download'),
   'crashes',(SELECT count(*) FROM admin_events WHERE kind='crash'),
   'open_crashes',(SELECT count(*) FROM admin_events WHERE kind='crash' AND NOT resolved),
   'skins',(SELECT count(*) FROM community_skins WHERE moderation<>'removed'),
   'skin_downloads',(SELECT count(*) FROM community_skin_downloads),
   'plugins',(SELECT count(*) FROM community_plugins WHERE moderation<>'removed'),
   'plugin_downloads',(SELECT count(*) FROM community_plugin_downloads),
   'dictionaries',resource_counts.dictionaries,
   'replies',resource_counts.replies,
   'resource_saves',(SELECT count(*) FROM community_resource_saves),
   'daily',(SELECT json_agg(x ORDER BY day) FROM (
     SELECT to_char(d AT TIME ZONE 'UTC','YYYY-MM-DD') AS day,
     (SELECT count(*) FROM auth_users WHERE created_at>=d AND created_at<d+interval '1 day') AS users,
     (SELECT count(*) FROM admin_events WHERE kind='download' AND created_at>=d AND created_at<d+interval '1 day') AS downloads,
     (SELECT count(*) FROM admin_events WHERE kind='crash' AND created_at>=d AND created_at<d+interval '1 day') AS crashes
     FROM generate_series(date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - ($1::int - 1) * interval '1 day',date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',interval '1 day') d
   ) x), 'range_days',$1,
   'downloads_30d',(SELECT count(*) FROM admin_events WHERE kind='download' AND created_at>=now()-interval '30 days'),
   'downloads_prev_30d',(SELECT count(*) FROM admin_events WHERE kind='download' AND created_at>=now()-interval '60 days' AND created_at<now()-interval '30 days'),
   'telemetry',json_build_object(
     'active',EXISTS(SELECT 1 FROM admin_events WHERE kind='active' AND created_at>=now()-interval '60 days'),
     'sessions',EXISTS(SELECT 1 FROM admin_events WHERE kind IN ('session','session_crash') AND created_at>=now()-interval '60 days')),
   'active_devices_daily',(SELECT json_agg(x ORDER BY day) FROM (
     SELECT to_char(d AT TIME ZONE 'UTC','YYYY-MM-DD') AS day,
     count(DISTINCT e.install_id) FILTER (WHERE `+overviewPlatform("e.platform")+` = 'windows') AS windows,
     count(DISTINCT e.install_id) FILTER (WHERE `+overviewPlatform("e.platform")+` IN ('macos','linux')) AS mac_linux,
     count(DISTINCT e.install_id) FILTER (WHERE `+overviewPlatform("e.platform")+` IN ('android','ios','harmonyos')) AS mobile
     FROM generate_series(date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - ($1::int - 1) * interval '1 day',date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',interval '1 day') d
     LEFT JOIN admin_events e ON e.kind='active' AND e.install_id IS NOT NULL AND e.created_at>=d AND e.created_at<d+interval '1 day'
     GROUP BY d
   ) x),
   'active_devices_7d',(SELECT count(DISTINCT install_id) FROM admin_events WHERE kind='active' AND created_at>=now()-interval '7 days'),
   'active_devices_prev_7d',(SELECT count(DISTINCT install_id) FROM admin_events WHERE kind='active' AND created_at>=now()-interval '14 days' AND created_at<now()-interval '7 days'),
   'platform_active_7d',(SELECT COALESCE(json_object_agg(platform,devices ORDER BY devices DESC),'{}'::json) FROM (
     SELECT `+overviewPlatform("platform")+` AS platform,count(DISTINCT install_id) AS devices FROM admin_events WHERE kind='active' AND install_id IS NOT NULL AND created_at>=now()-interval '7 days' GROUP BY 1
   ) p),
   'crash_free_rate',(SELECT round(count(*) FILTER (WHERE kind='session')::numeric/NULLIF(count(*),0),4) FROM admin_events WHERE kind IN ('session','session_crash') AND created_at>=now()-interval '7 days'),
   'crash_free_rate_prev',(SELECT round(count(*) FILTER (WHERE kind='session')::numeric/NULLIF(count(*),0),4) FROM admin_events WHERE kind IN ('session','session_crash') AND created_at>=now()-interval '14 days' AND created_at<now()-interval '7 days'),
   'crash_top',(SELECT json_build_object('platform',platform,'version',version,'crashes',crashes) FROM (
     SELECT `+overviewPlatform("platform")+` AS platform,version,count(*) AS crashes FROM admin_events WHERE kind='session_crash' AND created_at>=now()-interval '7 days' GROUP BY 1,2 ORDER BY 3 DESC,1,2 LIMIT 1
   ) c),
   'crash_group_latest',(SELECT json_build_object('signature',signature,'platform',`+overviewPlatform("platform")+`,'title',title) FROM admin_crash_groups WHERE status='open' AND first_seen>=now()-interval '7 days' ORDER BY first_seen DESC,signature LIMIT 1),
   'pending',json_build_object(
     'community',$2::int,
     'reports_7d',(SELECT count(*) FROM community_reports WHERE created_at>=now()-interval '7 days'),
     'crash_groups',(SELECT count(*) FROM admin_crash_groups WHERE status='open' AND first_seen>=now()-interval '7 days')),
   'services_configured',$6::bool,
   'services',(SELECT COALESCE(json_agg(json_build_object('key',s.key,'name',s.name,'provider',s.provider,
       'state',CASE WHEN today.total_minutes>0 AND today.ok_minutes=0 THEN 'down'
         WHEN EXISTS(SELECT 1 FROM admin_incidents i WHERE i.service=s.key AND i.state='open') THEN 'degraded'
         WHEN today.total_minutes IS NULL OR today.total_minutes=0 THEN 'unknown'
         WHEN today.degraded THEN 'degraded' ELSE 'ok' END,
       'uptime_60d',(SELECT round(sum(ok_minutes)::numeric*100/NULLIF(sum(total_minutes),0),2) FROM admin_service_daily WHERE service=s.key AND day>=(now() AT TIME ZONE 'UTC')::date-59),
       'p95_ms',today.p95_ms) ORDER BY s.ord),'[]'::json)
     FROM (
       SELECT key,name,provider,ord FROM unnest($3::text[],$4::text[],$5::text[]) WITH ORDINALITY AS c(key,name,provider,ord)
       UNION ALL
       SELECT service,service,'',1000+row_number() OVER (ORDER BY service) FROM (SELECT DISTINCT service FROM admin_service_daily WHERE day>=(now() AT TIME ZONE 'UTC')::date-59) d WHERE cardinality($3::text[])=0
     ) s
     LEFT JOIN LATERAL (SELECT ok_minutes,total_minutes,degraded,p95_ms FROM admin_service_daily WHERE service=s.key AND day>=(now() AT TIME ZONE 'UTC')::date-1 ORDER BY day DESC LIMIT 1) today ON true)) FROM (`+communityResourceCountsQuery+`) resource_counts`,
		days, community, keys, names, providers, len(a.admin.Services) > 0).Scan(&result)
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, result)
}
