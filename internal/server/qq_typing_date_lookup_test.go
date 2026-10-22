package server

import (
	"slices"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func qqTypingDateLookupReport(today time.Time, dense bool) account.QQTypingReport {
	counts := make([]int64, qqTypingDays)
	if dense {
		for n := range counts {
			counts[n] = 1000
		}
	} else {
		counts[8] = 1000
	}
	return account.QQTypingReport{Linked: true, DisplayName: "测试用户", Devices: 1, Days: qqTypingWeek(today, counts...)}
}

func BenchmarkQQTypingCardDateLookup(b *testing.B) {
	today := time.Date(2026, 10, 17, 0, 0, 0, 0, qqStatsZone)
	for _, tc := range []struct {
		name  string
		dense bool
	}{{"连续60天", true}, {"最近7天无记录", false}} {
		b.Run(tc.name, func(b *testing.B) {
			report := qqTypingDateLookupReport(today, tc.dense)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = qqTypingCard(report, today)
			}
		})
	}
}

func TestQQTypingCardDateLookupAllocations(t *testing.T) {
	today := time.Date(2026, 10, 17, 0, 0, 0, 0, qqStatsZone)
	for _, tc := range []struct {
		name   string
		dense  bool
		budget float64
	}{{"连续60天", true, 48}, {"最近7天无记录", false, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			report := qqTypingDateLookupReport(today, tc.dense)
			var card string
			allocs := testing.AllocsPerRun(100, func() { card = qqTypingCard(report, today) })
			if card == "" {
				t.Fatal("卡片为空")
			}
			if allocs > tc.budget {
				t.Fatalf("卡片分配 %.0f 次，超过 %.0f 次预算", allocs, tc.budget)
			}
		})
	}
}

func TestQQTypingCardDateLookupBoundaries(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		today      time.Time
		sparseDate string
	}{
		{"跨年", time.Date(2026, 1, 1, 0, 0, 0, 0, qqStatsZone), "12月30日"},
		{"闰日", time.Date(2024, 3, 1, 0, 0, 0, 0, qqStatsZone), "2月28日"},
		{"闰日之后", time.Date(2024, 3, 2, 0, 0, 0, 0, qqStatsZone), "2月29日"},
		{"夏令时开始", time.Date(2026, 3, 9, 0, 0, 0, 0, newYork), "3月7日"},
		{"夏令时结束", time.Date(2026, 11, 2, 0, 0, 0, 0, newYork), "10月31日"},
		{"负偏移时区", time.Date(2026, 10, 17, 23, 59, 0, 0, time.FixedZone("测试时区", -7*3600)), "10月15日"},
		{"五位年份", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), "12月30日"},
		{"负年份", time.Date(-1, 3, 1, 0, 0, 0, 0, time.UTC), "2月27日"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dense := qqTypingDateLookupReport(tc.today, true)
			want := "【测试用户 · 今天】\n1,000 字\n\n【趋势】近 7 天 7,000 字，和上一周差不多，已经连续 60 天\n\n（只统计打开了「上传到云端」的设备，现在有 1 台；别的设备要各自打开）"
			if got := qqTypingCard(dense, tc.today); got != want {
				t.Fatalf("连续记录输出：\n%s\n期望：\n%s", got, want)
			}

			sparse := account.QQTypingReport{Linked: true, DisplayName: "测试用户", Devices: 1, Days: qqTypingWeek(tc.today, 0, 0, 1, 1100)}
			slices.Reverse(sparse.Days)
			// 重复日期以最后一条为准；无效、未来和超过保留期的记录不影响卡片。
			sparse.Days = append(sparse.Days,
				account.TypingDay{Day: "无效日期", Characters: 999},
				account.TypingDay{Day: tc.today.AddDate(0, 0, 1).Format(time.DateOnly), Characters: 999},
				account.TypingDay{Day: tc.today.AddDate(0, 0, -60).Format(time.DateOnly), Characters: 999},
				account.TypingDay{Day: tc.today.AddDate(0, 0, -2).Format(time.DateOnly), Characters: 1200},
			)
			want = "【测试用户 · " + tc.sparseDate + "】\n1,200 字\n\n【趋势】近 7 天 2,300 字，已经连续 2 天\n\n（只统计打开了「上传到云端」的设备，现在有 1 台；别的设备要各自打开）"
			if got := qqTypingCard(sparse, tc.today); got != want {
				t.Fatalf("稀疏记录输出：\n%s\n期望：\n%s", got, want)
			}

			inactive := qqTypingDateLookupReport(tc.today, false)
			if got := qqTypingCard(inactive, tc.today); got != "「测试用户」最近 7 天没有打字记录。" {
				t.Fatal(got)
			}
		})
	}
}
