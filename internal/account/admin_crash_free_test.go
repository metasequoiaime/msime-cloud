package account

import (
	"context"
	"testing"
)

// seedCrashFreeFixture 写入覆盖会话漏报和崩溃循环的合成遥测，全部是合成数据：
//
//   - macOS 0.52.0：4 台活跃设备只有 1 次正常会话，20 次崩溃会话都来自同一台设备，模拟会话漏报加崩溃循环，按规则数据不足。
//   - macOS 0.53.0：2 台活跃设备 1 次正常会话，正好等于 0.5 的门槛，数据充分；其中一台以 Darwin 别名上报。
//   - macOS 0.54.0：10 台活跃设备 4 次正常会话、1 次崩溃会话，崩溃会话少于正常会话但仍是漏报，按规则数据不足。
//   - Linux 0.10.1：3 台活跃设备 2 次正常会话，12 次崩溃会话都来自同一台设备（崩溃循环）；正常会话足够，比率照常计入，不因 session_crash 多于 session 被隐藏。
//   - iOS 1.0：6 次正常会话，没有该版本的 active 事件，无从判断，按数据充分计入；该设备的 active 以旧版本 0.9 上报。
//   - Android 0.4.0：4 台活跃设备 1 次正常会话，平台内唯一的版本数据不足，平台比率为 null。
//   - Windows：只有活跃和崩溃，没有会话；一台未上报 active 的设备近 7 天崩溃，另一台的崩溃在 8 天前。
//   - 前 7 天窗口：macOS 0.52.0 数据不足，Linux 0.10.0 有 3 次正常会话、1 次崩溃会话。
//   - 系统强制终止：iOS 唯一的活跃设备只有三条 SIGKILL 类诊断（旧格式的 0xdead10cc、旧格式的裸 signal 9、新格式的 Killed 前缀），不算崩溃设备；macOS 那台崩溃循环的设备另有一条看门狗终止，崩溃和强制终止各算一次。
func seedCrashFreeFixture(t *testing.T, a *Service) {
	t.Helper()
	if _, err := a.store.pool.Exec(context.Background(), `INSERT INTO admin_events(id,kind,platform,version,message,install_id,created_at)
 SELECT 'cf-act-mac52-'||n,'active','macos','0.52.0','','cf-device-mac-'||lpad(n::text,4,'0'),now()-interval '1 hour' FROM generate_series(1,4) n
 UNION ALL SELECT 'cf-ses-mac52-1','session','macos','0.52.0','','cf-device-mac-0002',now()-interval '1 hour'
 UNION ALL SELECT 'cf-sc-mac52-'||n,'session_crash','macos','0.52.0','','cf-device-mac-0001',now()-n*interval '1 minute' FROM generate_series(1,20) n
 UNION ALL SELECT 'cf-act-mac53-'||n,'active',CASE WHEN n=6 THEN 'Darwin' ELSE 'macos' END,'0.53.0','','cf-device-mac-'||lpad(n::text,4,'0'),now()-interval '2 hours' FROM generate_series(5,6) n
 UNION ALL SELECT 'cf-ses-mac53-1','session','Darwin','0.53.0','','cf-device-mac-0005',now()-interval '2 hours'
 UNION ALL SELECT 'cf-sc-mac53-1','session_crash','macos','0.53.0','','cf-device-mac-0006',now()-interval '2 hours'
 UNION ALL SELECT 'cf-act-mac54-'||n,'active','macos','0.54.0','','cf-device-mac-'||lpad(n::text,4,'0'),now()-interval '3 hours' FROM generate_series(7,16) n
 UNION ALL SELECT 'cf-ses-mac54-'||n,'session','macos','0.54.0','','cf-device-mac-'||lpad((n+6)::text,4,'0'),now()-interval '3 hours' FROM generate_series(1,4) n
 UNION ALL SELECT 'cf-sc-mac54-1','session_crash','macos','0.54.0','','cf-device-mac-0007',now()-interval '3 hours'
 UNION ALL SELECT 'cf-crash-mac-'||n,'crash','macos','0.52.0','合成崩溃','cf-device-mac-0001',now()-n*interval '1 minute' FROM generate_series(1,5) n
 UNION ALL SELECT 'cf-act-lin101-'||n,'active','linux','0.10.1','','cf-device-lin-'||lpad(n::text,4,'0'),now()-interval '1 hour' FROM generate_series(1,3) n
 UNION ALL SELECT 'cf-ses-lin101-'||n,'session','linux','0.10.1','','cf-device-lin-'||lpad(n::text,4,'0'),now()-interval '1 hour' FROM generate_series(2,3) n
 UNION ALL SELECT 'cf-sc-lin101-'||n,'session_crash','linux','0.10.1','','cf-device-lin-0001',now()-n*interval '1 minute' FROM generate_series(1,12) n
 UNION ALL SELECT 'cf-crash-lin-'||n,'crash','linux','0.10.1','合成崩溃','cf-device-lin-0001',now()-n*interval '1 minute' FROM generate_series(1,12) n
 UNION ALL SELECT 'cf-act-ios-1','active','ios','0.9','','cf-device-ios-0001',now()-interval '1 hour'
 UNION ALL SELECT 'cf-kill-ios-1','crash','ios','1.0','Crash exception 10 code 0 signal 9 Namespace RUNNINGBOARD, Code 0xdead10cc','cf-device-ios-0001',now()-interval '1 hour'
 UNION ALL SELECT 'cf-kill-ios-2','crash','iPadOS','1.0','Crash exception 10 code 0 signal 9','cf-device-ios-0001',now()-interval '2 hours'
 UNION ALL SELECT 'cf-kill-ios-3','crash','ios','1.0','Killed RUNNINGBOARD 0xdead10cc','cf-device-ios-0001',now()-interval '3 hours'
 UNION ALL SELECT 'cf-kill-mac-1','crash','macos','0.52.0','Killed by watchdog','cf-device-mac-0001',now()-interval '1 hour'
 UNION ALL SELECT 'cf-ses-ios-'||n,'session',CASE WHEN n%2=0 THEN 'iPadOS' ELSE 'ios' END,'1.0','','cf-device-ios-0001',now()-interval '1 hour' FROM generate_series(1,6) n
 UNION ALL SELECT 'cf-act-and-'||n,'active','android','0.4.0','','cf-device-and-'||lpad(n::text,4,'0'),now()-interval '1 hour' FROM generate_series(1,4) n
 UNION ALL SELECT 'cf-ses-and-1','session','android','0.4.0','','cf-device-and-0001',now()-interval '1 hour'
 UNION ALL SELECT 'cf-sc-and-'||n,'session_crash','android','0.4.0','','cf-device-and-0002',now()-interval '1 hour' FROM generate_series(1,2) n
 UNION ALL SELECT 'cf-act-win-'||n,'active','windows','0.3.0','','cf-device-win-'||lpad(n::text,4,'0'),now()-interval '1 hour' FROM generate_series(1,2) n
 UNION ALL SELECT 'cf-crash-win-old','crash','windows','0.3.0','合成崩溃','cf-device-win-0002',now()-interval '8 days'
 UNION ALL SELECT 'cf-crash-win-new','crash','win','0.3.0','合成崩溃','cf-device-win-0003',now()-interval '1 hour'
 UNION ALL SELECT 'cf-prev-act-mac','active','macos','0.52.0','','cf-device-mac-0001',now()-interval '9 days'
 UNION ALL SELECT 'cf-prev-sc-mac-'||n,'session_crash','macos','0.52.0','','cf-device-mac-0001',now()-interval '9 days' FROM generate_series(1,3) n
 UNION ALL SELECT 'cf-prev-act-lin','active','linux','0.10.0','','cf-device-lin-0001',now()-interval '9 days'
 UNION ALL SELECT 'cf-prev-ses-lin-'||n,'session','linux','0.10.0','','cf-device-lin-0001',now()-interval '9 days' FROM generate_series(1,3) n
 UNION ALL SELECT 'cf-prev-sc-lin-1','session_crash','linux','0.10.0','','cf-device-lin-0001',now()-interval '9 days'`); err != nil {
		t.Fatal(err)
	}
}

