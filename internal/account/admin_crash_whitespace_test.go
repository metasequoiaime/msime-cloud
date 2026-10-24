package account

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// 旧正则只供兼容性对照使用，避免在服务启动时为测试基线编译它。
var crashSpaces = regexp.MustCompile(`\s+`)

// 保留原有的正则替换、Unicode 首尾裁剪和小写转换顺序。
func previousNormalizeCrashWhitespace(text string) string {
	return strings.ToLower(strings.TrimSpace(crashSpaces.ReplaceAllString(text, " ")))
}

func TestCrashWhitespaceCompatibility(t *testing.T) {
	check := func(text string) {
		t.Helper()
		if got, want := normalizeCrashWhitespace(text), previousNormalizeCrashWhitespace(text); got != want {
			t.Fatalf("输入=%q，结果=%q，原结果=%q", text, got, want)
		}
	}
	for _, text := range []string{
		"", " ", "\t\n\f\r ", "a b", "a  b", "a\tb", "a\vb", "a \v b",
		"\v a\v\tb \v", "\u3000\tMSIME\t\u3000", "水杉\u3000输入法",
		"\u00a0\t\vİ\u212aΣ\u00a0", "\xff\t\xfe", "a\x00\tb", "A\xff\tB",
		strings.Repeat("\t ", 8000), strings.Repeat("x \t", 5000) + "X",
	} {
		check(text)
	}
	// 穷举字节对，覆盖非法 UTF-8、控制字符和多字节序列边界。
	for first := range 256 {
		for second := range 256 {
			check("A" + string([]byte{byte(first), byte(second)}) + " B")
		}
	}
	random := rand.New(rand.NewSource(1475))
	atoms := []string{"a", "B", "0x10", "水杉", "İ", "Σ", "\u212a", "\xff", "\xfe", "\x00", " ", "\t", "\n", "\r", "\v", "\f", "\u0085", "\u00a0", "\u1680", "\u2000", "\u200b", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		check(text.String())
	}
}

func TestCrashWhitespaceAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		maximum    float64
	}{
		{"单空格", "synthetic crash message", 0},
		{"Unicode空白", "水杉\u3000输入法", 0},
		{"仅裁剪", "\u3000 synthetic crash \t", 0},
		{"内部垂直制表符", "synthetic\vcrash message", 0},
		{"折叠空白", "synthetic\t crash  message", 1},
		{"密集空白", strings.Repeat("x \t", 5000) + "x", 1},
		{"大小写转换", "Synthetic\t CRASH  Message", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := previousNormalizeCrashWhitespace(tc.text)
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = normalizeCrashWhitespace(tc.text) })
			if got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations > tc.maximum {
				t.Fatalf("分配 %.0f 次，超过 %.0f 次预算", allocations, tc.maximum)
			}
		})
	}
}

func TestCrashWhitespaceOutputSizeBudget(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		maximum    int64
	}{
		{"长空格段", "a" + strings.Repeat(" ", 15998) + "b", 256},
		{"多段控制空白", "a" + strings.Repeat("\t ", 3000) + "b" + strings.Repeat("\r\n", 3000) + "c", 256},
		{"长空白后有正文", "a" + strings.Repeat(" ", 8000) + strings.Repeat("x", 8000), 9000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := previousNormalizeCrashWhitespace(tc.text)
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					if got := normalizeCrashWhitespace(tc.text); got != want {
						t.Fatalf("结果=%q，原结果=%q", got, want)
					}
				}
			})
			// 缓冲随折叠后的输出增长，不能按将被删除的长空白段预留。
			if bytes := result.AllocedBytesPerOp(); bytes > tc.maximum {
				t.Fatalf("分配 %d B/op，超过 %d B/op 预算", bytes, tc.maximum)
			}
		})
	}
}

func FuzzCrashWhitespace(f *testing.F) {
	for _, text := range []string{"", "synthetic crash", "\tA\vB\u3000 C \n", "\xff\tİ\xfe", "a \v b"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if got, want := normalizeCrashWhitespace(text), previousNormalizeCrashWhitespace(text); got != want {
			t.Fatalf("输入=%q，结果=%q，原结果=%q", text, got, want)
		}
	})
}

var crashWhitespaceResult string

func BenchmarkCrashWhitespace(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Plain", "synthetic crash message"},
		{"Tabs", "synthetic\t crash  message"},
		{"Dense", strings.Repeat("x \t", 5000) + "x"},
		{"Sparse", "a" + strings.Repeat(" ", 15998) + "b"},
		{"Unicode", "\u3000水杉\t 输入法\u3000"},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{{"Previous", previousNormalizeCrashWhitespace}, {"Current", normalizeCrashWhitespace}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashWhitespaceResult = impl.normalize(tc.text)
				}
			})
		}
	}
}

func BenchmarkCrashWhitespaceSignature(b *testing.B) {
	var java, native strings.Builder
	for i := range 64 {
		fmt.Fprintf(&java, "at java.util.Part%d.get(Part.java:%d)\n", i, i+10)
		fmt.Fprintf(&native, "%d libsystem_kernel.dylib 0x%x __pthread_kill + %d\n", i, i+4096, i+8)
	}
	for _, tc := range []struct{ name, platform, message, stack string }{
		{"Plain", "ios", "synthetic crash", "input_runtime::page::select"},
		{"Java64", "android", "synthetic crash 123", java.String() + "at msime.Main.run(Main.java:123)"},
		{"Native64", "macos", "EXC_BAD_ACCESS at 0x10", native.String() + "65 MSIME 0x1000 Keyboard.layout() + 1"},
		{"Whitespace", "windows", "synthetic\t crash  123", "at\tmsime.Main.run(Main.java:123)"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				crashWhitespaceResult = crashSignature(tc.platform, tc.message, tc.stack)
			}
		})
	}
}
