package account

import (
	"strings"
	"testing"
	"time"
)

const pluginStrftimeMixedNumericFormat = "%C%d%e%g%H%I%j%k%l%m%M%S%U%V%W%y" +
	"%_C%_d%_e%_g%_H%_I%_j%_k%_l%_m%_M%_S%_U%_V%_W%_y" +
	"%-C%-d%-e%-g%-H%-I%-j%-k%-l%-m%-M%-S%-U%-V%-W%-y" +
	"%0C%0d%0e%0g%0H%0I%0j%0k%0l%0m%0M%0S%0U%0V%0W%0y"

func TestPluginStrftimeFixedComponents(t *testing.T) {
	instants := []time.Time{
		time.Date(2021, time.January, 3, 0, 4, 5, 0, time.UTC),
		time.Date(2016, time.January, 2, 13, 4, 5, 0, time.UTC),
	}
	for _, tc := range []struct {
		component byte
		want      [2]string
	}{
		{'%', [2]string{"%", "%"}},
		{'a', [2]string{"Sun", "Sat"}},
		{'A', [2]string{"Sunday", "Saturday"}},
		{'b', [2]string{"Jan", "Jan"}},
		{'h', [2]string{"Jan", "Jan"}},
		{'B', [2]string{"January", "January"}},
		{'c', [2]string{"Sun Jan  3 00:04:05 2021", "Sat Jan  2 13:04:05 2016"}},
		{'D', [2]string{"01/03/21", "01/02/16"}},
		{'x', [2]string{"01/03/21", "01/02/16"}},
		{'F', [2]string{"2021-01-03", "2016-01-02"}},
		{'G', [2]string{"2020", "2015"}},
		{'n', [2]string{"\n", "\n"}},
		{'p', [2]string{"AM", "PM"}},
		{'P', [2]string{"am", "pm"}},
		{'r', [2]string{"12:04:05 AM", "01:04:05 PM"}},
		{'R', [2]string{"00:04", "13:04"}},
		{'t', [2]string{"\t", "\t"}},
		{'T', [2]string{"00:04:05", "13:04:05"}},
		{'X', [2]string{"00:04:05", "13:04:05"}},
		{'u', [2]string{"7", "6"}},
		// 保留现有的星期编号：星期日为 1、星期六为 7。
		{'w', [2]string{"1", "7"}},
		{'Y', [2]string{"2021", "2016"}},
	} {
		for _, pad := range []string{"", "_", "-", "0"} {
			format := "前%" + pad + string(tc.component) + "后"
			for i, instant := range instants {
				want := "前" + tc.want[i] + "后"
				if got, ok := pluginStrftime(format, instant); !ok || got != want {
					t.Errorf("格式 %q，时间 %s：得到 %q/%t，期望 %q/true", format, instant, got, ok, want)
				}
			}
		}
	}
}

func TestPluginStrftimeDemandAllocations(t *testing.T) {
	at := commandTemplateInstants[0]
	for _, tc := range []struct {
		name, format, want string
		ok                 bool
		max                float64
	}{
		{"空格式", "", "", true, 0},
		{"纯文本", "日期", "日期", true, 1},
		{"百分号", "%%", "%", true, 1},
		{"星期缩写", "%a", "Wed", true, 1},
		{"缺少指令", "%", "", false, 0},
		{"缺少填充指令", "%_", "", false, 0},
		{"未知指令", "%Q", "", false, 0},
		{"带填充的未知指令", "%-Q", "", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var ok bool
			allocations := testing.AllocsPerRun(100, func() { got, ok = pluginStrftime(tc.format, at) })
			if got != tc.want || ok != tc.ok {
				t.Fatalf("得到 %q/%t，期望 %q/%t", got, ok, tc.want, tc.ok)
			}
			if allocations > tc.max {
				t.Fatalf("格式化分配 %g 次，预算 %g 次", allocations, tc.max)
			}
		})
	}
}

func TestPluginStrftimeRepeatedComponents(t *testing.T) {
	at := commandTemplateInstants[0]
	for _, tc := range []struct{ format, want string }{
		{"%m|%-m|%_m|%0m|%m|%-m", "09|9| 9|09|09|9"},
		{"%j|%_j|%-j|%0j|%j", "273|273|273|273|273"},
		{"%A|%_A|%-A|%0A|%A", "Wednesday|Wednesday|Wednesday|Wednesday|Wednesday"},
		{"%r|%_r|%-r|%0r|%r", strings.TrimSuffix(strings.Repeat("11:59:59 PM|", 5), "|")},
		{strings.Repeat("%Y", 40), strings.Repeat("2026", 40)},
		{strings.Repeat("%r", 16), strings.Repeat("11:59:59 PM", 16)},
	} {
		if got, ok := pluginStrftime(tc.format, at); !ok || got != tc.want {
			t.Errorf("格式 %q：得到 %q/%t，期望 %q/true", tc.format, got, ok, tc.want)
		}
	}
	if !validCommandTemplate("{date:" + pluginStrftimeMixedNumericFormat + "}") {
		t.Fatal("混合数字指令模板应在长度限制内被接受")
	}
}

func BenchmarkPluginStrftimeDemand(b *testing.B) {
	at := commandTemplateInstants[0]
	for _, tc := range []struct {
		name, format string
		ok           bool
	}{
		{"纯文本", strings.Repeat("日期", 20), true},
		{"日期", "%Y-%m-%d", true},
		{"时间", "%H:%M", true},
		{"星期与月份", "%A %B", true},
		{"复合时间", "%r", true},
		{"重复年份", strings.Repeat("%Y", 40), true},
		{"重复复合时间", strings.Repeat("%r", 16), true},
		{"混合填充复合时间", "%r%_r%-r%0r", true},
		{"混合数字指令", pluginStrftimeMixedNumericFormat, true},
		{"全部固定指令", "%a%A%b%h%B%c%D%x%F%G%p%P%r%R%T%X%u%w%Y", true},
		{"无效格式", "%Q", false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, ok := pluginStrftime(tc.format, at); ok != tc.ok {
					b.Fatal("格式化结果不符合预期")
				}
			}
		})
	}
}

func BenchmarkPluginCommandTemplateDates(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if !validCommandTemplate("{date} {time} {weekday}") {
			b.Fatal("命令模板校验失败")
		}
	}
}
