package server

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/6tail/tyme4go/tyme"
)

var qqTodayComputationResult string

func previousQQTodayCalendarCard(now time.Time, releases []qqPlatformRelease) string {
	now = now.In(qqStatsZone)
	day, err := tyme.SolarDay{}.FromYmd(now.Year(), int(now.Month()), now.Day())
	if err != nil {
		return now.Format("1月2日")
	}
	lines := []string{qqTodayHeadline(*day), qqTodaySeason(*day)}
	if line := qqTodayFestivals(*day); line != "" {
		lines = append(lines, line)
	}
	if line := qqTodayHoliday(*day); line != "" {
		lines = append(lines, "【假期】"+line)
	}
	if line := qqTodayReleases(*day, releases); line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n\n")
}

func TestQQTodayCardCalendarAllocationBudget(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, qqStatsZone)
	allocs := testing.AllocsPerRun(10, func() {
		qqTodayComputationResult = qqTodayCard(now, nil)
	})
	if allocs > 4 {
		t.Fatalf("同一日期卡片分配 %.0f 次，期望不超过 4 次", allocs)
	}
}

func TestQQTodayCardCalendarOutputCompatibility(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 2, 16, 8, 0, 0, 0, qqStatsZone),
		time.Date(2026, 2, 17, 8, 0, 0, 0, qqStatsZone),
		time.Date(2026, 7, 20, 8, 0, 0, 0, qqStatsZone),
		time.Date(2026, 10, 3, 8, 0, 0, 0, qqStatsZone),
		time.Date(2026, 10, 7, 8, 0, 0, 0, qqStatsZone),
		time.Date(2026, 10, 9, 8, 0, 0, 0, qqStatsZone),
		time.Date(2026, 10, 10, 8, 0, 0, 0, qqStatsZone),
		time.Date(2026, 12, 31, 15, 59, 59, 0, time.UTC),
		time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 28, 16, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 29, 16, 0, 0, 0, time.UTC),
		time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		for range 2 {
			if got, want := qqTodayCard(now, nil), previousQQTodayCalendarCard(now, nil); got != want {
				t.Errorf("时间=%s：当前 %q，原先 %q", now.Format(time.RFC3339), got, want)
			}
		}
	}
}

func TestQQTodayCardCalendarKeepsReleaseUpdates(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, qqStatsZone)
	for _, tag := range []string{"v1.0.0", "v1.0.1", ""} {
		var releases []qqPlatformRelease
		if tag != "" {
			releases = []qqPlatformRelease{{Name: "合成平台", Prefix: "v", Stable: &qqRelease{TagName: tag, PublishedAt: now.Format(time.RFC3339)}}}
		}
		if got, want := qqTodayCard(now, releases), previousQQTodayCalendarCard(now, releases); got != want {
			t.Errorf("版本=%q：当前 %q，原先 %q", tag, got, want)
		}
	}
}

func TestQQTodayCardCalendarConcurrentDates(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, qqStatsZone)
	type result struct {
		now  time.Time
		want string
	}
	cases := []result{{now: now}, {now: now.AddDate(0, 0, 1)}}
	for i := range cases {
		cases[i].want = previousQQTodayCalendarCard(cases[i].now, nil)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for n := range 12 {
		workers.Go(func() {
			<-start
			tc := cases[n%len(cases)]
			if got := qqTodayCard(tc.now, nil); got != tc.want {
				t.Errorf("并发日期=%s：当前 %q，期望 %q", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
	close(start)
	workers.Wait()
}

func BenchmarkQQTodayCardCalendarReuse(b *testing.B) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, qqStatsZone)
	for _, tc := range []struct {
		name     string
		releases []qqPlatformRelease
	}{
		{"没有发版", nil},
		{"当日发版", []qqPlatformRelease{{Name: "合成平台", Prefix: "v", Stable: &qqRelease{TagName: "v1.0.0", PublishedAt: now.Format(time.RFC3339)}}}},
	} {
		for _, impl := range []struct {
			name string
			run  func(time.Time, []qqPlatformRelease) string
		}{
			{"修改前", previousQQTodayCalendarCard},
			{"修改后", qqTodayCard},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				qqTodayComputationResult = impl.run(now, tc.releases)
				b.ReportAllocs()
				for b.Loop() {
					qqTodayComputationResult = impl.run(now, tc.releases)
				}
			})
		}
	}
}

func BenchmarkQQTodayComputation(b *testing.B) {
	day := qqTodayHolidayIndexDay(b, 2026, 10, 17)
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, qqStatsZone)
	for _, part := range []struct {
		name string
		run  func(tyme.SolarDay) string
	}{
		{"标题", qqTodayHeadline},
		{"节气", qqTodaySeason},
		{"近期节日", qqTodayFestivals},
		{"完整卡片", func(tyme.SolarDay) string { return qqTodayCard(now, nil) }},
	} {
		b.Run(part.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqTodayComputationResult = part.run(day)
			}
		})
	}
}

func BenchmarkQQTodayCalendarFirstDate(b *testing.B) {
	day := qqTodayHolidayIndexDay(b, 2026, 10, 17)
	var warmup qqTodayCalendarCache
	qqTodayComputationResult = warmup.get(day)
	b.ReportAllocs()
	for b.Loop() {
		var cache qqTodayCalendarCache
		qqTodayComputationResult = cache.get(day)
	}
}
