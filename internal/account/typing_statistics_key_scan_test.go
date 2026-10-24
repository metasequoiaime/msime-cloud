package account

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// 冻结原有键正则和完整上传校验，作为兼容性与性能对照。
var previousTypingStatisticsKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)

func previousValidTypingCounts(m map[string]int64) bool {
	if len(m) > typingStatisticsMaxKeys {
		return false
	}
	for k, n := range m {
		if !previousTypingStatisticsKey.MatchString(k) || !validTypingCount(n) {
			return false
		}
	}
	return true
}

// previousValidTypingUpload 校验上传：设备标识、平台、1 到 typingStatisticsUploadDays 个不重复的日期，日期在 today 往前 typingStatisticsRetentionDays-1 天到往后 1 天之间（时区差），活跃时间不超过一天，各计数在范围内。
func previousValidTypingUpload(v typingUpload, today time.Time) bool {
	if !typingStatisticsDeviceID.MatchString(v.Device) || !slices.Contains(typingStatisticsPlatforms, v.Platform) || len(v.Days) == 0 || len(v.Days) > typingStatisticsUploadDays {
		return false
	}
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	earliest, latest := today.AddDate(0, 0, -(typingStatisticsRetentionDays-1)), today.AddDate(0, 0, 1)
	seen := map[string]bool{}
	for _, d := range v.Days {
		day, err := time.Parse(time.DateOnly, d.Day)
		if err != nil || seen[d.Day] || day.Before(earliest) || day.After(latest) {
			return false
		}
		seen[d.Day] = true
		if !validTypingCount(d.Characters) || d.ActiveSeconds < 0 || d.ActiveSeconds > 24*3600 || !previousValidTypingCounts(d.Categories) || !previousValidTypingCounts(d.Sources) {
			return false
		}
		if d.Hours != nil && len(d.Hours) != 24 {
			return false
		}
		for _, n := range d.Hours {
			if !validTypingCount(n) {
				return false
			}
		}
	}
	if s := v.Selections; s != nil {
		if s.Ranks == nil || len(s.Ranks) > typingStatisticsMaxRanks || !validTypingCount(s.Beyond) {
			return false
		}
		for _, n := range s.Ranks {
			// 位次是全部历史的累计，比一天的上限宽一些。
			if n < 0 || n > 100*typingStatisticsMaxCount {
				return false
			}
		}
	}
	return true
}

var typingKeyValidationResult bool

func typingKeyUploadFixture(tb testing.TB) (typingUpload, time.Time) {
	tb.Helper()
	today := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	v := typingUpload{Device: "synthetic-desktop", Platform: "windows", Selections: &TypingSelections{Ranks: []int64{10, 2}, Beyond: 1}}
	for i := range typingStatisticsUploadDays {
		d := TypingDay{Day: today.AddDate(0, 0, -i).Format(time.DateOnly), Characters: 100, ActiveSeconds: 60, Hours: make([]int64, 24), Categories: map[string]int64{}, Sources: map[string]int64{}}
		for k := range typingStatisticsMaxKeys {
			d.Categories[fmt.Sprintf("category_%d", k)] = 1
			d.Sources[fmt.Sprintf("source_%d", k)] = 1
		}
		v.Days = append(v.Days, d)
	}
	body, err := json.Marshal(v)
	if err != nil || len(body) > typingStatisticsBodyBytes || !previousValidTypingUpload(v, today) {
		tb.Fatalf("上传样本无效：字节数=%d，错误=%v", len(body), err)
	}
	return v, today
}

func TestTypingKeyUploadTimeBudget(t *testing.T) {
	v, today := typingKeyUploadFixture(t)
	measure := func(validate func(typingUpload, time.Time) bool) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				typingKeyValidationResult = validate(v, today)
			}
		})
	}
	original := measure(previousValidTypingUpload)
	originalOK := typingKeyValidationResult
	current := measure(validTypingUpload)
	if !originalOK || !typingKeyValidationResult {
		t.Fatal("有效上传未通过校验")
	}
	// Go 不给测试文件中的对照实现插桩，覆盖率模式下两边的计时条件不同。
	// 仍执行真实函数和结果断言；原有耗时门槛由未启用覆盖率的竞态 CI 检查。
	if mode := testing.CoverMode(); mode != "" {
		t.Logf("覆盖率模式 %s 不比较插桩条件不同的耗时：原实现 %d ns/op，当前 %d ns/op", mode, original.NsPerOp(), current.NsPerOp())
		return
	}
	if current.NsPerOp()*2 > original.NsPerOp() {
		t.Fatalf("完整上传校验耗时 %d ns/op，原实现 %d ns/op，预算最多为原实现的一半", current.NsPerOp(), original.NsPerOp())
	}
}

func BenchmarkTypingKeyUploadValidation(b *testing.B) {
	v, today := typingKeyUploadFixture(b)
	for _, impl := range []struct {
		name     string
		validate func(typingUpload, time.Time) bool
	}{
		{"previous", previousValidTypingUpload}, {"current", validTypingUpload},
	} {
		b.Run(impl.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				typingKeyValidationResult = impl.validate(v, today)
			}
		})
	}
}

func checkTypingKeyCompatibility(t *testing.T, key string, count int64) {
	t.Helper()
	if got, want := validTypingStatisticsKey(key), previousTypingStatisticsKey.MatchString(key); got != want {
		t.Fatalf("键=%q：校验=%t，原校验=%t", key, got, want)
	}
	m := map[string]int64{key: count}
	if got, want := validTypingCounts(m), previousValidTypingCounts(m); got != want {
		t.Fatalf("键=%q 计数=%d：校验=%t，原校验=%t", key, count, got, want)
	}
}

