package account

import (
	"strings"
	"testing"
)

func TestCrashQuerySelectionReusesStaticSQL(t *testing.T) {
	all := crashGroupListQuery(false)
	platform := crashGroupListQuery(true)
	if all != previousCrashGroupListQuery(false) || platform != previousCrashGroupListQuery(true) {
		t.Fatal("静态崩溃分组列表查询与原查询文本不兼容")
	}
	if all == platform || !strings.Contains(all, "WHERE ($2='' OR g.platform=$2)") || !strings.Contains(platform, "signature IN (SELECT signature FROM admin_crash_groups WHERE platform=$2)") {
		t.Fatalf("崩溃分组列表查询缺少全量或平台筛选条件：all=%q platform=%q", all, platform)
	}
	for name, query := range map[string]string{
		"all":      all,
		"platform": platform,
		"detail":   crashGroupDetailQuery,
		"spike":    crashSpikeQuery,
	} {
		if query == "" || !strings.Contains(query, "WITH c AS (") {
			t.Fatalf("%s 查询没有预构建统计 CTE", name)
		}
	}
	var got string
	if allocations := testing.AllocsPerRun(100, func() { got = crashGroupListQuery(false) }); allocations != 0 || got != all {
		t.Fatalf("重复选择全量查询分配 %.0f 次或文本变化：%q", allocations, got)
	}
}

func BenchmarkCrashQuerySelection(b *testing.B) {
	for _, tc := range []struct {
		name     string
		platform bool
	}{
		{"all", false}, {"platform", true},
	} {
		b.Run(tc.name+"/previous", func(b *testing.B) {
			for b.Loop() {
				if previousCrashGroupListQuery(tc.platform) == "" {
					b.Fatal("空查询")
				}
			}
		})
		b.Run(tc.name+"/current", func(b *testing.B) {
			for b.Loop() {
				if crashGroupListQuery(tc.platform) == "" {
					b.Fatal("空查询")
				}
			}
		})
	}
}

func previousCrashGroupListQuery(platform bool) string {
	countsQuery := crashGroupCountsBase + ` AND ($4='' OR version=$4) GROUP BY signature`
	if platform {
		countsQuery = crashGroupCountsBase + ` AND signature IN (SELECT signature FROM admin_crash_groups WHERE platform=$2) AND ($4='' OR version=$4) GROUP BY signature`
	}
	return `WITH c AS (` + countsQuery + `)
SELECT ` + crashGroupColumns + ` FROM admin_crash_groups g LEFT JOIN c USING(signature)
WHERE ($2='' OR g.platform=$2) AND ($4='' OR c.c7 IS NOT NULL)
ORDER BY COALESCE(c.c7,0) DESC,g.last_seen DESC,g.signature LIMIT $3`
}
