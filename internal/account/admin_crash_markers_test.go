package account

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// 冻结本次优化前的完整流程，继续使用已经优化的空白折叠规则。
func previousCrashMarkerFrame(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	// 这些替换都要求 ASCII 数字或正则的 ASCII 空白；纯符号帧直接转小写，避免五次整行复制。
	if !strings.ContainsAny(line, "0123456789 \t\n\f\r") {
		return strings.ToLower(line)
	}
	line = crashFrameIndex.ReplaceAllString(line, "")
	line = crashAddressPattern.ReplaceAllString(line, "")
	line = crashFrameOffset.ReplaceAllString(line, "")
	line = crashFrameLineNumber.ReplaceAllString(line, "")
	return normalizeCrashWhitespace(line)
}

func previousCrashMarkerMessage(message string) string {
	line := crashFirstLine(message)
	if !crashMessageMayChange(line) {
		return strings.ToLower(line)
	}
	line = crashAddressPattern.ReplaceAllString(line, "0x?")
	line = crashUUIDPattern.ReplaceAllString(line, "?")
	line = crashHexRunPattern.ReplaceAllString(line, "?")
	line = crashDigitsPattern.ReplaceAllString(line, "#")
	return normalizeCrashWhitespace(line)
}

func previousCrashMarkerSignature(platform, message, stack string) string {
	normalized := previousCrashMarkerMessage(message)
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
			candidate = previousCrashMarkerFrame(line)
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

func checkCrashMarkerCompatibility(t *testing.T, text string) {
	t.Helper()
	if got, want := normalizeCrashFrame(text), previousCrashMarkerFrame(text); got != want {
		t.Fatalf("栈帧=%q，结果=%q，原结果=%q", text, got, want)
	}
	if got, want := normalizeCrashMessage(text), previousCrashMarkerMessage(text); got != want {
		t.Fatalf("消息=%q，结果=%q，原结果=%q", text, got, want)
	}
	stack := "at java.lang.Part.run(Part.java:42)\n" + text + "\nat msime.Main.run(Main.java:123)"
	if got, want := crashSignature("Android", text, stack), previousCrashMarkerSignature("Android", text, stack); got != want {
		t.Fatalf("输入=%q，签名=%q，原签名=%q", text, got, want)
	}
}

func TestCrashMarkerCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "\t\r\n", "module symbol42", "synthetic crash 123", "synthetic\t crash", "水杉 symbol42",
		"#1: at 0x1a module42.fn(source:123:45) + 67", "#1. at at \t0Xf module +\t12\f",
		"0x", "0X", "0xg", "0XＧ", "0xabcdef", "prefix0x1suffix", "\xff0X1\xfe",
		"at\vmodule", "at\u00a0module", "at\fmodule", "#1:\vmodule", "#1:\fmodule", "atom1",
		"0 module", "module:123", "module:１２３", "module + 4", "module\v+\v4", "module\f+\f4", "module + ١٢",
		"00000000-1111-2222-3333-444444444444", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"prefixaaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeeesuffix", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeee",
		"1234567", "12345678", "123456789", "abcdefg0", "gdeadbeefg", "_deadbeef_", "水deadbeef杉",
		"deadbeef0x1 deadBEEF", "0x1:23 + 45", "a\x00b\xff:12", strings.Repeat("a", 999) + "1",
	} {
		checkCrashMarkerCompatibility(t, text)
	}
	// 穷举前缀字节对，覆盖标记附近的非法 UTF-8 和控制字符。
	for first := range 256 {
		for second := range 256 {
			text := string([]byte{byte(first), byte(second)}) + "0x1:23 + 4"
			checkCrashMarkerCompatibility(t, text)
		}
	}
	random := rand.New(rand.NewSource(1481))
	atoms := []string{"a", "A", "at", "0x", "0X", "0", "12345678", "abcdefab", "-", "+", ":", "#", ".", "_", " ", "\t", "\n", "\r", "\f", "\v", "水", "\u00a0", "\u3000", "\xff", "\xfe", "\x00"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkCrashMarkerCompatibility(t, text.String())
	}
}

func TestCrashMarkerAllocationBudget(t *testing.T) {
	// 正则在竞态检测下会因 sync.Pool 随机丢弃缓存而增加分配，为仍需替换的路径留出余量。
	for _, tc := range []struct {
		name, text          string
		normalize, previous func(string) string
		maximum             float64
	}{
		{"帧中只有空格", "module symbol", normalizeCrashFrame, previousCrashMarkerFrame, 0},
		{"帧中嵌入数字", "module symbol42", normalizeCrashFrame, previousCrashMarkerFrame, 0},
		{"Unicode帧", "水杉 select42", normalizeCrashFrame, previousCrashMarkerFrame, 0},
		{"数字消息", "synthetic crash 123", normalizeCrashMessage, previousCrashMarkerMessage, 8},
		{"控制空白消息", "synthetic\t crash", normalizeCrashMessage, previousCrashMarkerMessage, 1},
		{"短连字符消息", "synthetic-4-crash", normalizeCrashMessage, previousCrashMarkerMessage, 8},
		{"字母UUID消息", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", normalizeCrashMessage, previousCrashMarkerMessage, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = tc.normalize(tc.text) })
			want := tc.previous(tc.text)
			if got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations > tc.maximum {
				t.Fatalf("分配 %.0f 次，超过 %.0f 次预算", allocations, tc.maximum)
			}
		})
	}
}

func FuzzCrashMarkerCompatibility(f *testing.F) {
	for _, text := range []string{"", "at\tmodule:42 + 4", "0x1", "水deadbeef杉", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "\xff\v0Xf:12"} {
		f.Add(text)
	}
	f.Fuzz(checkCrashMarkerCompatibility)
}

var crashMarkerResult string

func BenchmarkCrashMarkerNormalization(b *testing.B) {
	for _, tc := range []struct {
		name, text        string
		previous, current func(string) string
	}{
		{"FramePlain", "module symbol42", previousCrashMarkerFrame, normalizeCrashFrame},
		{"FrameAllMarkers", "#1: at 0x1a module42.fn(source:123:45) + 67", previousCrashMarkerFrame, normalizeCrashFrame},
		{"FrameLongDigits", strings.Repeat("1", 1000), previousCrashMarkerFrame, normalizeCrashFrame},
		{"MessageDigits", "synthetic crash 123", previousCrashMarkerMessage, normalizeCrashMessage},
		{"MessageSpaces", "synthetic\t crash", previousCrashMarkerMessage, normalizeCrashMessage},
		{"MessageIdentifiers", "Crash 01234567-89ab-cdef-0123-456789abcdef deadbeef 0x123", previousCrashMarkerMessage, normalizeCrashMessage},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{{"Previous", tc.previous}, {"Current", tc.current}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashMarkerResult = impl.normalize(tc.text)
				}
			})
		}
	}
}

func BenchmarkCrashMarkerSignature(b *testing.B) {
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
		}{{"Previous", previousCrashMarkerSignature}, {"Current", crashSignature}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashMarkerResult = impl.signature(tc.platform, tc.message, tc.stack)
				}
			})
		}
	}
}
