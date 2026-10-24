package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

type overviewBody struct {
	Downloads        int   `json:"downloads"`
	Downloads30d     int   `json:"downloads_30d"`
	DownloadsPrev30d int   `json:"downloads_prev_30d"`
	RangeDays        int   `json:"range_days"`
	Daily            []any `json:"daily"`
	EmailDelivery    []struct {
		Day       string `json:"day"`
		Attempted int    `json:"attempted"`
		Accepted  int    `json:"accepted"`
		Rejected  int    `json:"rejected"`
		Failed    int    `json:"failed"`
	} `json:"email_delivery"`
	Telemetry struct {
		Active   bool `json:"active"`
		Sessions bool `json:"sessions"`
	} `json:"telemetry"`
	ActiveDevicesDaily []struct {
		Day       string `json:"day"`
		Windows   int    `json:"windows"`
		MacOS     int    `json:"macos"`
		Linux     int    `json:"linux"`
		Android   int    `json:"android"`
		IOS       int    `json:"ios"`
		HarmonyOS int    `json:"harmonyos"`
		MacLinux  int    `json:"mac_linux"`
		Mobile    int    `json:"mobile"`
	} `json:"active_devices_daily"`
	ActiveDevices7d     int            `json:"active_devices_7d"`
	ActiveDevicesPrev7d int            `json:"active_devices_prev_7d"`
	PlatformActive7d    map[string]int `json:"platform_active_7d"`
	CrashFreeRate       *float64       `json:"crash_free_rate"`
	CrashFreeRatePrev   *float64       `json:"crash_free_rate_prev"`
	CrashTop            *struct {
		Platform string `json:"platform"`
		Version  string `json:"version"`
		Crashes  int    `json:"crashes"`
	} `json:"crash_top"`
	CrashGroupLatest *struct {
		Signature string `json:"signature"`
		Platform  string `json:"platform"`
		Title     string `json:"title"`
	} `json:"crash_group_latest"`
	CrashDevices7d     int                     `json:"crash_devices_7d"`
	DeviceCrashRate    *float64                `json:"device_crash_rate"`
	CrashPlatforms     []overviewCrashPlatform `json:"crash_platforms"`
	Pending            map[string]int          `json:"pending"`
	ServicesConfigured bool                    `json:"services_configured"`
	Services           []overviewService       `json:"services"`
}

type overviewService struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Provider  string   `json:"provider"`
	State     string   `json:"state"`
	Uptime60d *float64 `json:"uptime_60d"`
	P95MS     *int     `json:"p95_ms"`
}

type overviewCrashPlatform struct {
	Platform             string   `json:"platform"`
	CrashFreeRate        *float64 `json:"crash_free_rate"`
	Sessions             int      `json:"sessions"`
	CrashedSessions      int      `json:"crashed_sessions"`
	Versions             int      `json:"versions"`
	InsufficientVersions int      `json:"insufficient_versions"`
	ActiveDevices7d      int      `json:"active_devices_7d"`
	CrashDevices7d       int      `json:"crash_devices_7d"`
	DeviceCrashRate      *float64 `json:"device_crash_rate"`
}

func overviewTestService(t *testing.T) *Service {
	t.Helper()
	db := testStore(t)
	if _, err := db.pool.Exec(context.Background(), `TRUNCATE admin_events,admin_crash_groups,admin_service_daily,admin_incidents,admin_email_delivery_daily CASCADE`); err != nil {
		t.Fatal(err)
	}
	return &Service{store: db}
}

func getOverview(t *testing.T, a *Service, query string) overviewBody {
	t.Helper()
	w := httptest.NewRecorder()
	a.AdminHTTP(w, adminJSONRequest("GET", "/api/overview"+query, ""))
	var body overviewBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	return body
}

