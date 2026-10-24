package account

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// 冻结分段哈希之前的完整签名流程，验证已有崩溃分组保持一致。
func previousCrashSignatureDigest(platform, message, stack string) string {
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

var crashDigestResult string

func TestCrashSignatureDigestAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, platform, message, stack string
		maximum                        float64
	}{
		{"empty", "ios", "", "", 0},
		{"short", "ios", "a", "", 1},
		{"plain", "ios", "synthetic crash", "input_runtime::page::select", 1},
		{"maximum_message", "ios", strings.Repeat("q", telemetryMessageLimit), "", 1},
		{"long_message", "ios", "crash " + strings.Repeat("q", 10000), "", 1},
		{"long_frame", "ios", "synthetic crash", "input_runtime::" + strings.Repeat("q", 10000), 1},
		{"long_both", "ios", strings.Repeat("q", 10000), strings.Repeat("z", 10000), 1},
		{"long_platform", strings.Repeat("q", 10000), "synthetic crash", "", 1},
		{"unicode", "Darwin", strings.Repeat("水", 1000), strings.Repeat("杉", 1000), 2},
		{"digits", "ios", "synthetic crash 123", "input_runtime::page::select", 2},
		{"uuid", "ios", "01234567-89ab-cdef-0123-456789abcdef", "frame", 1},
		{"upper_frame", "ios", "synthetic crash", "INPUT_RUNTIME::PAGE::SELECT", 2},
		{"whitespace", "ios", "synthetic\tcrash", "input_runtime::page::select", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = crashSignature(tc.platform, tc.message, tc.stack) })
			if want := previousCrashSignatureDigest(tc.platform, tc.message, tc.stack); got != want {
				t.Fatalf("签名=%q，原签名=%q", got, want)
			}
			if allocations > tc.maximum {
				t.Fatalf("分配 %.0f 次，预算最多 %.0f 次", allocations, tc.maximum)
			}
		})
	}
}

func TestCrashSignatureDigestMemoryBudget(t *testing.T) {
	for _, tc := range []struct{ name, platform, message, stack string }{
		{"maximum_message", "ios", strings.Repeat("q", telemetryMessageLimit), ""},
		{"message", "ios", strings.Repeat("q", 10000), ""},
		{"frame", "ios", "synthetic crash", strings.Repeat("z", 10000)},
		{"both", "ios", strings.Repeat("q", 10000), strings.Repeat("z", 10000)},
		{"platform", strings.Repeat("q", 10000), "synthetic crash", "frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			measured := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					crashDigestResult = crashSignature(tc.platform, tc.message, tc.stack)
				}
			})
			if want := previousCrashSignatureDigest(tc.platform, tc.message, tc.stack); crashDigestResult != want {
				t.Fatalf("签名=%q，原签名=%q", crashDigestResult, want)
			}
			if bytes := measured.AllocedBytesPerOp(); bytes > 64 {
				t.Fatalf("分配 %d 字节，预算最多 64 字节", bytes)
			}
		})
	}
}

func TestCrashSignatureDigestCompatibility(t *testing.T) {
	check := func(platform, message, stack string) {
		t.Helper()
		if got, want := crashSignature(platform, message, stack), previousCrashSignatureDigest(platform, message, stack); got != want {
			t.Fatalf("平台=%q 消息=%q 堆栈=%q：签名=%q，原签名=%q", platform, message, stack, got, want)
		}
	}
	for _, platform := range []string{"", "ios", "WIN", "Mac", "Darwin", "iPadOS", "OHOS", "custom\nplatform", "水杉", "\xff"} {
		for _, message := range []string{"", " \t\r\n", "synthetic crash", "fault 123 at 0xabcdef", "水杉崩溃\nsecond", "\xff\xfe", "01234567-89ab-cdef-0123-456789abcdef"} {
			for _, stack := range []string{"", "frame", "FRAME 123", "\xff\xfe", "std::panicking::begin_panic\ninput::frame", strings.Repeat("std::panicking::begin_panic\n", crashStackLines-1) + "input::frame", strings.Repeat("std::panicking::begin_panic\n", crashStackLines) + "input::frame"} {
				check(platform, message, stack)
			}
		}
	}
	// 覆盖 SHA 块和固定缓冲的边界，以及字段在摘要中的不同切分位置。
	for _, n := range []int{0, 1, 55, 56, 63, 64, 65, 127, 128, 129, 255, 256, 257, 511, 512, 513, 1000, 10000} {
		for _, m := range []int{0, 1, 63, 64, 65, 255, 256, 257} {
			check(strings.Repeat("p", m), strings.Repeat("q", n), strings.Repeat("z", m))
			check("ios", strings.Repeat("水", n), strings.Repeat("杉", m))
		}
	}
	rng := rand.New(rand.NewSource(173))
	alphabet := []byte("abcXYZ0123456789 \t\r\n:_-+()[]/\\\xff\xfe水杉")
	for range 1000 {
		var parts [3]string
		for i := range parts {
			buf := make([]byte, rng.Intn(600))
			for j := range buf {
				buf[j] = alphabet[rng.Intn(len(alphabet))]
			}
			parts[i] = string(buf)
		}
		check(parts[0], parts[1], parts[2])
	}
}

func FuzzCrashSignatureDigestCompatibility(f *testing.F) {
	for _, seed := range [][3]string{
		{"", "", ""}, {"iPadOS", "synthetic crash", "std::panicking::begin_panic\ninput::frame"},
		{"Darwin", "水杉\xff 123", "FRAME 0xabcdef"},
		{"ios", strings.Repeat("q", 1000), strings.Repeat("z", 10000)},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, platform, message, stack string) {
		if got, want := crashSignature(platform, message, stack), previousCrashSignatureDigest(platform, message, stack); got != want {
			t.Fatalf("签名=%q，原签名=%q", got, want)
		}
	})
}

func BenchmarkCrashSignatureDigest(b *testing.B) {
	for _, tc := range []struct{ name, platform, message, stack string }{
		{"short", "ios", "a", ""},
		{"plain", "ios", "synthetic crash", "input_runtime::page::select"},
		{"maximum_message", "ios", strings.Repeat("q", telemetryMessageLimit), ""},
		{"long_frame", "ios", "synthetic crash", strings.Repeat("z", 10000)},
		{"maximum_message_long_frame", "ios", strings.Repeat("q", telemetryMessageLimit), strings.Repeat("z", 10000)},
	} {
		for _, impl := range []struct {
			name      string
			signature func(string, string, string) string
		}{{"previous", previousCrashSignatureDigest}, {"stream", crashSignature}} {
			b.Run(fmt.Sprintf("%s/%s", tc.name, impl.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashDigestResult = impl.signature(tc.platform, tc.message, tc.stack)
				}
			})
		}
	}
}
