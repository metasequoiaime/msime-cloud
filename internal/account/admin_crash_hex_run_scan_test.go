package account

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// 原正则与保守预筛选仅用于冻结参考，生产流程直接扫描完整匹配。
var crashHexRunPattern = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)

// 冻结十六进制串扫描前的消息与签名流程，验证已有分组保持一致。
func previousNormalizeCrashHexRunMessage(message string) string {
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
	line = maskCrashDigits(line)
	return normalizeCrashWhitespace(line)
}

func previousCrashHexRunSignature(platform, message, stack string) string {
	normalized := previousNormalizeCrashHexRunMessage(message)
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

func TestCrashHexRunMessageAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		message string
		maximum float64
	}{
		{"deadbeef", 0}, {"deadbee", 0}, {"gdeadbeef", 0}, {"deadbeefg", 0}, {"_deadbeef_", 0},
		{"g12345678", 1}, {"deadbeef deadbeef", 1}, {"水deadbeef杉", 1}, {"0x123 deadbeef", 3},
		{"deadbeef\t12", 3}, {"DEADBEEF", 0}, {"DEADBEEFG", 1}, {"plain message", 0},
		{"module" + strings.Repeat("1", 10000) + "symbol", 1}, {"module" + strings.Repeat("f", 10000) + "symbol", 0},
	} {
		t.Run(tc.message[:min(len(tc.message), 50)], func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = normalizeCrashMessage(tc.message) })
			if want := previousNormalizeCrashHexRunMessage(tc.message); got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations > tc.maximum {
				t.Fatalf("分配 %.0f 次，预算最多 %.0f 次", allocations, tc.maximum)
			}
		})
	}
}

func checkCrashHexRunCompatibility(t *testing.T, text string) {
	t.Helper()
	if got, want := maskCrashHexRuns(text), crashHexRunPattern.ReplaceAllString(text, "?"); got != want {
		t.Fatalf("输入=%q，掩码=%q，正则结果=%q", text, got, want)
	}
	if got, want := normalizeCrashMessage(text), previousNormalizeCrashHexRunMessage(text); got != want {
		t.Fatalf("输入=%q，消息结果=%q，原结果=%q", text, got, want)
	}
	const stack = "0 libsystem_kernel.dylib 0x100 __pthread_kill + 8\n1 msime 0x200 keyboard.layout() + 12"
	if got, want := crashSignature("MacOS", text, stack), previousCrashHexRunSignature("MacOS", text, stack); got != want {
		t.Fatalf("输入=%q，签名=%q，原签名=%q", text, got, want)
	}
}

func TestCrashHexRunCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "deadbee", "deadbeef", "DEADBEEF", "01234567", "deadbeef00", "fdeadbeef", "deadbeeff",
		"gdeadbeef", "deadbeefg", "_deadbeef_", "deadbeef_ffffffff", "deadbeef?ffffffff", "deadbeef ffffffff",
		"水deadbeef杉", "édeadbeefé", "Kdeadbeef", "deadbeefK", "ſdeadbeef", "deadbeef\u0301", "\xffdeadbeef\xfe",
		"deadbeef\x00ffffffff", "deadbeef\nffffffff", "0xdeadbeef 123", "Crash 01234567-89ab-cdef-0123-456789abcdef deadbeef 123",
		"module" + strings.Repeat("1", 10000) + "symbol", "module" + strings.Repeat("f", 10000) + "symbol",
		"hash " + strings.Repeat("f", 10000) + " value", strings.Repeat("deadbeef ", 1000),
	} {
		checkCrashHexRunCompatibility(t, text)
	}
	for length := range 13 {
		for _, text := range []string{strings.Repeat("a", length), "g" + strings.Repeat("a", length), strings.Repeat("a", length) + "_", "水" + strings.Repeat("a", length) + "杉"} {
			checkCrashHexRunCompatibility(t, text)
		}
	}
	random := rand.New(rand.NewSource(1504))
	atoms := []string{"deadbeef", "deadbee", "abcdefab", "01234567", "0x", "0X", "123", "g", "G", "_", "#", "?", "+", "-", " ", "\t", "\n", "\r", "\f", "\v", "水", "é", "K", "１２３", "\u00a0", "\u3000", "\xff", "\xfe", "\x00"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(60) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkCrashHexRunCompatibility(t, text.String())
	}
}

func TestCrashHexRunBytePairs(t *testing.T) {
	check := func(text string) {
		t.Helper()
		if got, want := maskCrashHexRuns(text), crashHexRunPattern.ReplaceAllString(text, "?"); got != want {
			t.Fatalf("输入=%q，掩码=%q，正则结果=%q", text, got, want)
		}
	}
	// 穷举首尾边界、数字段内部及七位临界长度的全部字节对。
	for first := range 256 {
		for second := range 256 {
			left, right := string([]byte{byte(first)}), string([]byte{byte(second)})
			pair := left + right
			check(left + "aBcD1234" + right)
			check("1234567" + pair + "abcdef01")
			check(left + "abcdef0" + right)
			check("aBcD1234" + pair + "abcd1234")
		}
	}
	for c := range 256 {
		one := string([]byte{byte(c)})
		for _, text := range []string{one + "deadbeef", "deadbeef" + one, one + "deadbee", "deadbee" + one} {
			check(text)
		}
	}
}

