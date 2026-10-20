package account

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestResourceTextScanCompatibility(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	bounds := [][2]int{{-1, -1}, {-1, 0}, {0, 0}, {0, 1}, {1, 1}, {1, 2}, {2, 3}, {3, 2}, {2, 5}, {1, 64}, {0, 16000}, {0, maximum}, {maximum, maximum}, {-maximum - 1, maximum}}
	check := func(text string, minimum, maximum int) {
		t.Helper()
		for _, multiline := range []bool{false, true} {
			if got, want := resourceText(text, minimum, maximum, multiline), resourceTextBeforeScan(text, minimum, maximum, multiline); got != want {
				t.Fatalf("文本校验结果改变：输入=%q，最小=%d，最大=%d，多行=%t，实际=%t，期望=%t", text, minimum, maximum, multiline, got, want)
			}
		}
	}
	var enumerate func(string, int)
	enumerate = func(text string, remaining int) {
		for _, bound := range bounds {
			check(text, bound[0], bound[1])
		}
		if remaining == 0 {
			return
		}
		for _, part := range []string{"a", " ", "\n", "\t", "界", "😀", "\xff"} {
			enumerate(text+part, remaining-1)
		}
	}
	enumerate("", 4)
	for _, text := range []string{
		"\u2003 合成 \u3000", "\u0085", "\x00", "\r", "\x7f", "\ufffd", "\xed\xa0\x80", "\xf4\x90\x80\x80",
		strings.Repeat("😀", 64), strings.Repeat("😀", 64) + "a", strings.Repeat("界", 200),
	} {
		for _, bound := range bounds {
			check(text, bound[0], bound[1])
		}
		check(text, 0, utf8.RuneCountInString(text))
		check(text, 0, len(text)/utf8.UTFMax)
	}
	random := rand.New(rand.NewPCG(37, 83))
	for range 4096 {
		raw := make([]byte, random.IntN(1024))
		for i := range raw {
			raw[i] = byte(random.Uint32())
		}
		bound := bounds[random.IntN(len(bounds))]
		check(string(raw), bound[0], bound[1])
	}
}

func TestResourceTextRejectsOversizeWithoutFullScan(t *testing.T) {
	text := strings.Repeat("a", 4<<20)
	var accepted bool
	measured := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			accepted = resourceText(text, 0, 64, true)
		}
	})
	if accepted {
		t.Fatal("超长文本被接受")
	}
	// 字节数已能证明超过字符上限，保留宽裕预算，防止重新扫描 4 MiB 全文。
	if duration := measured.NsPerOp(); duration > 100000 {
		t.Fatalf("明显超长文本每次校验耗时 %d ns，超过 100000 ns 预算", duration)
	}
}

func BenchmarkResourceTextScan(b *testing.B) {
	for _, tc := range []struct {
		name, text       string
		minimum, maximum int
		multiline        bool
	}{
		{"遥测堆栈", strings.Repeat("at msime.Layout.handle()\n", 400), 0, 16000, true},
		{"中文消息", strings.Repeat("合成消息", 200), 0, 1000, true},
		{"ASCII名称", "Synthetic Resource", 1, 32, false},
		{"中文名称", "合成测试资源", 1, 32, false},
		{"事件标识", "synthetic-telemetry-event-000001", 16, 128, false},
		{"最小字符数", "\u2003 合成文本 \u3000", 2, 32, false},
		{"非法UTF8", "synthetic\xff", 1, 32, false},
		{"明显超长", strings.Repeat("a", 4<<20), 0, 64, true},
	} {
		for _, impl := range []struct {
			name  string
			valid func(string, int, int, bool) bool
		}{{"原实现", resourceTextBeforeScan}, {"当前", resourceText}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				want := resourceTextBeforeScan(tc.text, tc.minimum, tc.maximum, tc.multiline)
				b.ReportAllocs()
				for b.Loop() {
					if got := impl.valid(tc.text, tc.minimum, tc.maximum, tc.multiline); got != want {
						b.Fatal("文本校验结果改变")
					}
				}
			})
		}
	}
}

// 保留改动前的完整校验作为独立对照，验证字符与控制符规则保持一致。
func resourceTextBeforeScan(s string, min, max int, multiline bool) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(strings.TrimSpace(s)) < min || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) && !(multiline && (c == '\n' || c == '\t')) {
			return false
		}
	}
	return true
}
