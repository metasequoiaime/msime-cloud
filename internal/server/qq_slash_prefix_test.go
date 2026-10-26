package server

import (
	"context"
	"math/rand"
	"strings"
	"testing"
)

func referenceQQSlashParse(text string) (name, args string, ok bool) {
	text = strings.TrimSpace(qqFoldWidth(text))
	rest, found := strings.CutPrefix(text, "/")
	if !found || rest == "" || !(rest[0] >= 'a' && rest[0] <= 'z' || rest[0] >= 'A' && rest[0] <= 'Z') {
		return "", "", false
	}
	name, args, _ = strings.Cut(rest, " ")
	if i := strings.IndexAny(name, "\n\t"); i >= 0 {
		name, args = name[:i], name[i+1:]+" "+args
	}
	return strings.ToLower(name), strings.TrimSpace(args), true
}

func qqSlashNonCommandCases() []struct{ name, input string } {
	body := strings.Repeat("候选框在哪里", 333)
	return []struct{ name, input string }{
		{"短中文", "候选框在哪里？"},
		{"短ASCII", "Help me"},
		{"两千字中文", body + "？"},
		{"两千字无全角", body},
		{"正文斜杠", body + " /help"},
		{"全角正文", "ＨＥＬＰ　" + body},
		{"Unicode边界空白", "\u0085\u00a0\u3000" + body + "？\u2002\t"},
		{"无效UTF8", "\xff" + body + "？\xe4\xb8"},
	}
}

func TestQQSlashNonCommandAvoidsAllocation(t *testing.T) {
	for _, tc := range qqSlashNonCommandCases() {
		t.Run(tc.name, func(t *testing.T) {
			var name, args string
			var ok bool
			allocations := testing.AllocsPerRun(100, func() {
				name, args, ok = qqSlashParse(tc.input)
			})
			if ok || name != "" || args != "" {
				t.Fatalf("普通消息被识别为斜杠命令：名称=%q，参数=%q，命中=%t", name, args, ok)
			}
			if allocations != 0 {
				t.Fatalf("排除普通消息分配 %.1f 次，期望零分配", allocations)
			}
		})
	}
}

func TestQQSlashPrefixMatchesPrevious(t *testing.T) {
	check := func(input string) {
		t.Helper()
		name, args, ok := qqSlashParse(input)
		wantName, wantArgs, wantOK := referenceQQSlashParse(input)
		if name != wantName || args != wantArgs || ok != wantOK {
			t.Fatalf("斜杠解析不兼容：输入=%q，结果=(%q,%q,%t)，期望=(%q,%q,%t)", input, name, args, ok, wantName, wantArgs, wantOK)
		}
	}
	for r := rune(0xFF01); r <= 0xFF5E; r++ {
		check(string(r) + "Help 参数")
		check("／" + string(r) + " 参数")
	}
	for _, space := range []string{"", " ", "\t\n\v\f\r", "\u0085", "\u00a0", "\u1680", "\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a", "\u2028\u2029", "\u202f", "\u205f", "\u3000", "\ufeff", "\u200b"} {
		for _, slash := range []string{"/", "／", "∕", "⁄", "\xff", "\xef\xbc"} {
			check(space + slash + "Ｈｅｌｐ 参数？\xff" + space)
			check(space + "候选框在哪里" + slash + "help" + space)
		}
	}
	units := []string{"", "/", "／", "∕", "⁄", "h", "H", "help", "ＨＥＬＰ", "ａ", "Ａ", "K", "ǅ", "水", "杉", "？", "🙏", " ", "\t", "\n", "\v", "\u0085", "\u00a0", "\u3000", "\u200b", "\ufffd", "\xff", "\xe4\xb8", "\xc0\xaf"}
	random := rand.New(rand.NewSource(956))
	for range 4000 {
		var input strings.Builder
		for range random.Intn(50) {
			input.WriteString(units[random.Intn(len(units))])
		}
		text := input.String()
		check(text)
		check(" /HeLP " + text)
		check("\u3000／Ｈｅｌｐ\t" + text + "\u3000")
	}
}

func BenchmarkQQSlashPrefix(b *testing.B) {
	cases := append(qqSlashNonCommandCases(), []struct{ name, input string }{
		{"半角命令", "/Help 参数"},
		{"全角命令", "\u3000／Ｈｅｌｐ 参数？\u3000"},
		{"命令内无效UTF8", "/help 内容\xff\xe4\xb8"},
		{"命令长参数", "/doc " + strings.Repeat("候选框在哪里？", 285)},
	}...)
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _, _ = qqSlashParse(tc.input)
			}
		})
	}
}

func BenchmarkQQNonSlashCommandChecks(b *testing.B) {
	s := &Server{}
	ctx := context.Background()
	reply := func(string) {}
	for _, tc := range qqSlashNonCommandCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				compact, _ := qqFreeCommand(tc.input)
				_ = s.qqCommand(ctx, botMessage{}, tc.input, compact, false, reply, reply)
			}
		})
	}
}
