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

// 原正则只供兼容性对照使用，生产流程不再编译它。
var crashFrameIndex = regexp.MustCompile(`^(?:#?[0-9]+[:.]?\s+|at\s+)+`)

// 冻结字节扫描前的帧规范化流程，隔离本次序号前缀优化的收益。
func previousNormalizeCrashFrameIndex(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	// 这些替换都要求 ASCII 数字或正则的 ASCII 空白；纯符号帧直接转小写，避免五次整行复制。
	if !strings.ContainsAny(line, "0123456789 \t\n\f\r") {
		return strings.ToLower(line)
	}
	// 只在存在必要标记时调用正则；未匹配的 ReplaceAllString 也会复制整行。
	if line[0] >= '0' && line[0] <= '9' || line[0] == '#' || strings.HasPrefix(line, "at") {
		line = crashFrameIndex.ReplaceAllString(line, "")
	}
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

func previousCrashFrameIndexSignature(platform, message, stack string) string {
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
			candidate = previousNormalizeCrashFrameIndex(line)
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

func checkCrashFrameIndexCompatibility(t *testing.T, text string) {
	t.Helper()
	if got, want := trimCrashFrameIndex(text), crashFrameIndex.ReplaceAllString(text, ""); got != want {
		t.Fatalf("输入=%q，前缀结果=%q，正则结果=%q", text, got, want)
	}
	if got, want := normalizeCrashFrame(text), previousNormalizeCrashFrameIndex(text); got != want {
		t.Fatalf("栈帧=%q，结果=%q，原结果=%q", text, got, want)
	}
	stack := "at java.lang.part.run(part.java:42)\n" + text + "\nat msime.main.run(main.java:123)"
	if got, want := crashSignature("Android", "synthetic crash 123", stack), previousCrashFrameIndexSignature("Android", "synthetic crash 123", stack); got != want {
		t.Fatalf("栈帧=%q，签名=%q，原签名=%q", text, got, want)
	}
}

func TestCrashFrameIndexCompatibility(t *testing.T) {
	for _, text := range []string{
		"", " ", "at", "at ", "at\tmodule", "at\fmodule", "at\rmodule", "at\nmodule", "at\vmodule", "at\u00a0module", "at\u3000module", "AT module", "atom42",
		"#", "# module", "#at module", "#0 module", "0 module", "0: module", "0. module", "0:: module", "0.. module", "#0:. module", "#0.: module", "０ module", "١ module",
		"at at #12. 34:\tmodule symbol42", "12 #34", "12 #34 ", "12 #34x", "12 #34:module", "12 at", "12 at at", "12 at at ", "12 #34.. symbol42",
		"1\vmodule", "1\fmodule", "1\u00a0module", "1\u0085module", "#1:\t\r\nmodule", "\xffat module", "at\xff module", "12 \xff34 module", "1\x002 module", "at #2 \xfe",
		"#12 0x10 module!symbol(source:42) + 8", "at java.lang.part42.run(part.java:123)", "  #12\tmodule:3 + 4  ", strings.Repeat("9", 10000), strings.Repeat("1 ", 1000) + "at #2",
	} {
		checkCrashFrameIndexCompatibility(t, text)
	}
	random := rand.New(rand.NewSource(1486))
	atoms := []string{"#", "a", "t", "at", "0", "12", ":", ".", " ", "\t", "\n", "\r", "\f", "\v", "\u00a0", "\u3000", "水", "\xff", "\xfe", "\x00", "module42"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(60) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkCrashFrameIndexCompatibility(t, text.String())
	}
}

func TestCrashFrameIndexBytePairs(t *testing.T) {
	check := func(text string) {
		t.Helper()
		if got, want := trimCrashFrameIndex(text), crashFrameIndex.ReplaceAllString(text, ""); got != want {
			t.Fatalf("输入=%q，前缀结果=%q，正则结果=%q", text, got, want)
		}
	}
	// 覆盖所有前缀字节对，并在已完成前缀后重复检查，包含非法 UTF-8 和控制字符。
	for first := range 256 {
		for second := range 256 {
			pair := string([]byte{byte(first), byte(second)})
			check(pair + " symbol42")
			check("at #12. 34:\t" + pair + " symbol42")
		}
	}
	for c := range 256 {
		b := string([]byte{byte(c)})
		for _, text := range []string{"at" + b + "module", "#" + b + " module", "12" + b + " module", "12:" + b + "module", "1 " + b + "2 module"} {
			check(text)
		}
	}
}

func TestCrashFrameIndexAllocationBudget(t *testing.T) {
	for _, text := range []string{
		"at module symbol42", "#12 module symbol42", "12:\tmodule symbol42", "12. module symbol42",
		"at #12. 34:\tmodule symbol42", "at #12.", "#", "#12module", "atom42", strings.Repeat("9", 1000),
	} {
		t.Run(text[:min(len(text), 40)], func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = normalizeCrashFrame(text) })
			if want := previousNormalizeCrashFrameIndex(text); got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations != 0 {
				t.Fatalf("分配 %.0f 次，预期不分配", allocations)
			}
		})
	}
}

func FuzzCrashFrameIndexCompatibility(f *testing.F) {
	for _, text := range []string{"", "at module42", "#12.\tmodule", "at #12.", "1\vmodule", "1 \xff2 module", strings.Repeat("1 ", 300) + "#2"} {
		f.Add(text)
	}
	f.Fuzz(checkCrashFrameIndexCompatibility)
}

var crashFrameIndexResult string

func BenchmarkCrashFrameIndexNormalization(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Plain", "module symbol42"},
		{"Java", "at java.util.Part1.get(Part.java:123)"},
		{"Native", "#1 libsystem_kernel.dylib 0x10 __pthread_kill + 8"},
		{"LongDigits", strings.Repeat("1", 1000)},
		{"Repeated", strings.Repeat("at #12. ", 100) + "module symbol42"},
		{"Incomplete", "at #12."},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{
			{"Previous", previousNormalizeCrashFrameIndex}, {"Current", normalizeCrashFrame},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashFrameIndexResult = impl.normalize(tc.text)
				}
			})
		}
	}
}

func BenchmarkCrashFrameIndexSignature(b *testing.B) {
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
			{"Previous", previousCrashFrameIndexSignature}, {"Current", crashSignature},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashFrameIndexResult = impl.signature(tc.platform, tc.message, tc.stack)
				}
			})
		}
	}
}
