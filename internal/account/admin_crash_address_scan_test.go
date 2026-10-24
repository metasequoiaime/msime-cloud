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

// 原地址正则仅用于兼容性对照，生产流程不再编译它。
var crashAddressPattern = regexp.MustCompile(`0[xX][0-9a-fA-F]+`)

// 冻结地址扫描前的帧、消息和签名流程，验证已有分组保持一致。
func previousNormalizeCrashAddressFrame(line string) string {
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
		line = stripCrashFrameLineNumbers(line)
	}
	return normalizeCrashWhitespace(line)
}

func previousNormalizeCrashAddressMessage(message string) string {
	line := crashFirstLine(message)
	if !crashMessageMayChange(line) {
		return strings.ToLower(line)
	}
	if strings.Contains(line, "0x") || strings.Contains(line, "0X") {
		line = crashAddressPattern.ReplaceAllString(line, "0x?")
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

func previousCrashAddressSignature(platform, message, stack string) string {
	normalized := previousNormalizeCrashAddressMessage(message)
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
			candidate = previousNormalizeCrashAddressFrame(line)
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

func checkCrashAddressCompatibility(t *testing.T, text string) {
	t.Helper()
	checkCrashAddressReplacement(t, text)
	if got, want := normalizeCrashFrame(text), previousNormalizeCrashAddressFrame(text); got != want {
		t.Fatalf("输入=%q，帧结果=%q，原结果=%q", text, got, want)
	}
	if got, want := normalizeCrashMessage(text), previousNormalizeCrashAddressMessage(text); got != want {
		t.Fatalf("输入=%q，消息结果=%q，原结果=%q", text, got, want)
	}
	stack := "0 libsystem_kernel.dylib 0x10 __pthread_kill + 8\n" + text + "\n2 msime 0x20 keyboard.layout() + 12"
	if got, want := crashSignature("MacOS", text, stack), previousCrashAddressSignature("MacOS", text, stack); got != want {
		t.Fatalf("输入=%q，签名=%q，原签名=%q", text, got, want)
	}
}

func TestCrashAddressCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "0", "x", "0x", "0X", "0xg", "0XＧ", "0x0", "0XaBcDeF", "0x1234567890abcdefABCDEF", "0x١٢", "0x１２",
		"0x1 0X2", "0x1\n0X2", "0x0x1", "00x1x2", "0x1g0X2", "0x1_0X2", "0x1:23 + 45", "0x+1", "0x-1",
		"prefix0x1suffix", "a0x1b", "module0xg42", "0\x00x1", "0x\xff1", "0x1\xff", "\xff0X1\xfe", "0x\t12", "水0x1杉",
		"#1: at 0x123 module.fn(file:12) + 34", "\n\tfirst 0Xab\nsecond 0x12", "module0x" + strings.Repeat("f", 10000) + "symbol",
		strings.Repeat("module", 1000) + "0xg", "module0x" + strings.Repeat("0", 1000) + "0x1",
	} {
		checkCrashAddressCompatibility(t, text)
	}
	random := rand.New(rand.NewSource(1497))
	atoms := []string{"0x", "0X", "0", "x", "X", "1", "12345678", "abcdefab", "ABCDEF", "g", ":", "#", "+", "-", "at", " ", "\t", "\n", "\r", "\f", "\v", "水", "\u00a0", "\u3000", "\xff", "\xfe", "\x00"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(60) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkCrashAddressCompatibility(t, text.String())
	}
}

func checkCrashAddressReplacement(t *testing.T, text string) {
	t.Helper()
	for _, replacement := range []string{"", "0x?"} {
		if got, want := replaceCrashAddresses(text, replacement), crashAddressPattern.ReplaceAllString(text, replacement); got != want {
			t.Fatalf("输入=%q，替换=%q，结果=%q，正则结果=%q", text, replacement, got, want)
		}
	}
}

func TestCrashAddressBytePairs(t *testing.T) {
	// 穷举地址内部及结尾的全部字节对，同时对照帧删除和消息掩码两种替换。
	for first := range 256 {
		for second := range 256 {
			pair := string([]byte{byte(first), byte(second)})
			checkCrashAddressReplacement(t, "module0x"+pair+"f!")
			checkCrashAddressReplacement(t, "module0X1"+pair+"0x2")
		}
	}
	for c := range 256 {
		one := string([]byte{byte(c)})
		for _, text := range []string{one + "0x1", "0x1" + one, "0" + one + "1", "0x" + one, "0X" + one + "0x1"} {
			checkCrashAddressReplacement(t, text)
		}
	}
}

func TestCrashAddressReplacementAllocationBudget(t *testing.T) {
	for _, replacement := range []string{"", "0x?"} {
		for _, text := range []string{"", "module", "0x", "0xg", "0XＧ", "0x١٢", "0x\xff1", "module0x1symbol", "00x1x2", "0x1"} {
			maximum := float64(0)
			want := crashAddressPattern.ReplaceAllString(text, replacement)
			if text != want && want != "" {
				maximum = 1
			}
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = replaceCrashAddresses(text, replacement) })
			if got != want {
				t.Fatalf("输入=%q，替换=%q，结果=%q，正则结果=%q", text, replacement, got, want)
			}
			if allocations > maximum {
				t.Fatalf("输入=%q，替换=%q，分配 %.0f 次，预算最多 %.0f 次", text, replacement, allocations, maximum)
			}
		}
	}
}

