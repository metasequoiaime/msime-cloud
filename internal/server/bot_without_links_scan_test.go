package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

var previousQQLinkNote = regexp.MustCompile(`（[A-Za-z][A-Za-z0-9+.-]*://[^）]*）`)

func previousQQWithoutLinks(text string) string {
	return qqStripLinks(previousQQLinkNote.ReplaceAllString(text, ""))
}

func TestQQWithoutLinksMatchesPrevious(t *testing.T) {
	units := []string{"", "（", "）", "a", "A", "1", "+", "-", ".", ":", "/", "!", " ", "\n", "甲", "\xff", "https", "HTTP+unix", "普通"}
	check := func(input string) {
		want := previousQQWithoutLinks(input)
		if got := qqWithoutLinks(input); got != want {
			t.Fatalf("回复链接清理变化：输入=%q，实际=%q，预期=%q", input, got, want)
		}
	}
	for _, input := range []string{
		"", "普通消息", "（https://example.test）", "前（https://example.test）后",
		"前（https://example.test） 后 https://example.test/path",
		"（HTTP+unix://socket/path）", "（7://not-a-scheme）", "（普通://网址）",
		"（https://\xff/路径）", "（https://a）和（ftp://b）", "（https://a（嵌套）",
		"（https://a\n多行）", "（https://a）https://b",
	} {
		check(input)
	}
	random := rand.New(rand.NewSource(1043))
	for range 5000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(units[random.Intn(len(units))])
		}
		check(text.String())
	}
}

func TestQQWithoutLinksAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		maxAllocs   float64
		maxBytes    int64
	}{
		{"long_plain", strings.Repeat("普通消息内容。", 32768), 0, 0},
		{"long_note_short_result", "前（https://example.test/" + strings.Repeat("path/", 1<<15) + "）后", 1, 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocs := testing.AllocsPerRun(20, func() { got = qqWithoutLinks(tc.input) })
			if got != previousQQWithoutLinks(tc.input) {
				t.Fatal("链接清理结果变化")
			}
			if allocs > tc.maxAllocs {
				t.Fatalf("链接清理分配 %.1f 次，超过 %.1f 次预算", allocs, tc.maxAllocs)
			}
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = qqWithoutLinks(tc.input)
				}
			})
			if result.AllocedBytesPerOp() > tc.maxBytes {
				t.Fatalf("链接清理分配 %d 字节，超过 %d 字节预算", result.AllocedBytesPerOp(), tc.maxBytes)
			}
		})
	}
}

func BenchmarkQQWithoutLinks(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"plain", strings.Repeat("普通消息内容。", 4096)},
		{"note", strings.Repeat("普通消息内容。", 4096) + "（https://github.com/metasequoiaime/msime/issues/12）"},
		{"dense_notes", strings.Repeat("普通（https://example.test）消息。", 512)},
	} {
		for _, implementation := range []struct {
			name string
			run  func(string) string
		}{
			{"previous", previousQQWithoutLinks},
			{"current", qqWithoutLinks},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.run(tc.input)
				}
			})
		}
	}
}
