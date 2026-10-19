package server

import (
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func previousQQFAQCommandMatch(text string) (string, []string) {
	trimmed := strings.TrimSpace(text)
	for _, command := range []struct {
		action  string
		pattern *regexp.Regexp
	}{{"write", qqFAQWriteCommand}, {"ignore", qqFAQIgnoreCommand}, {"view", qqFAQViewCommand}} {
		if match := command.pattern.FindStringSubmatch(trimmed); match != nil {
			return command.action, match
		}
	}
	return "", nil
}

func TestQQFAQCommandPrefixCompatibility(t *testing.T) {
	check := func(text string) {
		t.Helper()
		action, match := qqFAQCommandMatch(text)
		wantAction, wantMatch := previousQQFAQCommandMatch(text)
		if action != wantAction || !slices.Equal(match, wantMatch) || (match == nil) != (wantMatch == nil) {
			t.Fatalf("文档命令识别变化：输入=%q，动作=%q，匹配=%q，期望动作=%q，期望匹配=%q", text, action, match, wantAction, wantMatch)
		}
	}
	for _, verb := range []string{"写进", "写入", "加进", "加到", "放进", "记进", "忽略", "看"} {
		for _, space := range []string{"", " ", "\t\n\f\r", "\v", "\u0085", "\u3000"} {
			for _, body := range []string{"FAQ F3", "faq：#f3", "FaQ＃B 7", "F12", ":＃b 8", "：#F9", "B123456789", "F1234567890", "F0", "F-1", "Ｆ１２", "", "\xffF3"} {
				for _, tail := range []string{"", " 答：补充的答案", ": 内容", "\n多行\n内容\xff", "\u3000内容"} {
					check("\u3000" + verb + space + body + tail + "\t")
				}
			}
		}
		for end := range len(verb) + 1 {
			check(verb[:end] + "FAQ F3")
		}
		for value := range 256 {
			check(string([]byte{byte(value)}) + verb + "FAQ F3")
		}
	}
	for _, tc := range qqFAQCommandPrefixCases() {
		check(tc.input)
	}
	random := rand.New(rand.NewSource(979))
	units := []string{"写", "写进", "写入", "加进", "加到", "放进", "记进", "忽略", "看", "FAQ", "faq", "F", "f", "B", "b", "1", "1234567890", "#", "＃", "：", ":", " ", "\t", "\n", "\u3000", "\ufffd", "\xff", "\xe4\xb8", "普通正文"}
	for range 4000 {
		var text strings.Builder
		for range random.Intn(50) {
			text.WriteString(units[random.Intn(len(units))])
		}
		check(text.String())
	}
}

func qqFAQCommandPrefixCases() []struct{ name, input string } {
	body := strings.Repeat("候选框在哪里", 333)
	return []struct{ name, input string }{
		{"普通短中文", "候选框在哪里？"},
		{"普通ASCII", "Help me"},
		{"两千字正文", body + "？"},
		{"边界空白", "\u3000\u0085" + body + "？\u2002"},
		{"查看候选", "看 F12"},
		{"忽略候选", "忽略：＃b 7"},
		{"写入候选", "写进FAQ F3 答：补充答案"},
		{"写入别名", "记进 faq：#B3 答：补充答案"},
		{"写入长答案", "写进FAQ F3 答：" + body},
		{"动词开头普通消息", "看候选框的位置"},
		{"无效UTF8", "\xff" + body + "？\xe4\xb8"},
	}
}

func TestQQFAQCommandPlainAvoidsAllocation(t *testing.T) {
	for _, tc := range qqSlashNonCommandCases() {
		var action string
		var match []string
		allocations := testing.AllocsPerRun(100, func() { action, match = qqFAQCommandMatch(tc.input) })
		if action != "" || match != nil {
			t.Fatalf("普通消息被识别为文档命令：场景=%s，动作=%q，匹配=%q", tc.name, action, match)
		}
		if allocations != 0 {
			t.Errorf("普通消息文档命令检查分配 %g 次，期望零分配：场景=%s", allocations, tc.name)
		}
	}
}

func BenchmarkQQFAQCommandPrefix(b *testing.B) {
	for _, tc := range qqFAQCommandPrefixCases() {
		for _, impl := range []struct {
			name  string
			match func(string) (string, []string)
		}{
			{"Previous", previousQQFAQCommandMatch}, {"Current", qqFAQCommandMatch},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = impl.match(tc.input)
				}
			})
		}
	}
}
