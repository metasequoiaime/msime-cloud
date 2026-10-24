package account

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// 原数字正则仅用于兼容性对照，生产流程不再编译它。
var crashDigitsPattern = regexp.MustCompile(`[0-9]+`)

// 冻结数字扫描前的消息和签名流程，验证已有分组保持一致。
func previousNormalizeCrashDigitsMessage(message string) string {
	line := crashFirstLine(message)
	if !crashMessageMayChange(line) {
		return strings.ToLower(line)
	}
	if strings.Contains(line, "0x") || strings.Contains(line, "0X") {
		line = replaceCrashAddresses(line, "0x?")
	}
	if len(line) >= 36 && strings.ContainsRune(line, '-') {
		line = crashUUIDPattern.ReplaceAllString(line, "?")
	}
	if crashHasHexRun(line) {
		line = crashHexRunPattern.ReplaceAllString(line, "?")
	}
	if strings.ContainsAny(line, "0123456789") {
		line = crashDigitsPattern.ReplaceAllString(line, "#")
	}
	return normalizeCrashWhitespace(line)
}

func previousCrashDigitsSignature(platform, message, stack string) string {
	normalized := previousNormalizeCrashDigitsMessage(message)
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
			candidate = normalizeCrashFrame(line)
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

func TestCrashDigitsMessageAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		message string
		maximum float64
	}{
		{"synthetic crash 123", 1}, {"1", 0}, {"1 2 3", 1}, {"water1tree2", 1},
		{"synthetic crash 1-2-3", 1}, {"合成崩溃123", 1}, {"CRASH 123", 2},
		{"access at 0x123", 2}, {"access at 0X123", 2}, {"1\t2", 2}, {"no digits", 0},
	} {
		t.Run(tc.message, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = normalizeCrashMessage(tc.message) })
			if want := previousNormalizeCrashDigitsMessage(tc.message); got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations > tc.maximum {
				t.Fatalf("分配 %.0f 次，预算最多 %.0f 次", allocations, tc.maximum)
			}
		})
	}
}

func checkCrashDigitsCompatibility(t *testing.T, text string) {
	t.Helper()
	if got, want := maskCrashDigits(text), crashDigitsPattern.ReplaceAllString(text, "#"); got != want {
		t.Fatalf("输入=%q，掩码=%q，正则结果=%q", text, got, want)
	}
	if got, want := normalizeCrashMessage(text), previousNormalizeCrashDigitsMessage(text); got != want {
		t.Fatalf("输入=%q，消息结果=%q，原结果=%q", text, got, want)
	}
	const stack = "0 libsystem_kernel.dylib 0x100 __pthread_kill + 8\n1 msime 0x200 keyboard.layout() + 12"
	if got, want := crashSignature("MacOS", text, stack), previousCrashDigitsSignature("MacOS", text, stack); got != want {
		t.Fatalf("输入=%q，签名=%q，原签名=%q", text, got, want)
	}
}

func TestCrashDigitsCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "0", "1234567", "0 1", "1-2-3", "prefix1suffix", "#1#2#", "00123", "0x1 123", "0Xab 123",
		"1\x002", "1\xff2", "\xff123\xfe", "１２３", "١٢٣", "水1杉2", "1\t2", "1\v2", "1\u30002", "1\n2",
		" first 12\nsecond 34 ", "Crash 01234567-89ab-cdef-0123-456789abcdef deadbeef 123", "deadbeef1g",
		"module" + strings.Repeat("1", 10000) + "symbol", strings.Repeat("digit1 ", 1000),
	} {
		checkCrashDigitsCompatibility(t, text)
	}
	random := rand.New(rand.NewSource(1501))
	atoms := []string{"0", "1", "23456789", "0x", "0X", "abcdefab", "ABCDEF", "g", "#", "+", "-", "_", " ", "\t", "\n", "\r", "\f", "\v", "水", "１２３", "١٢٣", "\u00a0", "\u3000", "\xff", "\xfe", "\x00"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(60) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkCrashDigitsCompatibility(t, text.String())
	}
}

