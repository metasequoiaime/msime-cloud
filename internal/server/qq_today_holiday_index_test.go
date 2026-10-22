package server

import (
	"fmt"
	"testing"

	"github.com/6tail/tyme4go/tyme"
)

func previousQQTodayHoliday(day tyme.SolarDay) string {
	if h := day.GetLegalHoliday(); h != nil {
		if h.IsWork() {
			return "今天调休上班（" + h.GetName() + "）"
		}
		first, last := day, day
		rest := func(d tyme.SolarDay) bool {
			candidate := d.GetLegalHoliday()
			return candidate != nil && !candidate.IsWork() && candidate.GetName() == h.GetName()
		}
		for rest(first.Next(-1)) {
			first = first.Next(-1)
		}
		for rest(last.Next(1)) {
			last = last.Next(1)
		}
		if left := last.Subtract(day); left > 0 {
			return fmt.Sprintf("%s假期第 %d 天，还剩 %d 天", h.GetName(), day.Subtract(first)+1, left)
		}
		return h.GetName() + "假期最后一天"
	}
	if h := day.Next(1).GetLegalHoliday(); h != nil && h.IsWork() {
		return fmt.Sprintf("明天周%s调休上班（%s）", day.Next(1).GetWeek().GetName(), h.GetName())
	}
	for n := 1; n <= qqTodayHolidayDays; n++ {
		next := day.Next(n)
		if h := next.GetLegalHoliday(); h != nil && !h.IsWork() {
			return fmt.Sprintf("下一个是%s（%d月%d日起），还有 %d 天", h.GetName(), next.GetMonth(), next.GetDay(), n)
		}
	}
	return ""
}

func qqTodayHolidayIndexDay(tb testing.TB, year, month, day int) tyme.SolarDay {
	tb.Helper()
	d, err := tyme.SolarDay{}.FromYmd(year, month, day)
	if err != nil {
		tb.Fatal(err)
	}
	return *d
}

var qqTodayHolidayIndexResult string

func TestQQTodayHolidayLookupAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		year, month int
		day         int
	}{
		{"库里没有下一年安排", 2026, 10, 17},
		{"远处的下一个假期", 2026, 7, 20},
		{"放假中", 2026, 10, 3},
		{"今天调休", 2026, 10, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			day := qqTodayHolidayIndexDay(t, tc.year, tc.month, tc.day)
			allocs := testing.AllocsPerRun(10, func() {
				qqTodayHolidayIndexResult = qqTodayHoliday(day)
			})
			if allocs > 512 {
				t.Fatalf("分配 %.0f 次，期望不超过 512 次", allocs)
			}
		})
	}
}

func TestQQTodayHolidayIndexOutputCompatibility(t *testing.T) {
	for _, ymd := range [][3]int{
		{2000, 12, 30}, {2000, 12, 31},
		{2001, 12, 27}, {2001, 12, 29}, {2002, 1, 1}, {2008, 2, 5}, {2014, 10, 4},
		{2020, 1, 23}, {2020, 2, 1}, {2020, 2, 2}, {2024, 2, 29}, {2025, 9, 30},
		{2026, 2, 16}, {2026, 7, 20}, {2026, 10, 3}, {2026, 10, 7}, {2026, 10, 9},
		{2026, 10, 10}, {2026, 10, 17}, {2027, 1, 1},
	} {
		day := qqTodayHolidayIndexDay(t, ymd[0], ymd[1], ymd[2])
		if got, want := qqTodayHoliday(day), previousQQTodayHoliday(day); got != want {
			t.Errorf("日期=%v：当前 %q，原先 %q", ymd, got, want)
		}
	}
}

func TestQQTodayHolidayNextRestBoundary(t *testing.T) {
	for _, tc := range []struct {
		day  int
		want string
	}{
		{30, ""},
		{31, "下一个是元旦（1月1日起），还有 366 天"},
	} {
		day := qqTodayHolidayIndexDay(t, 2000, 12, tc.day)
		if got := qqTodayHoliday(day); got != tc.want {
			t.Errorf("2000-12-%d：当前 %q，期望 %q", tc.day, got, tc.want)
		}
	}
}

func TestQQTodayHolidayIndexMatchesLibrary(t *testing.T) {
	day := qqTodayHolidayIndexDay(t, 2000, 1, 1)
	end := qqTodayHolidayIndexDay(t, 2031, 1, 1)
	for day.IsBefore(end) {
		got, want := qqTodayLegalHoliday(day), day.GetLegalHoliday()
		if (got == nil) != (want == nil) {
			t.Fatalf("日期=%d-%02d-%02d：当前 %v，依赖 %v", day.GetYear(), day.GetMonth(), day.GetDay(), got, want)
		}
		if got != nil && (got.GetName() != want.GetName() || got.IsWork() != want.IsWork() || got.GetDay() != want.GetDay()) {
			t.Fatalf("日期=%d-%02d-%02d：当前 %v，依赖 %v", day.GetYear(), day.GetMonth(), day.GetDay(), got, want)
		}
		if day.GetDay() == 1 {
			if got, want := qqTodayHoliday(day), previousQQTodayHoliday(day); got != want {
				t.Errorf("日期=%d-%02d-%02d：当前 %q，原先 %q", day.GetYear(), day.GetMonth(), day.GetDay(), got, want)
			}
		}
		day = day.Next(1)
	}
}

func BenchmarkQQTodayHolidayIndexBuild(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if len(qqBuildTodayHolidayIndex().byDay) == 0 {
			b.Fatal("假期索引为空")
		}
	}
}

func BenchmarkQQTodayHolidayIndex(b *testing.B) {
	for _, tc := range []struct {
		name string
		ymd  [3]int
	}{
		{"库里没有下一年安排", [3]int{2026, 10, 17}},
		{"远处的下一个假期", [3]int{2026, 7, 20}},
		{"放假中", [3]int{2026, 10, 3}},
	} {
		day := qqTodayHolidayIndexDay(b, tc.ymd[0], tc.ymd[1], tc.ymd[2])
		for _, impl := range []struct {
			name string
			run  func(tyme.SolarDay) string
		}{
			{"修改前", previousQQTodayHoliday},
			{"修改后", qqTodayHoliday},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqTodayHolidayIndexResult = impl.run(day)
				}
			})
		}
	}
}
