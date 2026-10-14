package account

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCrashGroupPlatformLimitsEventAggregation(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events,admin_crash_groups;
 INSERT INTO admin_crash_groups(signature,platform,version,title,first_seen,last_seen)
 SELECT lpad(to_hex(n),16,'0'),CASE WHEN n>=98 THEN 'ios' ELSE 'windows' END,'1','合成崩溃',now()-interval '20 days',now()-n*interval '1 second'
 FROM generate_series(1,100) n;
 INSERT INTO admin_events(id,kind,platform,version,signature,install_id,created_at)
 SELECT 'crash-filter-'||n||'-'||copy,'crash',CASE WHEN n=99 THEN 'iPadOS' ELSE 'windows' END,'1',lpad(to_hex(n),16,'0'),
 CASE WHEN n=99 THEN NULL ELSE 'filter-device-'||lpad((copy%4)::text,2,'0') END,now()-(copy/10)*interval '10 days'
 FROM generate_series(1,99) n CROSS JOIN generate_series(0,29) copy;
 INSERT INTO admin_events(id,kind,platform,version,signature) VALUES
 ('crash-filter-null','crash','windows','1',lpad(to_hex(98),16,'0')),
 ('crash-filter-other-kind','active','ios','1',lpad(to_hex(98),16,'0')),
 ('crash-filter-unsigned','crash','ios','1',NULL);
 ANALYZE admin_events,admin_crash_groups;`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "admin_events", queries: make(chan communityPageQuery, 2)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, platform := range []string{"ios", "iPadOS", "windows", "missing", ""} {
		t.Run(platform, func(t *testing.T) {
			body := getCrashGroups(t, a, "?platform="+platform, 200)
			wantGroups := 3
			if platform == "windows" {
				wantGroups = 97
			} else if platform == "missing" {
				wantGroups = 0
			} else if platform == "" {
				wantGroups = 100
			}
			if len(body.Items) != wantGroups || body.Summary.Groups != int64(wantGroups) || body.HasMore || len(body.Platforms) != 2 || body.Platforms[0] != (crashPlatformCount{"ios", 3}) || body.Platforms[1] != (crashPlatformCount{"windows", 97}) {
				t.Fatalf("平台筛选或全局平台计数错误：%+v", body)
			}
			if platform == "ios" || platform == "iPadOS" {
				for i, item := range body.Items {
					want7d, wantPrev := int64(10), int64(10)
					if i == 0 {
						want7d = 11
					} else if i == 2 {
						want7d, wantPrev = 0, 0
					}
					if item.Signature != fmt.Sprintf("%016x", 98+i) || item.Platform != "ios" || item.Count7d != want7d || item.CountPrev7d != wantPrev || item.New {
						t.Errorf("筛选后的崩溃分组错误：%+v", item)
					}
					if i == 0 && (item.Devices7d == nil || *item.Devices7d != 4) || i != 0 && item.Devices7d != nil {
						t.Errorf("设备去重或未上报标记错误：%+v", item)
					}
				}
			}
			var query communityPageQuery
			for len(trace.queries) > 0 {
				captured := <-trace.queries
				if strings.Contains(captured.sql, "WITH c AS (") {
					query = captured
				}
			}
			if query.sql == "" {
				t.Fatal("没有记录到崩溃分组列表查询")
			}
			var raw json.RawMessage
			if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			type node struct {
				Relation string   `json:"Relation Name"`
				Keys     []string `json:"Group Key"`
				Rows     int      `json:"Actual Rows"`
				Loops    int      `json:"Actual Loops"`
				Plans    []node   `json:"Plans"`
			}
			var plans []struct{ Plan node }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatal(err, string(raw))
			}
			grouped, seen, groupScans := 0, false, 0
			var visit func(node)
			visit = func(n node) {
				if n.Relation == "admin_crash_groups" {
					groupScans += n.Loops
				}
				for _, key := range n.Keys {
					if key == "signature" || strings.HasSuffix(key, ".signature") {
						grouped += n.Rows * n.Loops
						seen = true
						break
					}
				}
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			// 少量分组的平台先筛选事件；占多数的平台和无筛选场景保留原聚合，避免额外扫描分组表。
			maxGrouped := wantGroups
			if platform == "windows" {
				maxGrouped = 100
			}
			if !seen || grouped > maxGrouped {
				t.Fatalf("事件聚合生成了 %d 个分组，最多需要 %d 个；找到聚合=%v", grouped, maxGrouped, seen)
			}
			if (platform == "windows" || platform == "") && groupScans != 1 {
				t.Errorf("多数平台或无筛选查询扫描分组表 %d 次，期望沿用原查询只扫描一次", groupScans)
			}
		})
	}
}