func TestOverviewDaysQuery(t *testing.T) {
	a := overviewTestService(t)
	for _, tc := range []struct {
		query string
		days  int
	}{
		{"", 30}, {"?days=", 30}, {"?days=&days=bad", 30},
		{"?days=7&days=bad", 7}, {"?days=%2B7", 7}, {"?days=030", 30},
		{"?days=8", 0}, {"?days=29", 0}, {"?days=0", 0}, {"?days=-7", 0},
		{"?days=%207", 0}, {"?days=7%20", 0}, {"?days=bad&days=7", 0},
		{"?days=999999999999999999999999999999", 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if tc.days != 0 {
				body := getOverview(t, a, tc.query)
				if body.RangeDays != tc.days || len(body.Daily) != tc.days {
					t.Fatalf("range_days=%d daily=%d，期望 %d", body.RangeDays, len(body.Daily), tc.days)
				}
				return
			}
			w := httptest.NewRecorder()
			a.AdminHTTP(w, adminJSONRequest("GET", "/api/overview"+tc.query, ""))
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_days"`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestOverviewWithoutClientTelemetry(t *testing.T) {
	a := overviewTestService(t)
	body := getOverview(t, a, "?days=7")
	if body.Telemetry.Active || body.Telemetry.Sessions || body.CrashFreeRate != nil || body.CrashTop != nil || body.CrashGroupLatest != nil || body.ActiveDevices7d != 0 || len(body.PlatformActive7d) != 0 || body.CrashDevices7d != 0 || body.DeviceCrashRate != nil || body.CrashPlatforms == nil || len(body.CrashPlatforms) != 0 {
		t.Fatalf("telemetry without events %+v", body)
	}
	if len(body.ActiveDevicesDaily) != 7 || len(body.Daily) != 7 || len(body.EmailDelivery) != 7 || body.RangeDays != 7 {
		t.Fatalf("series length %+v", body)
	}
	for _, day := range body.ActiveDevicesDaily {
		if day.Windows+day.MacOS+day.Linux+day.Android+day.IOS+day.HarmonyOS+day.MacLinux+day.Mobile != 0 || len(day.Day) != 10 {
			t.Fatalf("empty day %+v", day)
		}
	}
	for _, day := range body.EmailDelivery {
		if day.Attempted+day.Accepted+day.Rejected+day.Failed != 0 || len(day.Day) != 10 {
			t.Fatalf("空邮件统计日错误 %+v", day)
		}
	}
	if body.ServicesConfigured || len(body.Services) != 0 || body.Pending["community"] != 0 || body.Pending["crash_groups"] != 0 {
		t.Fatalf("services/pending %+v", body)
	}
}

func TestOverviewTelemetryAndServices(t *testing.T) {
	a := overviewTestService(t)
	ctx := context.Background()
	a.ConfigureAdmin(AdminSettings{Services: []AdminService{{Key: "cloud", Name: "云候选代理", Provider: "腾讯云"}, {Key: "chat", Name: "AI 联想", Provider: "OpenAI"}, {Key: "translation", Name: "翻译"}, {Key: "stt", Name: "语音识别"}}})
	if _, err := a.store.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,install_id,created_at) VALUES
 ('a1','active','Windows','1','device-windows-0001',now()),
 ('a2','active','windows','1','device-windows-0001',now()-interval '1 hour'),
 ('a3','active','windows','1','device-windows-0002',now()),
 ('a4','active','macos','1','device-macos-00001',now()),
 ('a5','active','linux','1','device-linux-00001',now()),
 ('a6','active','ios','1','device-ios-0000001',now()),
 ('a7','active','harmonyos','1','device-harmony-001',now()-interval '2 days'),
 ('a8','active','android','1','device-android-001',now()-interval '10 days'),
 ('a9','active','Darwin','1','device-darwin-0001',now()),
 ('a10','active','OHOS','1','device-ohos-00001',now()),
 ('a11','active','win','1','device-windows-0001',now()),
 ('s1','session','ios','1',NULL,now()),('s2','session','ios','1',NULL,now()),('s3','session','ios','1',NULL,now()),('s4','session_crash','ios','1',NULL,now()),
 ('s6','session_crash','Darwin','2',NULL,now()),('s7','session_crash','macos','2',NULL,now()-interval '1 day'),('s8','session_crash','android','3',NULL,now()-interval '9 days'),('s9','session_crash','android','3',NULL,now()-interval '9 days'),
 ('s5','session','ios','1',NULL,now()-interval '8 days'),
 ('d1','download','windows','1',NULL,now()),('d2','download','windows','1',NULL,now()-interval '40 days'),('d3','download','windows','1',NULL,now()-interval '45 days');
INSERT INTO admin_crash_groups(signature,platform,version,title,first_seen) VALUES('0123456789abcdef','ios','1','new',now()),('0123456789abcde2','Darwin','2','older new',now()-interval '1 day'),('0123456789abcde0','ios','1','old',now()-interval '30 days');
INSERT INTO admin_crash_groups(signature,platform,version,title,status) VALUES('0123456789abcde1','ios','1','known','known');
INSERT INTO admin_service_daily(service,day,ok_minutes,total_minutes,degraded,p95_ms) VALUES
 ('cloud',current_date,600,600,false,42),('cloud',current_date-1,1380,1440,false,40),
 ('chat',current_date,500,600,true,1800),
 ('stt',current_date,0,30,true,NULL),
 ('stale',current_date,10,10,false,1);
INSERT INTO admin_email_delivery_daily(day,attempted,accepted,rejected,failed) VALUES
 ((now() AT TIME ZONE 'UTC')::date,12,8,3,1),
 ((now() AT TIME ZONE 'UTC')::date-1,4,4,0,0);
INSERT INTO admin_incidents(service,title) VALUES('translation','翻译超时'),('stt','语音识别中断')`); err != nil {
		t.Fatal(err)
	}
	body := getOverview(t, a, "")
	if !body.Telemetry.Active || !body.Telemetry.Sessions || body.RangeDays != 30 || len(body.ActiveDevicesDaily) != 30 {
		t.Fatalf("flags %+v", body)
	}
	today := body.ActiveDevicesDaily[29]
	if today.Windows != 2 || today.MacOS != 2 || today.Linux != 1 || today.Android != 0 || today.IOS != 1 || today.HarmonyOS != 1 || today.MacLinux != 3 || today.Mobile != 2 {
		t.Fatalf("today %+v", today)
	}
	if body.ActiveDevices7d != 8 || body.ActiveDevicesPrev7d != 1 {
		t.Fatalf("7d %d prev %d", body.ActiveDevices7d, body.ActiveDevicesPrev7d)
	}
	// Aliases (win, Darwin, OHOS) fold into the canonical keys the daily groups use.
	wantPlatforms := map[string]int{"windows": 2, "macos": 2, "linux": 1, "ios": 1, "harmonyos": 2}
	if len(body.PlatformActive7d) != len(wantPlatforms) {
		t.Fatalf("platforms %+v", body.PlatformActive7d)
	}
	for platform, n := range wantPlatforms {
		if body.PlatformActive7d[platform] != n {
			t.Fatalf("platforms %+v", body.PlatformActive7d)
		}
	}
	if body.CrashFreeRate == nil || *body.CrashFreeRate != 0.5 || body.CrashFreeRatePrev == nil || *body.CrashFreeRatePrev != 0.3333 {
		t.Fatalf("crash free %v %v", body.CrashFreeRate, body.CrashFreeRatePrev)
	}
	if body.Downloads != 3 || body.Downloads30d != 1 || body.DownloadsPrev30d != 2 {
		t.Fatalf("downloads %+v", body)
	}
	if len(body.EmailDelivery) != 30 || body.EmailDelivery[29].Attempted != 12 || body.EmailDelivery[29].Accepted != 8 || body.EmailDelivery[29].Rejected != 3 || body.EmailDelivery[29].Failed != 1 || body.EmailDelivery[28].Attempted != 4 {
		t.Fatalf("邮件投递统计 %+v", body.EmailDelivery)
	}
	// The macOS aliases fold together and outrank iOS; the older Android crashes are outside the 7-day window.
	if top := body.CrashTop; top == nil || top.Platform != "macos" || top.Version != "2" || top.Crashes != 2 {
		t.Fatalf("crash top %+v", body.CrashTop)
	}
	if latest := body.CrashGroupLatest; latest == nil || latest.Signature != "0123456789abcdef" || latest.Platform != "ios" || latest.Title != "new" {
		t.Fatalf("crash group latest %+v", body.CrashGroupLatest)
	}
	if body.Pending["crash_groups"] != 2 {
		t.Fatalf("pending %+v", body.Pending)
	}
	if !body.ServicesConfigured || len(body.Services) != 4 {
		t.Fatalf("services %+v", body.Services)
	}
	cloud, chat, translation := body.Services[0], body.Services[1], body.Services[2]
	if cloud.Key != "cloud" || cloud.Name != "云候选代理" || cloud.Provider != "腾讯云" || cloud.State != "ok" || cloud.P95MS == nil || *cloud.P95MS != 42 || cloud.Uptime60d == nil || *cloud.Uptime60d != 97.06 {
		t.Fatalf("cloud %+v", cloud)
	}
	if chat.State != "degraded" || translation.State != "degraded" || translation.Uptime60d != nil || translation.P95MS != nil {
		t.Fatalf("chat %+v translation %+v", chat, translation)
	}
	// A service with no successful minute today is down even while an incident is open for it.
	if stt := body.Services[3]; stt.State != "down" || stt.Uptime60d == nil || *stt.Uptime60d != 0 || stt.P95MS != nil {
		t.Fatalf("stt %+v", stt)
	}
	// Services derived from the deployment are listed under their names, in their order, without counting as configured.
	a.ConfigureAdmin(AdminSettings{DerivedServices: []AdminService{{Key: "chat", Name: "AI 联想", Provider: "api.example.com"}, {Key: "images", Name: "皮肤生成"}}})
	named := getOverview(t, a, "?days=7")
	if named.ServicesConfigured || len(named.Services) != 2 || named.Services[0].Key != "chat" || named.Services[0].Name != "AI 联想" || named.Services[0].Provider != "api.example.com" || named.Services[0].State != "degraded" || named.Services[1].Key != "images" || named.Services[1].State != "unknown" {
		t.Fatalf("named %+v", named.Services)
	}
	// Without configured or derived services the overview lists the services the status probe recorded.
	a.ConfigureAdmin(AdminSettings{})
	derived := getOverview(t, a, "?days=7")
	if derived.ServicesConfigured || len(derived.Services) != 4 || derived.Services[0].Key != "chat" || derived.Services[3].Key != "stt" || derived.Services[2].Key != "stale" || derived.Services[2].Name != "stale" {
		t.Fatalf("derived %+v", derived.Services)
	}
}

// 后台概览的社区作品和下载计数排除已下架内容，与公共社区统计保持一致。
func TestOverviewCommunityCountersSkipRemoved(t *testing.T) {
	a := overviewTestService(t)
	ctx := context.Background()
	user := complete(t, a.store, Identity{"email", "overview-owner@example.test"})
	for _, statement := range []string{
		`INSERT INTO community_skins(id,owner_id,name,design,moderation) VALUES('overview-skin-1',$1,'A','{}','approved'),('overview-skin-2',$1,'B','{}','removed')`,
		`INSERT INTO community_skin_downloads(skin_id,user_id) VALUES('overview-skin-1',$1),('overview-skin-2',$1)`,
		`INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES('overview-dict-1',$1,'dictionary','D','{"entries":[]}','approved'),('overview-reply-1',$1,'reply','R','{"prompt":"hi"}','removed')`,
	} {
		if _, err := a.store.pool.Exec(ctx, statement, user.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	var counts struct {
		Skins         int `json:"skins"`
		SkinDownloads int `json:"skin_downloads"`
		Dictionaries  int `json:"dictionaries"`
		Replies       int `json:"replies"`
	}
	r := httptest.NewRequest("GET", "/api/overview", nil)
	r = r.WithContext(adminTestContext(r.Context(), "legacy-token"))
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &counts) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	var stats CommunityStats
	if err := a.store.pool.QueryRow(ctx, communityStatsQuery).Scan(&stats.Skins, &stats.SkinDownloads, &stats.Dictionaries, &stats.Replies, &stats.ResourceSaves, &stats.GeneratedAt); err != nil {
		t.Fatal(err)
	}
	if counts.Skins != int(stats.Skins) || counts.SkinDownloads != int(stats.SkinDownloads) || counts.Dictionaries != int(stats.Dictionaries) || counts.Replies != int(stats.Replies) || counts.Skins != 1 || counts.SkinDownloads != 1 || counts.Dictionaries != 1 || counts.Replies != 0 {
		t.Fatalf("overview %+v stats %+v", counts, stats)
	}
}

// Stored days are UTC, so the 60-day window and today's row must be UTC days whatever the database session's time zone is. Between them, these two zones put the local date on another day than UTC at every hour.
func TestOverviewServiceDaysAreUTC(t *testing.T) {
	overviewTestService(t)
	for _, zone := range []string{"Etc/GMT-14", "Etc/GMT+12"} {
		db, err := Open(context.Background(), os.Getenv("MSIME_TEST_DATABASE_URL")+"&timezone="+url.QueryEscape(zone))
		if err != nil {
			t.Fatal(err)
		}
		a := &Service{store: db}
		a.ConfigureAdmin(AdminSettings{Services: []AdminService{{Key: "cloud", Name: "云候选代理"}}})
		ctx := context.Background()
		if _, err = db.pool.Exec(ctx, `TRUNCATE admin_service_daily;
INSERT INTO admin_service_daily(service,day,ok_minutes,total_minutes,degraded,p95_ms) VALUES
 ('cloud',(now() AT TIME ZONE 'UTC')::date,60,60,false,42),
 ('cloud',(now() AT TIME ZONE 'UTC')::date-59,40,40,false,40),
 ('cloud',(now() AT TIME ZONE 'UTC')::date-60,0,100,true,40)`); err != nil {
			t.Fatal(err)
		}
		body := getOverview(t, a, "")
		db.Close()
		if len(body.Services) != 1 || body.Services[0].Uptime60d == nil || *body.Services[0].Uptime60d != 100 || body.Services[0].P95MS == nil || *body.Services[0].P95MS != 42 {
			t.Fatalf("%s: %+v", zone, body.Services)
		}
	}
}
