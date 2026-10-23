package account

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"
)

// 固定优化前的消息规范化和签名流程，防止改变已经保存的崩溃分组。
func previousNormalizeCrashMessageCopies(message string) string {
	line := crashFirstLine(message)
	line = crashAddressPattern.ReplaceAllString(line, "0x?")
	line = crashUUIDPattern.ReplaceAllString(line, "?")
	line = crashHexRunPattern.ReplaceAllString(line, "?")
	line = crashDigitsPattern.ReplaceAllString(line, "#")
	return strings.ToLower(strings.TrimSpace(crashSpaces.ReplaceAllString(line, " ")))
}

func previousCrashSignatureCopies(platform, message, stack string) string {
	normalized := previousNormalizeCrashMessageCopies(message)
	frame := ""
	lineCount := 0
	for line := range strings.SplitSeq(stack, "\n") {
		lineCount++
		if candidate := previousNormalizeCrashEmptyFrame(line); candidate != "" && !crashSystemFrame(candidate) {
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

func TestCrashNormalizationMatchesPrevious(t *testing.T) {
	inputs := []string{
		"", " \t\r\n", "input_runtime::page::select", "EXC_BAD_ACCESS at 0x10",
		"at\vwidget", "at\u00a0widget", "std::part٢", "deadBEEF", "水\v杉",
		"\t2 MSIME 0x01 Keyboard.layout() + 1\r", "#2: at at msime.Main.run(Main.java:123:45)",
		"at java.util.ArrayList.get(ArrayList.java:437)", "at\vat\fwidget:12 + 34", "at\twidget:12\t+\t34\f",
		"0XABC0 0xdeadBEEF 0x 0xg", "deadbeef abcd1234 deadbeefg _deadbeef_ 水deadbeef杉",
		"00000000-1111-2222-3333-444444444444 12345678", "a  b\t\tc\f\r d",
		"水\u3000杉\u00a0版本123", " \ufffd\xff\xfe0xFF:34 + 56 ", "\n\r\t\n First nonblank 123\nnext 456",
	}
	random := rand.New(rand.NewSource(687))
	atoms := []string{"a", "A", "at", "0x", "0X", "0", "1", "12345678", "deadbeef", "#", ":", ".", "+", "-", "_", " ", "\t", "\r", "\n", "\v", "\f", "水", "😀", "\u3000", "\u00a0", "\xff", "\xfe", "\x00"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		inputs = append(inputs, text.String())
	}
	for _, text := range inputs {
		if got, want := normalizeCrashFrame(text), previousNormalizeCrashEmptyFrame(text); got != want {
			t.Fatalf("栈帧=%q，结果=%q，原结果=%q", text, got, want)
		}
		if got, want := normalizeCrashMessage(text), previousNormalizeCrashMessageCopies(text); got != want {
			t.Fatalf("消息=%q，结果=%q，原结果=%q", text, got, want)
		}
		stack := "std::panicking::begin_panic\n" + text + "\ninput_runtime::page::select"
		if got, want := crashSignature("iPadOS", text, stack), previousCrashSignatureCopies("iPadOS", text, stack); got != want {
			t.Fatalf("消息=%q，签名=%q，原签名=%q", text, got, want)
		}
	}
}

func TestCrashNormalizationAvoidsUnchangedCopies(t *testing.T) {
	stack := strings.Repeat("std::panicking::begin_panic\n", 32) + "input_runtime::page::select"
	const message = "synthetic crash"
	want := previousCrashSignatureCopies("ios", message, stack)
	measure := func(signature func(string, string, string) string) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				if got := signature("ios", message, stack); got != want {
					t.Fatalf("签名=%q，原签名=%q", got, want)
				}
			}
		})
	}
	previous, current := measure(previousCrashSignatureCopies), measure(crashSignature)
	t.Logf("原分配=%s，新分配=%s", previous.MemString(), current.MemString())
	// 对比完整签名流程，给 race 正则对象池的固定开销留出余量。
	if saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < 8<<10 {
		t.Fatalf("32 条无需替换的系统栈帧只节省 %d 字节，期望至少 8 KiB", saved)
	}
}

func TestCrashSignatureReusesConsecutiveFrames(t *testing.T) {
	stack := strings.Repeat("at java.util.ArrayList.get(ArrayList.java:437)\n", 64) + "at msime.Main.run(Main.java:123)"
	want := previousCrashSignatureCopies("android", "synthetic crash", stack)
	var got string
	allocations := testing.AllocsPerRun(20, func() { got = crashSignature("android", "synthetic crash", stack) })
	if got != want {
		t.Fatalf("重复栈帧的签名变化：结果=%q，预期=%q", got, want)
	}
	if allocations > 80 {
		t.Fatalf("重复栈帧仍分配 %.0f 次，超过 80 次预算", allocations)
	}
}

var crashNormalizationResult string

func BenchmarkCrashNormalizationCopies(b *testing.B) {
	for _, tc := range []struct{ name, message, stack string }{
		{"Rust", "synthetic crash", strings.Repeat("std::panicking::begin_panic\n", 32) + "input_runtime::page::select"},
		{"Java", "java.lang.IllegalStateException: operation failed", strings.Repeat("at java.util.ArrayList.get(ArrayList.java:437)\n", 32) + "at msime.Main.run(Main.java:123)"},
		{"Native", "EXC_BAD_ACCESS at 0x0000000000000010", strings.Repeat("2 libsystem_kernel.dylib 0xABCDEF __pthread_kill + 8\n", 32) + "3 MSIME 0x1000 Keyboard.layout() + 1"},
		{"Identifiers", "Crash 01234567-89ab-cdef-0123-456789abcdef abcd1234 at 0xABCDEF + 123", "#0: at MSIME.Main.run(Main.java:123:45) + 678"},
		{"Unicode", "合成崩溃消息", "水杉\u3000页面选择"},
	} {
		for _, impl := range []struct {
			name      string
			signature func(string, string, string) string
		}{
			{"Previous", previousCrashSignatureCopies}, {"Current", crashSignature},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashNormalizationResult = impl.signature("ios", tc.message, tc.stack)
				}
			})
		}
	}
}