func rateIs(got *float64, want float64) bool { return got != nil && *got == want }

func TestOverviewCrashFreeRateExcludesUnderReportedVersions(t *testing.T) {
	a := overviewTestService(t)
	seedCrashFreeFixture(t, a)
	body := getOverview(t, a, "?days=7")
	// 计入的版本：macOS 0.53.0（1/2）、Linux 0.10.1（2/14）、iOS 1.0（6/6），合计 9/22。不排除数据不足的版本时是 14/48≈0.2917。
	if !rateIs(body.CrashFreeRate, 0.4091) {
		t.Fatalf("crash_free_rate %v", body.CrashFreeRate)
	}
	// 前 7 天只有 Linux 0.10.0 计入：3/4；macOS 0.52.0 只有崩溃会话、1 台活跃设备，数据不足。
	if !rateIs(body.CrashFreeRatePrev, 0.75) {
		t.Fatalf("crash_free_rate_prev %v", body.CrashFreeRatePrev)
	}
	// macOS 0.52.0 的崩溃会话最多但数据不足，不作为拖累版本。
	if top := body.CrashTop; top == nil || top.Platform != "linux" || top.Version != "0.10.1" || top.Crashes != 12 {
		t.Fatalf("crash_top %+v", body.CrashTop)
	}
	// 崩溃设备：macOS 1 台（崩溃 5 次只算一次）、Linux 1 台（崩溃循环 12 次只算一次）、Windows 1 台（8 天前的不算），iOS 只有系统强制终止，不算；活跃设备 16+3+1+4+2=26。
	if body.ActiveDevices7d != 26 || body.CrashDevices7d != 3 || !rateIs(body.DeviceCrashRate, 0.1154) || body.ForcedTerminationDevices7d != 2 {
		t.Fatalf("devices active=%d crashed=%d rate=%v forced=%d", body.ActiveDevices7d, body.CrashDevices7d, body.DeviceCrashRate, body.ForcedTerminationDevices7d)
	}
	f := func(v float64) *float64 { return &v }
	want := []overviewCrashPlatform{
		{Platform: "android", CrashFreeRate: nil, Sessions: 0, CrashedSessions: 0, Versions: 1, InsufficientVersions: 1, ActiveDevices7d: 4, CrashDevices7d: 0, DeviceCrashRate: f(0), ForcedTerminationRate: f(0)},
		{Platform: "ios", CrashFreeRate: f(1), Sessions: 6, CrashedSessions: 0, Versions: 1, InsufficientVersions: 0, ActiveDevices7d: 1, CrashDevices7d: 0, DeviceCrashRate: f(0), ForcedTerminationDevices7d: 1, ForcedTerminationRate: f(1)},
		{Platform: "linux", CrashFreeRate: f(0.1429), Sessions: 2, CrashedSessions: 12, Versions: 1, InsufficientVersions: 0, ActiveDevices7d: 3, CrashDevices7d: 1, DeviceCrashRate: f(0.3333), ForcedTerminationRate: f(0)},
		{Platform: "macos", CrashFreeRate: f(0.5), Sessions: 1, CrashedSessions: 1, Versions: 3, InsufficientVersions: 2, ActiveDevices7d: 16, CrashDevices7d: 1, DeviceCrashRate: f(0.0625), ForcedTerminationDevices7d: 1, ForcedTerminationRate: f(0.0625)},
		{Platform: "windows", CrashFreeRate: nil, Sessions: 0, CrashedSessions: 0, Versions: 0, InsufficientVersions: 0, ActiveDevices7d: 2, CrashDevices7d: 1, DeviceCrashRate: f(0.5), ForcedTerminationRate: f(0)},
	}
	if len(body.CrashPlatforms) != len(want) {
		t.Fatalf("crash_platforms %+v", body.CrashPlatforms)
	}
	sameRate := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	for i, w := range want {
		g := body.CrashPlatforms[i]
		if g.Platform != w.Platform || !sameRate(g.CrashFreeRate, w.CrashFreeRate) || g.Sessions != w.Sessions || g.CrashedSessions != w.CrashedSessions || g.Versions != w.Versions || g.InsufficientVersions != w.InsufficientVersions || g.ActiveDevices7d != w.ActiveDevices7d || g.CrashDevices7d != w.CrashDevices7d || !sameRate(g.DeviceCrashRate, w.DeviceCrashRate) || g.ForcedTerminationDevices7d != w.ForcedTerminationDevices7d || !sameRate(g.ForcedTerminationRate, w.ForcedTerminationRate) {
			t.Fatalf("crash_platforms[%d] = %+v，期望 %+v", i, g, w)
		}
	}
}

