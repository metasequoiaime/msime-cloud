package account

import (
	"strings"
	"testing"
)

// 保留原有规范化步骤，验证空行快速返回不改变栈帧内容。
func previousNormalizeCrashEmptyFrame(line string) string {
	line = strings.TrimSpace(line)
	line = crashFrameIndex.ReplaceAllString(line, "")
	line = crashAddressPattern.ReplaceAllString(line, "")
	line = crashFrameOffset.ReplaceAllString(line, "")
	line = crashFrameLineNumber.ReplaceAllString(line, "")
	return strings.ToLower(strings.TrimSpace(crashSpaces.ReplaceAllString(line, " ")))
}

func TestNormalizeCrashEmptyFrame(t *testing.T) {
	for _, line := range []string{
		"", " ", "\t\r\n\v\f", "\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000",
		"\u200b", "\ufeff", "\x00", "\xff", " \xff ", " at ", "0x10", "#1: 0x10 + 2", "\u2003Keyboard.layout()\u2003", "\t2 MSIME 0x01 Keyboard.layout() + 1\r", "at java.util.ArrayList.get(ArrayList.java:437)",
	} {
		if got, want := normalizeCrashFrame(line), previousNormalizeCrashEmptyFrame(line); got != want {
			t.Errorf("输入 %q，规范化为 %q，原结果 %q", line, got, want)
		}
	}
}

func TestNormalizeCrashWhitespace(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"  Foo\t Bar\n", "foo bar"},
		{"水杉\u3000输入法", "水杉\u3000输入法"},
		{"", ""},
	} {
		if got := normalizeCrashWhitespace(tc.input); got != tc.want {
			t.Errorf("输入 %q，规范化为 %q，期望 %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeCrashEmptyFrameAllocations(t *testing.T) {
	for _, line := range []string{"", "\t\r\n ", strings.Repeat("\u2003", 1024)} {
		var frame string
		allocations := testing.AllocsPerRun(1000, func() {
			frame = normalizeCrashFrame(line)
		})
		if frame != "" {
			t.Fatalf("空白行规范化为 %q", frame)
		}
		if allocations != 0 {
			t.Errorf("空白行长度 %d，分配 %.0f 次，期望零分配", len(line), allocations)
		}
	}
}

func BenchmarkNormalizeCrashEmptyFrame(b *testing.B) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"空行", ""},
		{"ASCII空白", "\t\r\n "},
		{"Unicode空白", strings.Repeat("\u2003", 32)},
		{"应用帧", "2 MSIME 0x01 Keyboard.layout() + 1"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			want := previousNormalizeCrashEmptyFrame(tc.line)
			b.ReportAllocs()
			for b.Loop() {
				if got := normalizeCrashFrame(tc.line); got != want {
					b.Fatalf("规范化为 %q，原结果 %q", got, want)
				}
			}
		})
	}
}
