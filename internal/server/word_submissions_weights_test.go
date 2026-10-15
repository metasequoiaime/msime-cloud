package server

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// 保留优化前的范围扫描，用于校验异常字段和空白处理的兼容性。
func previousWordWeightRange(content string) (low, high int, ok bool) {
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			continue
		}
		weight, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil || weight < 1 {
			continue
		}
		if !ok {
			low, high, ok = weight, weight, true
		}
		low, high = min(low, weight), max(high, weight)
	}
	return low, high, ok
}

func TestWordWeightRangeValues(t *testing.T) {
	for _, tc := range []struct {
		content string
		low     int
		high    int
		ok      bool
	}{
		{"", 0, 0, false},
		{"# 注释\n\n单列\n少\t一列\n多\t一\t2\textra\n零\t值\t0\n负\t值\t-1\n坏\t值\tnan\n", 0, 0, false},
		{"一\tyi\t30\n二\ter\t10\n三\tsan\t20", 10, 30, true},
		{" \u2003一\t yi \t +7 \r\n\t二\t er \t3\t\t\n三\t\t5\n\u2003# 忽略\tx\t1\n", 3, 7, true},
		{"一\tyi\t" + strconv.Itoa(math.MaxInt) + "\n二\ter\t" + strconv.Itoa(math.MaxInt) + "0\n", math.MaxInt, math.MaxInt, true},
		{strings.Repeat("长", 1<<20) + "\tpinyin\t9\n二\ter\t2", 2, 9, true},
	} {
		low, high, ok := wordWeightRange(tc.content)
		if low != tc.low || high != tc.high || ok != tc.ok {
			t.Fatalf("文本字节数=%d got=(%d,%d,%v) want=(%d,%d,%v)", len(tc.content), low, high, ok, tc.low, tc.high, tc.ok)
		}
	}
}

func TestWordWeightRangeMatchesPrevious(t *testing.T) {
	random := rand.New(rand.NewSource(941))
	fields := []string{"合成", "pinyin", "", "1", "10", "-1", "0", "+7", " 3 ", "\u20038\u2003", "# ignored", "\xff", "9223372036854775808", "1\x002"}
	for sample := range 500 {
		var text strings.Builder
		for range random.Intn(40) {
			for column := range random.Intn(6) {
				if column > 0 {
					text.WriteByte('\t')
				}
				text.WriteString(fields[random.Intn(len(fields))])
			}
			if random.Intn(3) == 0 {
				text.WriteByte('\r')
			}
			text.WriteByte('\n')
		}
		content := text.String()
		low, high, ok := wordWeightRange(content)
		wantLow, wantHigh, wantOK := previousWordWeightRange(content)
		if low != wantLow || high != wantHigh || ok != wantOK {
			t.Fatalf("sample=%d got=(%d,%d,%v) want=(%d,%d,%v)", sample, low, high, ok, wantLow, wantHigh, wantOK)
		}
	}
}

func TestWordWeightRangeMemoryBudget(t *testing.T) {
	content := strings.Repeat("合成\tpinyin\t10\n", 5000)
	var low, high int
	var ok bool
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			low, high, ok = wordWeightRange(content)
		}
	})
	if low != 10 || high != 10 || !ok {
		t.Fatalf("got=(%d,%d,%v) want=(10,10,true)", low, high, ok)
	}
	if bytes := result.AllocedBytesPerOp(); bytes > 16<<10 {
		t.Fatalf("扫描五千行权重分配 %d 字节，超过 16 KiB 预算", bytes)
	}
}

func BenchmarkWordWeightRange(b *testing.B) {
	for _, rows := range []int{100, 5000} {
		content := strings.Repeat("合成\tpinyin\t10\n", rows)
		for _, impl := range []struct {
			name string
			fn   func(string) (int, int, bool)
		}{{"previous", previousWordWeightRange}, {"current", wordWeightRange}} {
			b.Run(fmt.Sprintf("%d/%s", rows, impl.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					low, high, ok := impl.fn(content)
					if low != 10 || high != 10 || !ok {
						b.Fatal("扫描结果发生变化")
					}
				}
			})
		}
	}
}