func TestCrashDigitsBytePairs(t *testing.T) {
	check := func(text string) {
		t.Helper()
		if got, want := maskCrashDigits(text), crashDigitsPattern.ReplaceAllString(text, "#"); got != want {
			t.Fatalf("输入=%q，掩码=%q，正则结果=%q", text, got, want)
		}
	}
	// 穷举数字段内部与结尾的全部字节对，覆盖合并、分隔与非法 UTF-8。
	for first := range 256 {
		for second := range 256 {
			pair := string([]byte{byte(first), byte(second)})
			check("module" + pair + "123symbol")
			check("module1" + pair + "2")
		}
	}
	for c := range 256 {
		one := string([]byte{byte(c)})
		for _, text := range []string{one + "1", "1" + one, one, "1" + one + "2"} {
			check(text)
		}
	}
}

func TestCrashDigitsReplacementAllocationBudget(t *testing.T) {
	for _, text := range []string{"", "plain", "１２٣", "#", "\xff", "1", "123456789", "module123symbol", "1-2-3", "水123杉", strings.Repeat("1", 10000)} {
		want := crashDigitsPattern.ReplaceAllString(text, "#")
		maximum := float64(1)
		if text == want || want == "#" {
			maximum = 0
		}
		var got string
		allocations := testing.AllocsPerRun(100, func() { got = maskCrashDigits(text) })
		if got != want {
			t.Fatalf("输入=%q，掩码=%q，正则结果=%q", text, got, want)
		}
		if allocations > maximum {
			t.Fatalf("输入长度=%d，分配 %.0f 次，预算最多 %.0f 次", len(text), allocations, maximum)
		}
	}
}

func TestCrashDigitsOutputSizeBudget(t *testing.T) {
	text := "module" + strings.Repeat("1", 10000) + "symbol"
	measured := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			crashDigitsResult = maskCrashDigits(text)
		}
	})
	if want := "module#symbol"; crashDigitsResult != want {
		t.Fatalf("结果=%q，预期=%q", crashDigitsResult, want)
	}
	if bytes := measured.AllocedBytesPerOp(); bytes > 64 {
		t.Fatalf("短输出分配 %d 字节，预算最多 64 字节", bytes)
	}
}

func FuzzCrashDigitsCompatibility(f *testing.F) {
	for _, text := range []string{"", "123", "0x123 45", "1\xff2", "１２３", "1\u30002", "module" + strings.Repeat("1", 1000) + "symbol"} {
		f.Add(text)
	}
	f.Fuzz(checkCrashDigitsCompatibility)
}

var crashDigitsResult string

func BenchmarkCrashDigitsMessage(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Plain", "synthetic crash"}, {"Single", "synthetic crash 123"}, {"Many", strings.Repeat("error1 ", 32)},
		{"Address", "access at 0x123"}, {"LongRun", "module" + strings.Repeat("1", 10000) + "symbol"}, {"Unicode", strings.Repeat("合成", 100) + "123"},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{{"Previous", previousNormalizeCrashDigitsMessage}, {"Current", normalizeCrashMessage}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashDigitsResult = impl.normalize(tc.text)
				}
			})
		}
	}
}

func BenchmarkCrashDigitsSignature(b *testing.B) {
	for _, tc := range []struct{ name, message string }{
		{"Plain", "synthetic crash"}, {"Digits", "synthetic crash 123"}, {"Address", "access at 0x123"},
	} {
		for _, impl := range []struct {
			name      string
			signature func(string, string, string) string
		}{{"Previous", previousCrashDigitsSignature}, {"Current", crashSignature}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashDigitsResult = impl.signature("ios", tc.message, "input_runtime::page::select")
				}
			})
		}
	}
}
