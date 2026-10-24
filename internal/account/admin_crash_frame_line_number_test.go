package account

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// 原行号正则仅用于兼容性对照，生产流程不再编译它。
var crashFrameLineNumber = regexp.MustCompile(`(?::[0-9]+)+`)

// 冻结行号扫描前的帧规范化和签名流程，验证已有崩溃分组保持一致。
func previousNormalizeCrashFrameLineNumber(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	// 这些替换都要求 ASCII 数字或正则的 ASCII 空白；纯符号帧直接转小写，避免五次整行复制。
	if !strings.ContainsAny(line, "0123456789 \t\n\f\r") {
		return strings.ToLower(line)
	}
	if line[0] >= '0' && line[0] <= '9' || line[0] == '#' || strings.HasPrefix(line, "at") {
		line = trimCrashFrameIndex(line)
	}
	// 只在存在必要标记时调用正则；未匹配的 ReplaceAllString 也会复制整行。
	if strings.Contains(line, "0x") || strings.Contains(line, "0X") {
		line = crashAddressPattern.ReplaceAllString(line, "")
	}
	if strings.ContainsRune(line, '+') {
		line = trimCrashFrameOffset(line)
	}
	if strings.ContainsRune(line, ':') {
		line = crashFrameLineNumber.ReplaceAllString(line, "")
	}
	return normalizeCrashWhitespace(line)
}

func previousCrashFrameLineNumberSignature(platform, message, stack string) string {
	normalized := normalizeCrashMessage(message)
	frame := ""
	previousLine, previousFrame := "", ""
	previousFrameValid := false
	lineCount := 0
	for line := range strings.SplitSeq(stack, "\n") {
		lineCount++
		candidate := ""
		if previousFrameValid && line == previousLine {
			candidate = previousFrame
		} else {
			candidate = previousNormalizeCrashFrameLineNumber(line)
			previousLine, previousFrame, previousFrameValid = line, candidate, true
		}
		if candidate != "" && !crashSystemFrame(candidate) {
			frame = candidate
			break
		}
		if lineCount == crashStackLines {
			break
		}
	}
	if normalized == "" && frame == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(crashPlatform(platform) + "\n" + normalized + "\n" + frame))
	return hex.EncodeToString(sum[:8])
}

func checkCrashFrameLineNumberCompatibility(t *testing.T, text string) {
	t.Helper()
	if got, want := stripCrashFrameLineNumbers(text), crashFrameLineNumber.ReplaceAllString(text, ""); got != want {
		t.Fatalf("输入=%q，行号结果=%q，正则结果=%q", text, got, want)
	}
	if got, want := normalizeCrashFrame(text), previousNormalizeCrashFrameLineNumber(text); got != want {
		t.Fatalf("输入=%q，结果=%q，原结果=%q", text, got, want)
	}
	stack := "at java.util.List.get(List.java:12)\n" + text + "\nat msime.Main.run(Main.java:34)"
	if got, want := crashSignature("Android", "synthetic crash 123", stack), previousCrashFrameLineNumberSignature("Android", "synthetic crash 123", stack); got != want {
		t.Fatalf("栈帧=%q，签名=%q，原签名=%q", text, got, want)
	}
}

func TestCrashFrameLineNumberCompatibility(t *testing.T) {
	for _, text := range []string{
		"", ":", ":0", ":0012", ":12:34", "::12", ":12::34", ":12:34:", ":12x:34", ":12 34", ":12 :34",
		"module:12", "module:12:34", "module(:12)", "module:12symbol:34", "module:12:foo", "module::symbol42",
		"module:١٢", "module:１２", "module:1٢3", "module:\xff12", "module:12\xff", "module\x00:12",
		"module:\t12", "module:\n12", "module:\v12", "module:\u00a012", "module：12", "module:12\u3000:34",
		"at java.util.List.get(List.java:123:45)", "#1: 0x10 module!symbol + 12", "std::panicking::begin_panic42",
		"水杉:12页面:34", strings.Repeat(":", 1000), "module:" + strings.Repeat("9", 10000) + ")",
	} {
		checkCrashFrameLineNumberCompatibility(t, text)
	}
	random := rand.New(rand.NewSource(1492))
	atoms := []string{":", "::", "0", "12", "123456789", " ", "\t", "\n", "\r", "\f", "\v", "\u00a0", "\u3000", "水", "\xff", "\xfe", "\x00", "module42", "#", "at", "+", "0x"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(60) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkCrashFrameLineNumberCompatibility(t, text.String())
	}
}

