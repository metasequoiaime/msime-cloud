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

// 原偏移正则仅用于兼容性对照，生产流程不再编译它。
var crashFrameOffset = regexp.MustCompile(`\s*\+\s*[0-9]+\s*$`)

// 冻结反向扫描前的帧规范化流程，隔离本次尾部偏移优化的收益。
func previousNormalizeCrashFrameOffset(line string) string {
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
		line = crashFrameOffset.ReplaceAllString(line, "")
	}
	if strings.ContainsRune(line, ':') {
		line = crashFrameLineNumber.ReplaceAllString(line, "")
	}
	return normalizeCrashWhitespace(line)
}

func previousCrashFrameOffsetSignature(platform, message, stack string) string {
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
			candidate = previousNormalizeCrashFrameOffset(line)
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

func checkCrashFrameOffsetCompatibility(t *testing.T, text string) {
	t.Helper()
	if got, want := trimCrashFrameOffset(text), crashFrameOffset.ReplaceAllString(text, ""); got != want {
		t.Fatalf("输入=%q，偏移结果=%q，正则结果=%q", text, got, want)
	}
	if got, want := normalizeCrashFrame(text), previousNormalizeCrashFrameOffset(text); got != want {
		t.Fatalf("栈帧=%q，结果=%q，原结果=%q", text, got, want)
	}
	stack := "0 libsystem_kernel.dylib 0x10 __pthread_kill + 8\n" + text + "\n2 msime 0x100 keyboard.layout() + 12"
	if got, want := crashSignature("Darwin", "EXC_BAD_ACCESS at 0x10", stack), previousCrashFrameOffsetSignature("Darwin", "EXC_BAD_ACCESS at 0x10", stack); got != want {
		t.Fatalf("栈帧=%q，签名=%q，原签名=%q", text, got, want)
	}
}

func TestCrashFrameOffsetCompatibility(t *testing.T) {
	for _, text := range []string{
		"", " ", "+", "+0", "+ 0", " + 0 ", "module +0", "module + 12", "module+12", "module +0012", "module -12",
		"module ++12", "module +1 +2", "module +1 +2 3", "module + 12 34", "module + + 12", "module +12foo", "module +foo12", "module + ١٢", "module +１２",
		"module\t+\t12\t", "module\f+\f12\f", "module\r+\r12\r", "module\n+\n12\n", "module\v+12", "module +\v12", "module +12\v",
		"module\u00a0+12", "module +\u00a012", "module +12\u00a0", "module\u3000+12", "module +\u300012", "module +12\u3000",
		"\xffmodule +12", "module\xff+12", "module +\xff12", "module +12\xff", "module +12\x00", "module +12\v \t", "at module.fn(file:42) + 12",
		"#12 0x10 module!symbol + 8", "+" + strings.Repeat("9", 10000), "module" + strings.Repeat(" \t", 1000) + "+ 12", strings.Repeat("module", 1000) + " +1",
	} {
		checkCrashFrameOffsetCompatibility(t, text)
	}
	random := rand.New(rand.NewSource(1490))
	atoms := []string{"+", "-", "0", "12", "123456789", " ", "\t", "\n", "\r", "\f", "\v", "\u00a0", "\u3000", "水", "\xff", "\xfe", "\x00", "module42", "#", "at", ":"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(60) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkCrashFrameOffsetCompatibility(t, text.String())
	}
}

func TestCrashFrameOffsetBytePairs(t *testing.T) {
	check := func(text string) {
		t.Helper()
		if got, want := trimCrashFrameOffset(text), crashFrameOffset.ReplaceAllString(text, ""); got != want {
			t.Fatalf("输入=%q，偏移结果=%q，正则结果=%q", text, got, want)
		}
	}
	// 穷举偏移内部和尾部的所有字节对，包含控制字符与非法 UTF-8。
	for first := range 256 {
		for second := range 256 {
			pair := string([]byte{byte(first), byte(second)})
			check("module +12" + pair)
			check("module +" + pair + "12")
		}
	}
	for c := range 256 {
		one := string([]byte{byte(c)})
		for _, text := range []string{"module" + one + "+12", "module +" + one, "module " + one + "12", "module +12" + one + " ", one + "+12"} {
			check(text)
		}
	}
}

func TestCrashFrameOffsetAllocationBudget(t *testing.T) {
	for _, text := range []string{
		"module symbol + 12", "module symbol+12", "module\t+\t12", "module +0", "module ++12", "module +1 +2",
		"module + 12 34", "module +foo12", "module + ١٢", "module\v+12", "+" + strings.Repeat("9", 1000),
	} {
		t.Run(text[:min(len(text), 40)], func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = normalizeCrashFrame(text) })
			if want := previousNormalizeCrashFrameOffset(text); got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations != 0 {
				t.Fatalf("分配 %.0f 次，预期不分配", allocations)
			}
		})
	}
}

func FuzzCrashFrameOffsetCompatibility(f *testing.F) {
	for _, text := range []string{"", "module +12", "module +1 +2", "module +12 34", "module\v+12", "module +\xff12", "+" + strings.Repeat("9", 1000)} {
		f.Add(text)
	}
	f.Fuzz(checkCrashFrameOffsetCompatibility)
}

var crashFrameOffsetResult string

func BenchmarkCrashFrameOffsetNormalization(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Plain", "module symbol42"},
		{"Offset", "module symbol + 12"},
		{"Native", "#1 libsystem_kernel.dylib 0x10 __pthread_kill + 8"},
		{"LongDigits", "module +" + strings.Repeat("9", 1000)},
		{"LongSymbol", strings.Repeat("module", 1000) + " +1"},
		{"Incomplete", "module + 12 34"},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{
			{"Previous", previousNormalizeCrashFrameOffset}, {"Current", normalizeCrashFrame},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashFrameOffsetResult = impl.normalize(tc.text)
				}
			})
		}
	}
}

func BenchmarkCrashFrameOffsetSignature(b *testing.B) {
	var java, native strings.Builder
	for i := range 64 {
		fmt.Fprintf(&java, "at java.util.Part%d.get(Part.java:%d)\n", i, i+10)
		fmt.Fprintf(&native, "%d libsystem_kernel.dylib 0x%x __pthread_kill + %d\n", i, i+4096, i+8)
	}
	for _, tc := range []struct{ name, platform, message, stack string }{
		{"Plain", "ios", "synthetic crash", "input_runtime::page::select"},
		{"Java64", "android", "synthetic crash 123", java.String() + "at msime.Main.run(Main.java:123)"},
		{"Native64", "macos", "EXC_BAD_ACCESS at 0x10", native.String() + "65 MSIME 0x1000 Keyboard.layout() + 1"},
	} {
		for _, impl := range []struct {
			name      string
			signature func(string, string, string) string
		}{
			{"Previous", previousCrashFrameOffsetSignature}, {"Current", crashSignature},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashFrameOffsetResult = impl.signature(tc.platform, tc.message, tc.stack)
				}
			})
		}
	}
}
