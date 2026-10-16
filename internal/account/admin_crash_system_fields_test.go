package account

import (
	"strings"
	"testing"
)

// 保留原有字段切片实现，验证逐字段遍历保持系统帧识别结果。
func previousCrashSystemFields(frame string) bool {
	for _, field := range strings.Fields(frame) {
		field = strings.TrimLeft(field, "-+[(")
		for _, prefix := range crashSystemPrefixes {
			if strings.HasPrefix(field, prefix) {
				return true
			}
		}
	}
	return false
}

func TestCrashSystemFields(t *testing.T) {
	for _, tc := range []struct {
		frame string
		want  bool
	}{
		{"", false},
		{" \t\r\n\u2003", false},
		{"input_runtime::page::select", false},
		{"msime keyboard.layout()", false},
		{"kernel32.dll!BaseThreadInitThunk", true},
		{"KERNEL32.DLL!BaseThreadInitThunk", false},
		{"module \t\u2003-+[(std::panicking::begin_panic", true},
		{"module symbol java.util.arraylist.get", true},
		{"module ]std::panicking::begin_panic", false},
		{"module (application::select)", false},
		{"\xff std::panicking::begin_panic", true},
		{"\xffstd::panicking::begin_panic", false},
		{"\u200bstd::panicking::begin_panic", false},
		{"module\u00a0\u0085std::panicking::begin_panic", true},
	} {
		if got := crashSystemFrame(tc.frame); got != tc.want {
			t.Errorf("帧 %q，系统帧=%t，期望 %t", tc.frame, got, tc.want)
		}
	}
	for _, prefix := range crashSystemPrefixes {
		for _, separator := range []string{" ", "\t\r\n", "\u0085\u00a0\u2003\u3000", "\xff "} {
			for _, frame := range []string{
				prefix,
				"-+[(" + prefix + "symbol" + separator + "application",
				"application" + separator + "-+[(" + prefix + "symbol",
				"application" + separator + "x" + prefix,
			} {
				if got, want := crashSystemFrame(frame), previousCrashSystemFields(frame); got != want {
					t.Errorf("帧 %q，系统帧=%t，原结果 %t", frame, got, want)
				}
			}
		}
	}
}

func TestCrashSystemFieldsAllocations(t *testing.T) {
	for _, frame := range []string{
		"input_runtime::page::select",
		"std::panicking::begin_panic",
		"module symbol\u2003std::panicking::begin_panic",
		"std::panicking::begin_panic" + strings.Repeat(" a", 4000),
	} {
		want := previousCrashSystemFields(frame)
		var got bool
		allocations := testing.AllocsPerRun(1000, func() {
			got = crashSystemFrame(frame)
		})
		if got != want {
			t.Fatalf("系统帧=%t，原结果 %t", got, want)
		}
		if allocations != 0 {
			t.Errorf("帧长度 %d，分配 %.0f 次，期望零分配", len(frame), allocations)
		}
	}
}

func BenchmarkCrashSystemFields(b *testing.B) {
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"应用帧", "msime keyboard.layout()"},
		{"系统帧", "module std::panicking::begin_panic"},
		{"Unicode分隔", "module\u2003std::panicking::begin_panic"},
		{"首字段命中长后缀", "std::panicking::begin_panic" + strings.Repeat(" a", 4000)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			want := previousCrashSystemFields(tc.frame)
			b.ReportAllocs()
			for b.Loop() {
				if got := crashSystemFrame(tc.frame); got != want {
					b.Fatalf("系统帧=%t，原结果 %t", got, want)
				}
			}
		})
	}
}