func TestCrashAddressOutputSizeBudget(t *testing.T) {
	text := "module0x" + strings.Repeat("f", 10000) + "symbol"
	for _, replacement := range []string{"", "0x?"} {
		measured := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				crashAddressResult = replaceCrashAddresses(text, replacement)
			}
		})
		if want := "module" + replacement + "symbol"; crashAddressResult != want {
			t.Fatalf("结果=%q，预期=%q", crashAddressResult, want)
		}
		if bytes := measured.AllocedBytesPerOp(); bytes > 64 {
			t.Fatalf("短输出分配 %d 字节，预算最多 64 字节", bytes)
		}
	}
}

func TestCrashAddressFrameAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		text    string
		maximum float64
	}{
		{"module 0xg", 0}, {"module 0x", 0}, {"module0xg42", 0}, {"module 0Xg", 1},
		{"module0x1symbol", 1}, {"module 0x1 symbol", 2}, {"module0x1 0x2symbol", 1}, {"module00x1x2", 1}, {"0x1", 0},
	} {
		t.Run(tc.text, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = normalizeCrashFrame(tc.text) })
			if want := previousNormalizeCrashAddressFrame(tc.text); got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations > tc.maximum {
				t.Fatalf("分配 %.0f 次，预算最多 %.0f 次", allocations, tc.maximum)
			}
		})
	}
}

func FuzzCrashAddressCompatibility(f *testing.F) {
	for _, text := range []string{"", "0x", "0Xab", "00x1x2", "0x0x1", "0x\xff1", "0x١٢", "module0x" + strings.Repeat("f", 1000) + "symbol"} {
		f.Add(text)
	}
	f.Fuzz(checkCrashAddressCompatibility)
}

var crashAddressResult string

func benchmarkCrashAddressText(b *testing.B, text string, previous, current func(string) string) {
	for _, impl := range []struct {
		name      string
		normalize func(string) string
	}{
		{"Previous", previous}, {"Current", current},
	} {
		b.Run(impl.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				crashAddressResult = impl.normalize(text)
			}
		})
	}
}

func BenchmarkCrashAddressFrame(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Plain", "module symbol42"}, {"Address", "module0x123symbol"}, {"Incomplete", "module0xg42"},
		{"LongHex", "module0x" + strings.Repeat("f", 1000) + "symbol"}, {"LongSymbol", strings.Repeat("module", 1000) + "0xg42"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkCrashAddressText(b, tc.text, previousNormalizeCrashAddressFrame, normalizeCrashFrame)
		})
	}
}

func BenchmarkCrashAddressMessage(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Address", "access at 0x123"}, {"Incomplete", "access at 0xg"}, {"LongHex", "access at 0x" + strings.Repeat("f", 1000)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkCrashAddressText(b, tc.text, previousNormalizeCrashAddressMessage, normalizeCrashMessage)
		})
	}
}

func BenchmarkCrashAddressSignature(b *testing.B) {
	var native strings.Builder
	for i := range 64 {
		fmt.Fprintf(&native, "%d libsystem_kernel.dylib 0x%x __pthread_kill + %d\n", i, i+4096, i+8)
	}
	for _, tc := range []struct{ name, platform, message, stack string }{
		{"Plain", "ios", "synthetic crash", "input_runtime::page::select"},
		{"Native64", "macos", "EXC_BAD_ACCESS at 0x10", native.String() + "65 msime 0x1000 keyboard.layout() + 1"},
		{"Message", "ios", "access at 0x123", "input_runtime::page::select"},
	} {
		for _, impl := range []struct {
			name      string
			signature func(string, string, string) string
		}{
			{"Previous", previousCrashAddressSignature}, {"Current", crashSignature},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashAddressResult = impl.signature(tc.platform, tc.message, tc.stack)
				}
			})
		}
	}
}
