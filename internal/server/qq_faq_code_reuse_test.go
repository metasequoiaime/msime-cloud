package server

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// 固定优化前的分派和解析比较，仅改函数名，避免用旧正则语义作为对照。
func previousQQSameCodeBlocksBeforeReuse(a, b string) bool {
	if qqFAQCodeNeedsParser(a) || qqFAQCodeNeedsParser(b) {
		return previousQQFAQSameParsedCodeBeforeReuse(a, b)
	}
	for {
		first, nextA, validA := qqNextCodeBlock(a)
		second, nextB, validB := qqNextCodeBlock(b)
		if !validA || !validB || first != second {
			return false
		}
		if first == "" {
			return true
		}
		a, b = nextA, nextB
	}
}

func previousQQFAQSameParsedCodeBeforeReuse(first, second string) bool {
	a, validA := qqFAQParsedCode(first, false)
	b, validB := qqFAQParsedCode(second, false)
	return validA && validB && slices.EqualFunc(a, b, func(a, b qqFAQCodeBlock) bool { return a.kind == b.kind && a.source == b.source })
}

func qqFAQCodeReuseSamples() []struct {
	name, first, second string
	want                bool
} {
	valid := strings.Repeat("> ```sh\n> echo example\n> ```\n\n", 50)
	invalid := valid + "> ```sh\n> unclosed"
	return []struct {
		name, first, second string
		want                bool
	}{
		{"IdenticalQuoted", valid, valid, true},
		{"IdenticalUnclosed", invalid, invalid, false},
		{"InvalidFirst", invalid, valid, false},
		{"DifferentProse", "原文\n\n" + valid, "譯文\n\n" + valid, true},
		{"DifferentCode", valid, strings.ReplaceAll(valid, "example", "changed"), false},
	}
}

func TestQQFAQParsedCodeReuseAllocationBudget(t *testing.T) {
	for _, tc := range qqFAQCodeReuseSamples()[:3] {
		t.Run(tc.name, func(t *testing.T) {
			if utf8.RuneCountInString(tc.first) > qqFAQRunes || utf8.RuneCountInString(tc.second) > qqFAQRunes || !qqFAQCodeNeedsParser(tc.first) {
				t.Fatal("样本未覆盖合法长度内的解析路径")
			}
			var got, previous bool
			old := testing.AllocsPerRun(20, func() { previous = previousQQSameCodeBlocksBeforeReuse(tc.first, tc.second) })
			current := testing.AllocsPerRun(20, func() { got = qqSameCodeBlocks(tc.first, tc.second) })
			if got != tc.want || previous != tc.want {
				t.Fatalf("比较结果变化：当前=%t，旧版=%t，期望=%t", got, previous, tc.want)
			}
			t.Logf("分配次数：旧版=%g，当前=%g", old, current)
			// 只给代码块比较阶段设预算；相同答案或无效首份不应重复建立解析树。
			if current > old*0.75 {
				t.Fatalf("分配次数 %g 超过旧版 %g 的 75%%", current, old)
			}
		})
	}
}

func TestQQFAQParsedCodeReuseCompatibility(t *testing.T) {
	samples := []string{
		"", "普通正文", "> ```sh\n> echo example\n> ```", "> ```sh\n> unclosed",
		"> ```sh\n> code\n\n容器结束", "- ```sh\n  code\n  ```", "1. ```\n   code\n   ```",
		"> > ~~~\n> > code\n> > ~~~", "    code\n    second", "<pre>\n```\nhidden\n</pre>",
		"> ```\r\n> code\r\n> ```", "> ```\n> \x00\xff\n> ```", "> ```\n> a\n> ```\n\n    b",
		"    b\n\n> ```\n> a\n> ```", "```\ncode\n```", "<div>\ntext\n</div>",
	}
	for _, tc := range qqFAQCodeReuseSamples() {
		samples = append(samples, tc.first, tc.second)
	}
	random := rand.New(rand.NewSource(1528))
	for range 100 {
		data := make([]byte, random.Intn(96))
		_, _ = random.Read(data)
		samples = append(samples, "> ```\n> "+string(data)+"\n> ```", string(data))
	}
	for _, first := range samples {
		for _, second := range samples {
			if got, want := qqSameCodeBlocks(first, second), previousQQSameCodeBlocksBeforeReuse(first, second); got != want {
				t.Fatalf("分派比较变化：%q / %q，当前=%t，旧版=%t", first, second, got, want)
			}
			if got, want := qqFAQSameParsedCode(first, second), previousQQFAQSameParsedCodeBeforeReuse(first, second); got != want {
				t.Fatalf("解析比较变化：%q / %q，当前=%t，旧版=%t", first, second, got, want)
			}
		}
	}
}

func FuzzQQFAQParsedCodeReuse(f *testing.F) {
	for _, tc := range qqFAQCodeReuseSamples() {
		f.Add(tc.first, tc.second)
	}
	f.Add("> ```\n> \x00\xff\n> ```", "    code")
	f.Fuzz(func(t *testing.T, first, second string) {
		if len(first) > 4096 || len(second) > 4096 {
			t.Skip()
		}
		for _, pair := range [][2]string{{first, second}, {first, first}} {
			if got, want := qqSameCodeBlocks(pair[0], pair[1]), previousQQSameCodeBlocksBeforeReuse(pair[0], pair[1]); got != want {
				t.Fatalf("比较变化：%q / %q", pair[0], pair[1])
			}
		}
	})
}

func BenchmarkQQFAQParsedCodeReuse(b *testing.B) {
	for _, tc := range qqFAQCodeReuseSamples() {
		for _, impl := range []struct {
			name    string
			compare func(string, string) bool
		}{{"Previous", previousQQSameCodeBlocksBeforeReuse}, {"Current", qqSameCodeBlocks}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if got := impl.compare(tc.first, tc.second); got != tc.want {
						b.Fatal("比较结果变化")
					}
				}
			})
		}
	}
}
