package server

import (
	"context"
	"math/rand"
	"strings"
	"testing"
)

func TestQQSlashIgnoredArgumentsAllocationBudget(t *testing.T) {
	tail := strings.Repeat("正文 ", 10000)
	for _, tc := range []struct{ command, want, reply string }{
		{"/docs ", "cmd_slash_docs", "使用文档："},
		{"/mcp ", "cmd_slash_mcp", qqMCPHelpText},
		{"/Library/Input Methods ", "", ""},
		{"/draft ", "cmd_slash_draft", "只有管理员能用 /draft。"},
		{"/q ", "cmd_slash_question", "只有管理员能用 /q。"},
		{"/undraft 12 ", "cmd_draft_dismiss", "只有管理员能忽略草稿。"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			s := &Server{}
			input := tc.command + tail
			var got, reply string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					reply = ""
					got = s.qqSlashCommand(context.Background(), qqGroupMessage{}, input, false, func(text string) { reply = text }, func(string) {})
				}
			})
			if got != tc.want || !strings.HasPrefix(reply, tc.reply) || tc.reply == "" && reply != "" {
				t.Fatalf("命令结果=%q，回复=%q", got, reply)
			}
			if result.AllocedBytesPerOp() > 16<<10 {
				t.Fatalf("忽略的参数不应构造完整字段切片：%s", result.MemString())
			}
		})
	}
}

func BenchmarkQQSlashArgumentHandling(b *testing.B) {
	for _, command := range []string{"/docs ", "/mcp ", "/Library/Input Methods ", "/draft ", "/q "} {
		b.Run(command, func(b *testing.B) {
			s := &Server{}
			input := command + strings.Repeat("正文 ", 10000)
			b.ReportAllocs()
			for b.Loop() {
				_ = s.qqSlashCommand(context.Background(), qqGroupMessage{}, input, false, func(string) {}, func(string) {})
			}
		})
	}
}

func TestQQSlashArgumentStreamingMatchesFields(t *testing.T) {
	texts := []string{"", " ", "12", "12  答案\t正文", "\u0085#12\u3000答案\n正文", "\xff D12 \xe6 \x8f \x90", "win  #12", "win\t关键词\u00a0正文"}
	random := rand.New(rand.NewSource(658))
	units := []string{"正文", "win", "D12", "#13", "F2", "＃14", "a", " ", "\t", "\n", "\r", "\v", "\f", "\u0085", "\u00a0", "\u3000", "\ufffd", "\xff", "\xe6", "\x8f", "\x90", "\x00"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(50) {
			text.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		fields := strings.Fields(text)
		wantHead, wantTail := "", ""
		if len(fields) > 0 {
			wantHead, wantTail = fields[0], strings.Join(fields[1:], " ")
		}
		head, tail := qqSlashArgumentHead(text)
		if head != wantHead || qqNormalizeWhitespace(tail) != wantTail {
			t.Fatalf("首参数或剩余正文变化：输入=%q，首参数=%q，剩余=%q", text, head, tail)
		}
		if got, want := qqNormalizeWhitespace(text), strings.Join(fields, " "); got != want {
			t.Fatalf("参数空白合并变化：输入=%q，结果=%q，期望=%q", text, got, want)
		}
	}
}

func TestQQSlashArgumentTextAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		budget     int64
	}{
		{"密集参数", strings.Repeat("正文 ", 10000), 86 << 10},
		{"大量空白", strings.Repeat(" ", 1<<20) + "答案" + strings.Repeat("\u3000", 1<<18), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := strings.Join(strings.Fields(tc.args), " ")
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqNormalizeWhitespace(tc.args)
				}
			})
			if got != want || result.AllocedBytesPerOp() > tc.budget {
				t.Fatalf("参数正文变化或超出分配预算：结果长度=%d，期望长度=%d，分配=%s，预算=%d", len(got), len(want), result.MemString(), tc.budget)
			}
		})
	}
}

func BenchmarkQQSlashArgumentText(b *testing.B) {
	args := strings.Repeat("正文 ", 10000)
	for _, implementation := range []struct {
		name string
		run  func(string) string
	}{
		{"previous", func(text string) string { return strings.Join(strings.Fields(text), " ") }},
		{"current", qqNormalizeWhitespace},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = implementation.run(args)
			}
		})
	}
}
