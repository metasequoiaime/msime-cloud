package server

import (
	"math/rand"
	"strings"
	"testing"
)

// previousInlineCode 保留先拆分和拼接字段的旧实现，核对 Markdown 输出和分配开销。
func previousInlineCode(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	fence := strings.Repeat("`", longestBacktickRun(s)+1)
	return fence + " " + s + " " + fence
}

func TestInlineCodeCompatibility(t *testing.T) {
	texts := []string{
		"", " ", "水杉😀", "msime-cloud", "one two", " one\t two\nthree ",
		"`", "```", "`` \t ```", "\xff\n\xe2\x80\x00", "\u0085\u00a0\u1680正文\u2009尾部\u2028\u2029\u202f\u205f\u3000",
		strings.Repeat("\t ", 32768), strings.Repeat("正文\t", 8192),
	}
	random := rand.New(rand.NewSource(1068))
	units := []string{"正文", "a", "😀", "`", "``", " ", "\t", "\r", "\n", "\v", "\f", "\u0085", "\u3000", "\xff", "\xe2\x80", "\x00"}
	for range 300 {
		var input strings.Builder
		for range random.Intn(100) {
			input.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, input.String())
	}
	for _, text := range texts {
		if got, want := inlineCode(text), previousInlineCode(text); got != want {
			t.Fatalf("行内代码输出变化：输入=%q，结果=%q，期望=%q", text, got, want)
		}
	}
}

func TestInlineCodeAllocationBudget(t *testing.T) {
	for _, text := range []string{"msime-cloud", "one\t two\nthree"} {
		t.Run(text, func(t *testing.T) {
			var got string
			allocs := testing.AllocsPerRun(100, func() { got = inlineCode(text) })
			if got != previousInlineCode(text) || allocs > 1 {
				t.Fatalf("行内代码输出变化或产生中间分配：输入=%q，分配=%.0f 次，预算=1 次", text, allocs)
			}
		})
	}
	text := strings.Repeat("正文\t", 8192)
	want := previousInlineCode(text)
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = inlineCode(text)
		}
	})
	if got != want || result.AllocedBytesPerOp() > int64(2*len(want)+1024) {
		t.Fatalf("多字段行内代码输出变化或分配超预算：分配=%s，结果长度=%d", result.MemString(), len(got))
	}
}

func BenchmarkCrashInlineCode(b *testing.B) {
	for _, sample := range []struct{ name, text string }{
		{"single_field", "msime-cloud"},
		{"short", "one two three"},
		{"whitespace", " one\t two\nthree "},
		{"unicode", "\u3000合成\u2009崩溃\n详情\u0085"},
		{"many_fields", strings.Repeat("正文\t", 8192)},
		{"only_whitespace", strings.Repeat(" \t\u3000", 8192)},
		{"dense_backticks", strings.Repeat("`` `\t", 100)},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) string
		}{{"previous", previousInlineCode}, {"current", inlineCode}} {
			b.Run(sample.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(sample.text)
				}
			})
		}
	}
}
