package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// 固定优化前的链接清理，只改函数名，供输出和分配开销对照。
func previousQQFAQRemoveProseLinksBeforeReuse(answer string) (string, bool) {
	if !strings.Contains(answer, "://") {
		return answer, true
	}
	if !strings.ContainsAny(answer, "`~") && !qqFAQCodeNeedsParser(answer) {
		return qqRemoveRawLinks(answer), true
	}
	blocks, valid := qqFAQParsedCode(answer, true)
	if !valid {
		return answer, false
	}
	if len(blocks) == 0 {
		return qqRemoveRawLinks(answer), true
	}
	var out strings.Builder
	out.Grow(len(answer))
	offset := 0
	for _, block := range blocks {
		gap := qqRemoveRawLinks(answer[offset:block.span.start])
		// 删除相邻行内代码之间的链接时保留间隔，避免反引号合成新的分隔符。
		if gap == "" && offset > 0 && answer[offset-1] == '`' && answer[block.span.start] == '`' {
			out.WriteByte(' ')
		}
		out.WriteString(gap)
		out.WriteString(answer[block.span.start:block.span.end])
		offset = block.span.end
	}
	out.WriteString(qqRemoveRawLinks(answer[offset:]))
	return out.String(), true
}

func qqFAQLinkReuseSamples() []struct{ name, answer string } {
	code := "https://example.test/code " + strings.Repeat("合成参数", 600)
	return []struct{ name, answer string }{
		{"LongFence", "说明。\n\n```sh\n" + code + "\n```\n结束。"},
		{"LongInline", "说明。`" + code + "` 结束。"},
		{"ManyBlocks", strings.Repeat("说明。\n\n```sh\nhttps://example.test/code\n```\n\n", 40)},
	}
}

func TestQQFAQLinkReuseAllocationBudget(t *testing.T) {
	for _, tc := range qqFAQLinkReuseSamples() {
		t.Run(tc.name, func(t *testing.T) {
			if utf8.RuneCountInString(tc.answer) > qqFAQRunes {
				t.Fatal("样本超过 FAQ 字数限制")
			}
			measure := func(clean func(string) (string, bool)) testing.BenchmarkResult {
				return testing.Benchmark(func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if got, valid := clean(tc.answer); !valid || got != tc.answer {
							b.Fatal("无正文链接的答案发生变化")
						}
					}
				})
			}
			old, current := measure(previousQQFAQRemoveProseLinksBeforeReuse), measure(qqFAQRemoveProseLinks)
			saved := old.AllocedBytesPerOp() - current.AllocedBytesPerOp()
			t.Logf("链接清理分配：旧版=%d B/op，当前=%d B/op，节省=%d B/op", old.AllocedBytesPerOp(), current.AllocedBytesPerOp(), saved)
			// 仅约束链接清理阶段：保留解析开销，省去至少一份答案输出缓冲。
			if saved < int64(len(tc.answer)) {
				t.Fatalf("节省 %d 字节，不足答案的 %d 字节", saved, len(tc.answer))
			}
		})
	}
}

func checkQQFAQLinkReuse(t *testing.T, answer string) {
	t.Helper()
	got, valid := qqFAQRemoveProseLinks(answer)
	want, wantValid := previousQQFAQRemoveProseLinksBeforeReuse(answer)
	if got != want || valid != wantValid {
		t.Fatalf("链接清理变化：输入=%q，当前=%q/%t，旧版=%q/%t", answer, got, valid, want, wantValid)
	}
}

func TestQQFAQLinkReuseCompatibility(t *testing.T) {
	for _, tc := range qqFAQLinkReuseSamples() {
		checkQQFAQLinkReuse(t, tc.answer)
	}
	for _, code := range []string{
		"`https://example.test/code`", "``https://example.test/code `literal` ``",
		"```sh\nhttps://example.test/code\n```", "> ~~~\n> https://example.test/code\n> ~~~",
		"- ```\n  https://example.test/code\n  ```", "    https://example.test/code",
		"```\nhttps://example.test/unclosed", "`https://example.test/unclosed", "\\`https://example.test/escaped`",
	} {
		for _, prefix := range []string{"", "https://example.test/start ", "https://example.test/start\n\n"} {
			for _, gap := range []string{"", " ", "https://example.test/middle", " https://example.test/middle ", "\n\n"} {
				for _, tail := range []string{"", " https://example.test/end", "\nhttps://example.test/end"} {
					checkQQFAQLinkReuse(t, prefix+code+gap+code+tail)
				}
			}
		}
	}
	for _, s := range []string{"", "普通正文", "https://example.test/plain", "\x00\xff", "`a``b` https://example.test/end"} {
		checkQQFAQLinkReuse(t, s)
	}
	random := rand.New(rand.NewSource(1540))
	units := []string{"普通", "`", "``", "```", "~~~", "\n", "\r\n", "\t", " ", "> ", "- ", "https://example.test/code", "\\", "\x00", "\xff"}
	for range 2000 {
		var answer strings.Builder
		for range random.Intn(60) {
			answer.WriteString(units[random.Intn(len(units))])
		}
		checkQQFAQLinkReuse(t, answer.String())
	}
}

func FuzzQQFAQLinkReuse(f *testing.F) {
	for _, tc := range qqFAQLinkReuseSamples() {
		f.Add(tc.answer)
	}
	for _, s := range []string{"`https://example.test/code`https://example.test/prose`https://example.test/code`", "```\nhttps://example.test/unclosed", "https://example.test/plain", "\x00\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, answer string) {
		if len(answer) > 12000 || utf8.RuneCountInString(answer) > qqFAQRunes {
			t.Skip()
		}
		checkQQFAQLinkReuse(t, answer)
	})
}

func BenchmarkQQFAQLinkReuse(b *testing.B) {
	samples := append(qqFAQLinkReuseSamples(), []struct{ name, answer string }{
		{"ChangedPrefix", "https://example.test/start " + qqFAQLinkReuseSamples()[0].answer},
		{"ChangedTail", qqFAQLinkReuseSamples()[0].answer + " https://example.test/end"},
		{"AdjacentInline", "`https://example.test/code`https://example.test/prose`https://example.test/code`"},
		{"NoURL", strings.Repeat("普通说明。", 100)}, {"NoCode", "说明 https://example.test/prose"},
		{"UnclosedFence", "```\nhttps://example.test/unclosed"},
	}...)
	for _, tc := range samples {
		for _, impl := range []struct {
			name  string
			clean func(string) (string, bool)
		}{{"Previous", previousQQFAQRemoveProseLinksBeforeReuse}, {"Current", qqFAQRemoveProseLinks}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqFAQLinkReuseResult, qqFAQLinkReuseValid = impl.clean(tc.answer)
				}
			})
		}
	}
}

var qqFAQLinkReuseResult string
var qqFAQLinkReuseValid bool