func TestCrashFrameLineNumberBytePairs(t *testing.T) {
	check := func(text string) {
		t.Helper()
		if got, want := stripCrashFrameLineNumbers(text), crashFrameLineNumber.ReplaceAllString(text, ""); got != want {
			t.Fatalf("输入=%q，行号结果=%q，正则结果=%q", text, got, want)
		}
	}
	// 穷举行号数字段前后的全部字节对，包含控制字符与非法 UTF-8。
	for first := range 256 {
		for second := range 256 {
			pair := string([]byte{byte(first), byte(second)})
			check("module:" + pair + "12)")
			check("module:12" + pair + ":34)")
		}
	}
	for c := range 256 {
		one := string([]byte{byte(c)})
		for _, text := range []string{one + ":12", ":12" + one, "module" + one + ":12", "module:" + one, "module:" + one + ":12"} {
			check(text)
		}
	}
}

func TestCrashFrameLineNumberAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		text    string
		maximum float64
	}{
		{"module::symbol42", 0}, {"module:foo42", 0}, {"module:١٢42", 0}, {"module:\xff42", 1},
		{"module(:12)", 1}, {"module:12:34", 1}, {"module:12symbol:34", 1}, {":12:34", 0},
	} {
		t.Run(tc.text, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = normalizeCrashFrame(tc.text) })
			if want := previousNormalizeCrashFrameLineNumber(tc.text); got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations > tc.maximum {
				t.Fatalf("分配 %.0f 次，预算最多 %.0f 次", allocations, tc.maximum)
			}
		})
	}
}

func TestCrashFrameLineNumberOutputSizeBudget(t *testing.T) {
	text := "module:" + strings.Repeat("9", 10000) + ")"
	measured := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			crashFrameLineNumberResult = normalizeCrashFrame(text)
		}
	})
	if crashFrameLineNumberResult != "module)" {
		t.Fatalf("结果=%q", crashFrameLineNumberResult)
	}
	if bytes := measured.AllocedBytesPerOp(); bytes > 64 {
		t.Fatalf("输出只有 7 字节，却分配 %d 字节，预算为 64 字节", bytes)
	}
}

func FuzzCrashFrameLineNumberCompatibility(f *testing.F) {
	for _, text := range []string{"", ":12:34", "module::symbol42", "module:\xff12", "module:١٢", "module:12\x00:34", "module:" + strings.Repeat("9", 1000)} {
		f.Add(text)
	}
	f.Fuzz(checkCrashFrameLineNumberCompatibility)
}

var crashFrameLineNumberResult string

func BenchmarkCrashFrameLineNumberNormalization(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Plain", "module symbol42"}, {"Unmatched", "module::symbol42"}, {"Java", "at java.util.List.get(List.java:123:45)"},
		{"Multiple", "module:12symbol:34"}, {"LongDigits", "module:" + strings.Repeat("9", 1000) + ")"}, {"LongSymbol", strings.Repeat("module", 1000) + "::symbol42"},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{
			{"Previous", previousNormalizeCrashFrameLineNumber}, {"Current", normalizeCrashFrame},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashFrameLineNumberResult = impl.normalize(tc.text)
				}
			})
		}
	}
}

func BenchmarkCrashFrameLineNumberSignature(b *testing.B) {
	var java, rust strings.Builder
	for i := range 64 {
		fmt.Fprintf(&java, "at java.util.Part%d.get(Part.java:%d)\n", i, i+10)
		fmt.Fprintf(&rust, "std::panicking::part%d\n", i)
	}
	for _, tc := range []struct{ name, platform, stack string }{
		{"Plain", "ios", "input_runtime::page::select"},
		{"Java64", "android", java.String() + "at msime.Main.run(Main.java:123)"},
		{"Rust64", "linux", rust.String() + "input_runtime::page::select42"},
	} {
		for _, impl := range []struct {
			name      string
			signature func(string, string, string) string
		}{
			{"Previous", previousCrashFrameLineNumberSignature}, {"Current", crashSignature},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashFrameLineNumberResult = impl.signature(tc.platform, "synthetic crash", tc.stack)
				}
			})
		}
	}
}
