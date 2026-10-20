package server

import (
	"context"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// 冻结本次优化前的表达式与捕获结果提取，避免差分和基准跟着实现改变。
var qqSlashNumberBeforeScan = regexp.MustCompile(`^[#＃]?[DdFfBb]?([0-9]{1,9})$`)

func qqSlashParseNumberBeforeScan(text string) (string, bool) {
	match := qqSlashNumberBeforeScan.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	return match[1], true
}

func TestQQSlashNumberWithoutAllocations(t *testing.T) {
	for _, input := range []string{"0", "000000000", "999999999", "#818", "＃b00012", "F3", "", "#", "＃", "D", "＃D", "##12", "#＃12", "DD12", "1234567890", "D１２", "12\xff", strings.Repeat("9", 1<<16)} {
		t.Run(input[:min(len(input), 24)], func(t *testing.T) {
			want, wantOK := qqSlashParseNumberBeforeScan(input)
			var got string
			var ok bool
			allocs := testing.AllocsPerRun(1000, func() {
				got, ok = qqSlashParseNumber(input)
			})
			if got != want || ok != wantOK {
				t.Fatalf("编号解析变化：输入=%q，实际=(%q,%v)，期望=(%q,%v)", input, got, ok, want, wantOK)
			}
			if allocs != 0 {
				t.Fatalf("编号解析不应分配内存：分配次数=%g", allocs)
			}
		})
	}
}

func TestQQSlashNumberMatchesPrevious(t *testing.T) {
	check := func(input string) {
		t.Helper()
		got, ok := qqSlashParseNumber(input)
		want, wantOK := qqSlashParseNumberBeforeScan(input)
		if got != want || ok != wantOK {
			t.Fatalf("编号解析变化：输入=%q，实际=(%q,%v)，期望=(%q,%v)", input, got, ok, want, wantOK)
		}
	}
	for _, hash := range []string{"", "#", "＃", "##", "＃＃", "#＃", "＃#"} {
		for _, marker := range []string{"", "D", "d", "F", "f", "B", "b", "DD", "dF", "X", "Ｄ", "#D", "＃D"} {
			for _, digits := range []string{"", "0", "0012", "12345678", "123456789", "1234567890", "１２", "١٢", "1.2", "-1", "+1", "1\x00", "1\xff", "1\n"} {
				input := hash + marker + digits
				check(input)
				check(" " + input)
				check(input + "\u3000")
			}
		}
	}
	for _, prefix := range []string{"", "#D", "＃f"} {
		for pos := 0; pos <= 8; pos++ {
			for b := 0; b < 256; b++ {
				check(prefix + strings.Repeat("1", pos) + string([]byte{byte(b)}) + strings.Repeat("2", 8-pos))
			}
		}
	}
	rng := rand.New(rand.NewPCG(101, 137))
	units := []string{"#", "＃", "D", "d", "F", "f", "B", "b", "0", "1", "9", "２", " ", "\t", "\r", "\n", "\x00", "\xff", "\xef", "\xbc", "\x83"}
	for range 4096 {
		var input strings.Builder
		for range rng.IntN(16) {
			input.WriteString(units[rng.IntN(len(units))])
		}
		check(input.String())
	}
}

func TestQQSlashNumberCallers(t *testing.T) {
	s := &Server{qq: &qqBot{config: QQBotConfig{Repo: "o/msime"}}}
	for _, args := range []string{"00012", "#D00012", "＃f00012", "msime #b00012"} {
		got := s.qqSlashIssues(context.Background(), args, func(string) { t.Fatal("编号查询不应走搜索回复") }, func(command string) string { return command })
		want := "#00012"
		if strings.HasPrefix(args, "msime ") {
			want = "msime #00012"
		}
		if got != want {
			t.Fatalf("issue 编号改写变化：输入=%q，实际=%q，期望=%q", args, got, want)
		}
	}
	for _, command := range []string{"/undraft 12", "/undraft #D12", "/undraft ＃f12", "/undraft #Ｂ１２"} {
		var reply string
		got := s.qqSlashCommand(context.Background(), qqGroupMessage{}, command, false, func(text string) { reply = text }, func(string) {})
		if got != "cmd_draft_dismiss" || reply != "只有管理员能忽略草稿。" {
			t.Fatalf("草稿编号命令或权限变化：输入=%q，结果=%q，回复=%q", command, got, reply)
		}
	}
}

func BenchmarkQQSlashNumberParsing(b *testing.B) {
	for _, input := range []string{"12", "#D00012", "＃b123456789", "1234567890", "DD12", "＃１２", strings.Repeat("9", 1<<16)} {
		for _, impl := range []struct {
			name string
			run  func(string) (string, bool)
		}{{"previous", qqSlashParseNumberBeforeScan}, {"current", qqSlashParseNumber}} {
			b.Run(input[:min(len(input), 24)]+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = impl.run(input)
				}
			})
		}
	}
}

func BenchmarkQQSlashNumberCommands(b *testing.B) {
	s := &Server{qq: &qqBot{config: QQBotConfig{Repo: "o/msime"}}}
	b.Run("undraft", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.qqSlashCommand(context.Background(), qqGroupMessage{}, "/undraft D12", false, func(string) {}, func(string) {})
		}
	})
	b.Run("issues", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.qqSlashIssues(context.Background(), "#D12", func(string) {}, func(command string) string { return command })
		}
	})
}
