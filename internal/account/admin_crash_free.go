package account

// 无崩溃会话率与崩溃设备占比（概览和崩溃页共用）。
//
// 无崩溃会话率按「平台 + 版本」分别计算 session/(session+session_crash)，平台和全局的比率只合计会话数据充分的版本。客户端曾经漏报正常结束的 `session`（macOS 输入法进程被 SIGTERM 结束时不补发，Linux IBus 共用会话标记），而 `session_crash` 照常上报，漏报版本的比率会被压得很低，所以要先把这些版本排除再合计。
//
// 判定规则：窗口内某版本的 `session` 数少于该版本去重活跃设备数（`active` 事件的 install_id）的 crashFreeMinSessionsPerDevice 倍，就标记为「会话数据不足」（insufficient），不计入平台和全局比率。只看正常结束的 `session`，不看 `session_crash`：漏报的正是 `session`；而用 session_crash>session 判定会把真正崩溃严重的版本当成数据不足藏起来，也漏掉 `session_crash` 很少但 `session` 同样缺失的版本。窗口内没有 `active` 事件的版本无从判断，按数据充分处理。代价是：所有设备整周都在崩溃循环、一次正常会话都没有的版本也会被标为数据不足，这种版本仍体现在崩溃设备占比和崩溃分组里。
//
// 崩溃设备占比按平台计算：近 7 天有 `crash` 事件的去重 install_id 数除以近 7 天 `active` 事件的去重 install_id 数。崩溃循环会让一台设备贡献成百上千次崩溃会话，这个指标每台设备只算一次。install_id 按安装随机生成，不跨平台，所以全局值直接合计各平台。

// crashFreeMinSessionsPerDevice 是版本被视为会话数据充分所需的每台活跃设备平均 `session` 数（窗口为 7 天）。2026-10 生产数据中会话上报正常的版本在 0.86 以上，漏报的 macOS 版本都低于 0.07。
const crashFreeMinSessionsPerDevice = "0.5"

// sessionVersionsSQL 按规范平台和版本统计 where 条件内的正常会话、崩溃会话和去重活跃设备，并给出 insufficient 标记。where 只能是本文件内的固定条件，不能拼接请求参数。
func sessionVersionsSQL(where string) string {
	return `SELECT platform,version,sessions,crashed,active,active>0 AND sessions<active*` + crashFreeMinSessionsPerDevice + ` AS insufficient FROM (
 SELECT ` + overviewPlatform("platform") + ` AS platform,version,
  count(*) FILTER (WHERE kind='session') AS sessions,
  count(*) FILTER (WHERE kind='session_crash') AS crashed,
  count(DISTINCT install_id) FILTER (WHERE kind='active') AS active
 FROM admin_events WHERE kind IN ('active','session','session_crash') AND ` + where + ` GROUP BY 1,2) v`
}

// crashFreeRateSQL 是在 sessionVersionsSQL 结果上合计的无崩溃会话率，只计会话数据充分的版本；没有这样的会话时为 NULL。
const crashFreeRateSQL = `round(sum(sessions) FILTER (WHERE NOT insufficient)::numeric/NULLIF(sum(sessions+crashed) FILTER (WHERE NOT insufficient),0),4)::float8`

// crashDevicePlatformsSQL 按规范平台统计近 7 天的去重活跃设备和去重崩溃设备。
var crashDevicePlatformsSQL = `SELECT platform,count(DISTINCT install_id) FILTER (WHERE kind='active') AS active,count(DISTINCT install_id) FILTER (WHERE kind='crash') AS crashed FROM (
 SELECT ` + overviewPlatform("platform") + ` AS platform,kind,install_id FROM admin_events
 WHERE kind IN ('active','crash') AND install_id IS NOT NULL AND created_at>=now()-interval '7 days') e GROUP BY 1`

// sessionVersionJSON 把 sessionVersionsSQL 的一行转成接口返回的版本统计；数据不足的版本不给比率。
const sessionVersionJSON = `json_build_object('platform',platform,'version',version,'sessions',sessions,'crashed_sessions',crashed,'active_devices',active,
 'crash_free_rate',CASE WHEN insufficient THEN NULL ELSE round(sessions::numeric/NULLIF(sessions+crashed,0),4)::float8 END,'insufficient',insufficient)`

// sessionVersionStat 是崩溃页按版本列出的会话统计，字段含义见 sessionVersionsSQL。
type sessionVersionStat struct {
	Platform        string   `json:"platform"`
	Version         string   `json:"version"`
	Sessions        int64    `json:"sessions"`
	CrashedSessions int64    `json:"crashed_sessions"`
	ActiveDevices   int64    `json:"active_devices"`
	CrashFreeRate   *float64 `json:"crash_free_rate"`
	Insufficient    bool     `json:"insufficient"`
}