func TestCrashSummarySessionRateByVersion(t *testing.T) {
	a := &Service{store: testStore(t)}
	resetCrashTables(t, a)
	seedCrashFreeFixture(t, a)

	all := getCrashGroups(t, a, "", 200).Summary
	// 影响设备统计全部 crash 事件（4 台），崩溃设备占比只算真正崩溃的 3 台；macOS 那台两种都有。
	if all.CrashDevices7d != 3 || all.ForcedTerminationDevices7d != 2 {
		t.Fatalf("全部平台崩溃与强制终止设备 %+v", all)
	}
	if !rateIs(all.CrashFreeRate, 0.4091) || all.Devices7d == nil || *all.Devices7d != 4 || all.ActiveDevices7d == nil || *all.ActiveDevices7d != 26 || !rateIs(all.DeviceCrashRate, 0.1154) || len(all.Versions) != 6 {
		t.Fatalf("全部平台汇总 %+v", all)
	}

	mac := getCrashGroups(t, a, "?platform=Darwin", 200).Summary
	if mac.CrashDevices7d != 1 || mac.ForcedTerminationDevices7d != 1 || !rateIs(mac.CrashFreeRate, 0.5) || mac.Devices7d == nil || *mac.Devices7d != 1 || mac.ActiveDevices7d == nil || *mac.ActiveDevices7d != 16 || !rateIs(mac.DeviceCrashRate, 0.0625) {
		t.Fatalf("macOS 汇总 %+v", mac)
	}
	// 按会话总数从多到少：0.52.0（21）、0.54.0（5）、0.53.0（2）；数据不足的版本不给比率。
	wantMac := []sessionVersionStat{
		{Platform: "macos", Version: "0.52.0", Sessions: 1, CrashedSessions: 20, ActiveDevices: 4, Insufficient: true},
		{Platform: "macos", Version: "0.54.0", Sessions: 4, CrashedSessions: 1, ActiveDevices: 10, Insufficient: true},
		{Platform: "macos", Version: "0.53.0", Sessions: 1, CrashedSessions: 1, ActiveDevices: 2, CrashFreeRate: new(0.5)},
	}
	if len(mac.Versions) != len(wantMac) {
		t.Fatalf("macOS 版本 %+v", mac.Versions)
	}
	for i, w := range wantMac {
		g := mac.Versions[i]
		if g.Platform != w.Platform || g.Version != w.Version || g.Sessions != w.Sessions || g.CrashedSessions != w.CrashedSessions || g.ActiveDevices != w.ActiveDevices || g.Insufficient != w.Insufficient || (g.CrashFreeRate == nil) != (w.CrashFreeRate == nil) || (g.CrashFreeRate != nil && *g.CrashFreeRate != *w.CrashFreeRate) {
			t.Fatalf("macOS 版本[%d] = %+v，期望 %+v", i, g, w)
		}
	}

	// 崩溃循环的版本 session_crash 多于 session，但正常会话足够，比率照常给出。
	linux := getCrashGroups(t, a, "?platform=linux", 200).Summary
	if !rateIs(linux.CrashFreeRate, 0.1429) || !rateIs(linux.DeviceCrashRate, 0.3333) || len(linux.Versions) != 1 || linux.Versions[0].Insufficient || !rateIs(linux.Versions[0].CrashFreeRate, 0.1429) {
		t.Fatalf("Linux 汇总 %+v", linux)
	}
	// 平台内所有版本都数据不足时比率为 null，但仍列出版本。
	android := getCrashGroups(t, a, "?platform=android", 200).Summary
	if android.CrashFreeRate != nil || len(android.Versions) != 1 || !android.Versions[0].Insufficient || android.Versions[0].CrashFreeRate != nil || !rateIs(android.DeviceCrashRate, 0) {
		t.Fatalf("Android 汇总 %+v", android)
	}
	// 没有会话的平台：比率为 null，版本列表为空数组；未上报 active 的设备的崩溃也计入分子。
	windows := getCrashGroups(t, a, "?platform=win", 200).Summary
	if windows.CrashFreeRate != nil || windows.Versions == nil || len(windows.Versions) != 0 || windows.Devices7d == nil || *windows.Devices7d != 1 || !rateIs(windows.DeviceCrashRate, 0.5) {
		t.Fatalf("Windows 汇总 %+v", windows)
	}
	// iOS 只有系统强制终止：影响设备 1 台，崩溃设备占比为 0。
	ios := getCrashGroups(t, a, "?platform=iPadOS", 200).Summary
	if ios.Devices7d == nil || *ios.Devices7d != 1 || ios.CrashDevices7d != 0 || ios.ForcedTerminationDevices7d != 1 || !rateIs(ios.DeviceCrashRate, 0) {
		t.Fatalf("iOS 汇总 %+v", ios)
	}
	// 没有任何活跃设备的平台没有崩溃设备占比。
	harmony := getCrashGroups(t, a, "?platform=ohos", 200).Summary
	if harmony.ActiveDevices7d != nil || harmony.DeviceCrashRate != nil || harmony.CrashFreeRate != nil || len(harmony.Versions) != 0 {
		t.Fatalf("HarmonyOS 汇总 %+v", harmony)
	}
}

