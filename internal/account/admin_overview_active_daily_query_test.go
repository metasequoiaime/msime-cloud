package account

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminOverviewActiveDailyUsesBoundedScans(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events,admin_crash_groups,admin_service_daily,admin_incidents CASCADE;
 INSERT INTO admin_events(id,kind,platform,version,install_id,created_at)
 SELECT 'active-daily-'||n||'-'||age,'active',platform,'1',CASE WHEN n=12 THEN NULL WHEN n=11 THEN 'device-second-0001' ELSE 'device-shared-0001' END,
 (date_trunc('day',now() AT TIME ZONE 'UTC')-age*interval '1 day') AT TIME ZONE 'UTC'
 FROM unnest(ARRAY['Windows','win','macOS','Darwin','linux','android','ipados','ohos','harmony','unknown','windows','linux']) WITH ORDINALITY p(platform,n)
 CROSS JOIN unnest(ARRAY[-1,0,6,29,30]) age;
 INSERT INTO admin_events(id,kind,platform,version,install_id,created_at) VALUES('active-daily-download','download','windows','1','device-download-01',now());
 ANALYZE admin_events;`); err != nil {
		t.Fatal(err)
	}
	var today time.Time
	if err := db.pool.QueryRow(t.Context(), `SELECT date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&today); err != nil {
		t.Fatal(err)
	}
	for _, zone := range []string{"UTC", "Etc/GMT-14", "Australia/Sydney"} {
		t.Run(zone, func(t *testing.T) {
			trace := &overviewQueryTrace{queries: make(chan string, 1)}
			cfg := db.pool.Config()
			cfg.ConnConfig.RuntimeParams["timezone"] = zone
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a := &Service{store: &Store{pool: pool}}
			for _, days := range []int{7, 30} {
				body := getOverview(t, a, "?days="+url.QueryEscape(fmt.Sprint(days)))
				if len(body.ActiveDevicesDaily) != days {
					t.Fatalf("活跃设备序列长度错误：%d，期望 %d", len(body.ActiveDevicesDaily), days)
				}
				for i, day := range body.ActiveDevicesDaily {
					age := days - 1 - i
					wantDate := today.UTC().AddDate(0, 0, -age).Format("2006-01-02")
					windows, other := 0, 0
					if age == 0 || age == 6 || age == 29 {
						windows, other = 2, 1
					}
					if day.Day != wantDate || day.Windows != windows || day.MacOS != other || day.Linux != other || day.Android != other || day.IOS != other || day.HarmonyOS != other || day.MacLinux != other || day.Mobile != other {
						t.Errorf("每日活跃设备错误：%+v，期望 %s windows=%d，其余平台及兼容分组均为 %d", day, wantDate, windows, other)
					}
				}
				query := <-trace.queries
				if zone != "UTC" || days != 30 {
					continue
				}
				// 只检查每日设备子查询，避免总览其他统计影响扫描次数。
				_, query, ok := strings.Cut(query, "'active_devices_daily',(")
				if !ok {
					t.Fatal("没有找到每日活跃设备查询")
				}
				query, _, ok = strings.Cut(query, "),\n   'active_devices_7d'")
				if !ok {
					t.Fatal("没有找到每日活跃设备查询的结束位置")
				}
				var raw json.RawMessage
				if err := pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query, days).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				type node struct {
					Relation string `json:"Relation Name"`
					Loops    int    `json:"Actual Loops"`
					Plans    []node `json:"Plans"`
				}
				var plans []struct{ Plan node }
				if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
					t.Fatal(err, string(raw))
				}
				var visit func(node) bool
				visit = func(n node) bool {
					hasEvents := n.Relation == "admin_events"
					for _, child := range n.Plans {
						hasEvents = visit(child) || hasEvents
					}
					if hasEvents && n.Loops > 1 {
						t.Errorf("每日设备统计重复遍历事件 %d 次", n.Loops)
					}
					return hasEvents
				}
				if !visit(plans[0].Plan) {
					t.Fatal("执行计划中没有事件表扫描")
				}
			}
		})
	}
}