func TestCrashHexRunSpans(t *testing.T) {
	for _, text := range []string{"", "deadbeef", "_deadbeef_", "deadbeef ffffffff", "水deadbeef杉ffffffff", "\xffdeadbeef\xfe12345678", "gdeadbeef 12345678"} {
		matches := crashHexRunPattern.FindAllStringIndex(text, -1)
		for from := 0; from <= len(text); from++ {
			wantStart, wantEnd := -1, 0
			for _, match := range matches {
				if match[0] >= from {
					wantStart, wantEnd = match[0], match[1]
					break
				}
			}
			if start, end := nextCrashHexRun(text, from); start != wantStart || end != wantEnd {
				t.Fatalf("输入=%q，起点=%d，范围=(%d,%d)，正则范围=(%d,%d)", text, from, start, end, wantStart, wantEnd)
			}
		}
	}
}

func TestCrashHexRunReplacementAllocationBudget(t *testing.T) {
	for _, text := range []string{"", "plain", "deadbee", "deadbeef", "_deadbeef_", "deadbeefg", "水deadbeef杉", "deadbeef ffffffff", "\xffdeadbeef\xfe", strings.Repeat("f", 10000), strings.Repeat("module", 1000) + "deadbeef"} {
		want := crashHexRunPattern.ReplaceAllString(text, "?")
		maximum := float64(1)
		if text == want || want == "?" {
			maximum = 0
		}
		var got string
		allocations := testing.AllocsPerRun(100, func() { got = maskCrashHexRuns(text) })
		if got != want {
			t.Fatalf("输入=%q，掩码=%q，正则结果=%q", text, got, want)
		}
		if allocations > maximum {
			t.Fatalf("输入长度=%d，分配 %.0f 次，预算最多 %.0f 次", len(text), allocations, maximum)
		}
	}
}

func TestCrashHexRunOutputSizeBudget(t *testing.T) {
	for _, text := range []string{"hash " + strings.Repeat("f", 10000) + " value", strings.Repeat("f", 10000)} {
		measured := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				crashHexRunResult = maskCrashHexRuns(text)
			}
		})
		if want := crashHexRunPattern.ReplaceAllString(text, "?"); crashHexRunResult != want {
			t.Fatalf("结果=%q，正则结果=%q", crashHexRunResult, want)
		}
		if bytes := measured.AllocedBytesPerOp(); bytes > 64 {
			t.Fatalf("短输出分配 %d 字节，预算最多 64 字节", bytes)
		}
	}
}

func FuzzCrashHexRunCompatibility(f *testing.F) {
	for _, text := range []string{"", "deadbeef", "_deadbeef_", "Kdeadbeef", "deadbeef\xffffffffff", "deadbee", "module" + strings.Repeat("1", 1000) + "symbol"} {
		f.Add(text)
	}
	f.Fuzz(checkCrashHexRunCompatibility)
}

var crashHexRunResult string

func BenchmarkCrashHexRunMessage(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Plain", "synthetic crash"}, {"Digits", "synthetic crash 123"}, {"Bounded", "hash deadbeef value"}, {"Whole", "deadbeef"},
		{"Unbounded", "moduledeadbeefsymbol"}, {"LongDigits", "module" + strings.Repeat("1", 10000) + "symbol"},
		{"LongLetters", "module" + strings.Repeat("f", 10000) + "symbol"}, {"Many", strings.Repeat("deadbeef ", 32)},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{{"Previous", previousNormalizeCrashHexRunMessage}, {"Current", normalizeCrashMessage}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashHexRunResult = impl.normalize(tc.text)
				}
			})
		}
	}
}

func BenchmarkCrashHexRunSignature(b *testing.B) {
	for _, tc := range []struct{ name, message string }{
		{"Plain", "synthetic crash"}, {"Digits", "synthetic crash 123"}, {"Whole", "deadbeef"},
		{"LongDigits", "module" + strings.Repeat("1", 10000) + "symbol"}, {"Unbounded", "moduledeadbeefsymbol"},
	} {
		for _, impl := range []struct {
			name      string
			signature func(string, string, string) string
		}{{"Previous", previousCrashHexRunSignature}, {"Current", crashSignature}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashHexRunResult = impl.signature("ios", tc.message, "input_runtime::page::select")
				}
			})
		}
	}
}

func crashHasHexRun(line string) bool {
	run := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			run++
			if run == 8 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}