// TestCrashForcedTerminationPatterns 固定系统强制终止的判定。用例取自 2026-10 生产 `crash` 事件的摘要形状（数字是实际出现的取值，不含任何用户数据）：iOS 旧版客户端把 MetricKit 诊断报成 `Crash exception <类型> code <代码> signal <信号> ...`，其中 signal 9 是 SIGKILL；其他平台常见的崩溃摘要都不应被当成强制终止。
func TestCrashForcedTerminationPatterns(t *testing.T) {
	db := testStore(t)
	cases := []struct {
		message string
		forced  bool
	}{
		// iOS 旧格式：RunningBoard 因挂起时持有文件锁终止、没有命名空间的 SIGKILL、看门狗。
		{"Crash exception 10 code 0 signal 9 Namespace RUNNINGBOARD, Code 0xdead10cc", true},
		{"Crash exception 10 code 0 signal 9", true},
		{"Crash exception 10 code 0 signal 9 <RBSTerminateContext| domain:10 code:0x8BADF00D explanation:scene-update watchdog>", true},
		// metasequoiaime/msime#9611 起的新前缀，以及明确写出 SIGKILL、0xdead10cc、看门狗的摘要。
		{"Killed RUNNINGBOARD 0xdead10cc", true},
		{"Killed: watchdog timeout", true},
		{"EXC_CRASH (SIGKILL)", true},
		{"Termination Reason: RUNNINGBOARD 0xDEAD10CC", true},
		{"Watchdog terminated the keyboard extension", true},
		// iOS 真正的崩溃：SIGABRT（signal 6）、SIGTRAP（signal 5）、Objective-C 异常。
		{"Crash exception 10 code 0 signal 6", false},
		{"Crash exception 10 code 0 signal 6 Namespace SIGNAL, Code 0x6", false},
		{"Crash exception 6 code 1 signal 5 Namespace SIGNAL, Code 0x5", false},
		{"*** Terminating app due to uncaught exception 'NSInvalidArgumentException'", false},
		// 其他平台生产中出现的崩溃摘要。
		{"Fatal signal SIGTRAP", false},
		{"Fatal signal SIGILL", false},
		{"SIGABRT: aborted", false},
		{"SIGSEGV: segmentation fault (code 1)", false},
		{"std::terminate: nlohmann::json_abi_v3_11_3::detail::type_error 302", false},
		{"EXCEPTION_ACCESS_VIOLATION (0xc0000005) in libcurl.dll+0x1a2b3", false},
		{"java.lang.NullPointerException: Attempt to invoke virtual method 'void android.view.View.setVisibility(int)'", false},
		// 只认完整的信号号和关键词。
		{"Crash exception 10 code 0 signal 90", false},
		{"deadline 0xdead10cce exceeded", false},
		{"", false},
	}
	messages := make([]string, len(cases))
	for i, c := range cases {
		messages[i] = c.message
	}
	rows, err := db.pool.Query(t.Context(), `SELECT `+crashForcedTerminationSQL("m")+` FROM unnest($1::text[]) WITH ORDINALITY AS u(m,i) ORDER BY i`, messages)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var forced bool
		if err := rows.Scan(&forced); err != nil {
			t.Fatal(err)
		}
		if forced != cases[i].forced {
			t.Errorf("%q 判定为强制终止=%v，期望 %v", cases[i].message, forced, cases[i].forced)
		}
		i++
	}
	if rows.Err() != nil || i != len(cases) {
		t.Fatalf("读取 %d/%d 行：%v", i, len(cases), rows.Err())
	}
}
