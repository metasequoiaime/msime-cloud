package account

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func previousTelemetryLines(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

func TestTelemetryLinesCompatibility(t *testing.T) {
	check := func(text string) {
		t.Helper()
		if got, want := telemetryLines(text), previousTelemetryLines(text); got != want {
			t.Fatalf("换行归一化发生变化：输入=%q，结果=%q，期望=%q", text, got, want)
		}
	}
	var enumerate func(string, int)
	enumerate = func(text string, remaining int) {
		check(text)
		if remaining == 0 {
			return
		}
		for _, part := range []string{"\r", "\n", "a", "界", "\xff"} {
			enumerate(text+part, remaining-1)
		}
	}
	enumerate("", 6)
	random := rand.New(rand.NewPCG(31, 73))
	for range 4096 {
		text := make([]byte, random.IntN(1024))
		for i := range text {
			switch random.IntN(8) {
			case 0:
				text[i] = '\r'
			case 1:
				text[i] = '\n'
			default:
				text[i] = byte(random.IntN(256))
			}
		}
		check(string(text))
	}
}

func TestTelemetryLinesAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		budget     float64
	}{
		{"空文本", "", 0},
		{"LF", strings.Repeat("at msime.Layout.handle()\n", 400), 0},
		{"CRLF", strings.Repeat("at msime.Layout.handle()\r\n", 400), 1},
		{"CR", strings.Repeat("at msime.Layout.handle()\r", 400), 1},
		{"混合换行", strings.Repeat("at msime.Layout.handle()\r\nat msime.Layout.dispatch()\r", 200), 1},
		{"连续换行", strings.Repeat("\r\r\n", 4000), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result string
			allocations := testing.AllocsPerRun(3, func() { result = telemetryLines(tc.text) })
			if result != previousTelemetryLines(tc.text) {
				t.Fatal("归一化结果改变")
			}
			if allocations > tc.budget {
				t.Fatalf("归一化分配 %.0f 次，预算 %.0f 次", allocations, tc.budget)
			}
		})
	}
}

func TestTelemetryLinesDenseCRLFByteBudget(t *testing.T) {
	text := strings.Repeat("\r\n", 4000)
	var result string
	measured := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			result = telemetryLines(text)
		}
	})
	if result != strings.Repeat("\n", 4000) {
		t.Fatal("连续 CRLF 的结果改变")
	}
	// 按归一化后的长度分配，允许分配器取整，防止按原文容量保留两倍空间。
	if allocated := measured.AllocedBytesPerOp(); allocated > 4608 {
		t.Fatalf("4000 字节归一化结果分配 %d 字节，超过 4608 字节预算", allocated)
	}
}

func BenchmarkTelemetryLines(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"LF", strings.Repeat("at msime.Layout.handle()\n", 400)},
		{"CRLF", strings.Repeat("at msime.Layout.handle()\r\n", 400)},
		{"CR", strings.Repeat("at msime.Layout.handle()\r", 400)},
		{"混合换行", strings.Repeat("at msime.Layout.handle()\r\nat msime.Layout.dispatch()\r", 200)},
		{"短CRLF", "crash\r\nframe"},
		{"短混合换行", "crash\r\nframe\r"},
		{"连续CRLF", strings.Repeat("\r\n", 4000)},
		{"连续混合换行", strings.Repeat("\r\r\n", 4000)},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{{"原实现", previousTelemetryLines}, {"当前", telemetryLines}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				want := previousTelemetryLines(tc.text)
				b.ReportAllocs()
				b.SetBytes(int64(len(tc.text)))
				for b.Loop() {
					if got := impl.normalize(tc.text); got != want {
						b.Fatal("归一化结果改变")
					}
				}
			})
		}
	}
}