func TestTypingKeyScanCompatibility(t *testing.T) {
	keys := []string{"", "A", "z", "han", "Category_01", "a_", "a0", "0a", "_a", "a-b", "a.b", "a b", "a\n", "\na", "a\r\n", "a\x00", "水杉", "é", "K", "ſ", "a\xff", "\xc0\xaf", "e\u0301"}
	for _, n := range []int{0, 1, 2, 31, 32, 33, 64, 1024} {
		for _, symbol := range []string{"a", "Z", "1", "_", "-", "水", "\xff"} {
			keys = append(keys, strings.Repeat(symbol, n), "a"+strings.Repeat(symbol, n))
		}
	}
	for _, key := range keys {
		for _, count := range []int64{math.MinInt64, -1, 0, 1, typingStatisticsMaxCount, typingStatisticsMaxCount + 1, math.MaxInt64} {
			checkTypingKeyCompatibility(t, key, count)
		}
	}
	for first := range 256 {
		for second := range 256 {
			checkTypingKeyCompatibility(t, string([]byte{byte(first), byte(second)}), 1)
		}
	}
	for _, n := range []int{0, 1, 31, 32, 33} {
		m := map[string]int64{}
		for i := range n {
			m[fmt.Sprintf("key_%d", i)] = 1
		}
		if got, want := validTypingCounts(m), previousValidTypingCounts(m); got != want {
			t.Fatalf("键数=%d：校验=%t，原校验=%t", n, got, want)
		}
	}
	if !validTypingCounts(nil) {
		t.Fatal("nil 计数表应有效")
	}
}

func TestTypingKeyUploadCompatibility(t *testing.T) {
	base, today := typingKeyUploadFixture(t)
	check := func(v typingUpload) {
		t.Helper()
		if got, want := validTypingUpload(v, today), previousValidTypingUpload(v, today); got != want {
			t.Fatalf("上传校验=%t，原校验=%t", got, want)
		}
	}
	check(base)
	for _, key := range []string{"", "A", "a_0", "_a", "0a", "a-b", "a\n", "水杉", "a\xff", strings.Repeat("a", 32), strings.Repeat("a", 33)} {
		for _, count := range []int64{-1, 0, typingStatisticsMaxCount, typingStatisticsMaxCount + 1} {
			for _, source := range []bool{false, true} {
				v := base
				v.Days = append([]TypingDay(nil), base.Days...)
				m := map[string]int64{key: count}
				if source {
					v.Days[len(v.Days)-1].Sources = m
				} else {
					v.Days[len(v.Days)-1].Categories = m
				}
				check(v)
			}
		}
	}
	rng := rand.New(rand.NewSource(1984))
	alphabet := []byte("abcABC019_- \n\x00\xff水杉")
	for range 1000 {
		key := make([]byte, rng.Intn(70))
		for i := range key {
			key[i] = alphabet[rng.Intn(len(alphabet))]
		}
		count := int64(rng.Intn(typingStatisticsMaxCount+3)) - 1
		checkTypingKeyCompatibility(t, string(key), count)
		v := base
		v.Days = append([]TypingDay(nil), base.Days...)
		day := rng.Intn(len(v.Days))
		v.Days[day].Categories = map[string]int64{string(key): count}
		switch rng.Intn(12) {
		case 0:
			v.Device = "bad device"
		case 1:
			v.Platform = "unknown"
		case 2:
			v.Days[day].Day = "2026-13-01"
		case 3:
			v.Days[day].Day = today.AddDate(0, 0, -62).Format(time.DateOnly)
		case 4:
			v.Days[day].Day = today.AddDate(0, 0, 2).Format(time.DateOnly)
		case 5:
			v.Days[day].ActiveSeconds = -1
		case 6:
			v.Days[day].Hours = []int64{1}
		case 7:
			v.Selections = &TypingSelections{Ranks: []int64{-1}}
		case 8:
			v.Days[day].Characters = -1
		case 9:
			v.Days[day].Sources = map[string]int64{"valid": -1}
		case 10:
			v.Selections = nil
		}
		check(v)
	}
}

func TestTypingKeyScanAvoidsAllocations(t *testing.T) {
	for _, key := range []string{"", "a", "Category_01", strings.Repeat("a", 32), strings.Repeat("a", 33), "a\xff"} {
		var got bool
		if allocations := testing.AllocsPerRun(100, func() { got = validTypingStatisticsKey(key) }); allocations != 0 {
			t.Fatalf("键=%q：分配 %.0f 次", key, allocations)
		}
		if got != previousTypingStatisticsKey.MatchString(key) {
			t.Fatalf("键=%q：校验结果不一致", key)
		}
	}
}

func FuzzTypingKeyScanCompatibility(f *testing.F) {
	for _, key := range []string{"", "A", "han", "Category_01", "_a", "0a", "a\n", "水杉\xff", strings.Repeat("a", 32), strings.Repeat("a", 33)} {
		for _, count := range []int64{-1, 0, typingStatisticsMaxCount, typingStatisticsMaxCount + 1} {
			f.Add(key, count)
		}
	}
	f.Fuzz(func(t *testing.T, key string, count int64) { checkTypingKeyCompatibility(t, key, count) })
}

func BenchmarkTypingKeyValidation(b *testing.B) {
	for _, key := range []string{"han", "category_31", strings.Repeat("a", 32)} {
		for _, impl := range []struct {
			name     string
			validate func(string) bool
		}{
			{"previous", previousTypingStatisticsKey.MatchString}, {"scan", validTypingStatisticsKey},
		} {
			b.Run(key+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					typingKeyValidationResult = impl.validate(key)
				}
			})
		}
	}
}
