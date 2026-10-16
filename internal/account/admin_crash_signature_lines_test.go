package account

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"strings"
	"testing"
)

// 保留原有整栈拆行方式，验证遍历优化不改变已有分组签名。
func previousCrashSignatureLines(platform, message, stack string) string {
	normalized := normalizeCrashMessage(message)
	frame := ""
	lines := strings.Split(stack, "\n")
	if len(lines) > crashStackLines {
		lines = lines[:crashStackLines]
	}
	for _, line := range lines {
		if candidate := normalizeCrashFrame(line); candidate != "" && !crashSystemFrame(candidate) {
			frame = candidate
			break
		}
	}
	if normalized == "" && frame == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(crashPlatform(platform) + "\n" + normalized + "\n" + frame))
	return hex.EncodeToString(sum[:8])
}

func TestCrashSignatureLineLimit(t *testing.T) {
	const own = "input_runtime::page::select"
	for _, prefix := range []string{"\n", "std::panicking::begin_panic\r\n"} {
		for _, count := range []int{0, 1, crashStackLines - 1, crashStackLines, crashStackLines + 1} {
			stack := strings.Repeat(prefix, count) + own
			want := crashSignature("ios", "合成崩溃", own)
			if count >= crashStackLines {
				want = crashSignature("ios", "合成崩溃", "")
			}
			if got := crashSignature("ios", "合成崩溃", stack); got != want {
				t.Fatalf("前置行=%d，签名=%s，期望 %s", count, got, want)
			}
		}
	}
}

func TestCrashSignatureLinesMatchPrevious(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 9))
	frames := []string{"", "\r", "std::panicking::begin_panic", "\tat java.util.ArrayList.get(ArrayList.java:437)", "2 MSIME 0x01 Keyboard.layout() + 1", "\u2003input_runtime::page::select\u2003", "#说明\xff", "ntdll.dll!RtlUserThreadStart"}
	for i := range 200 {
		lines := make([]string, random.IntN(2*crashStackLines))
		for j := range lines {
			lines[j] = frames[random.IntN(len(frames))]
		}
		stack := strings.Join(lines, "\n")
		for _, message := range []string{"", "  \n ", "EXC_BAD_ACCESS at 0x10\n后续内容"} {
			want := previousCrashSignatureLines("iPadOS", message, stack)
			if got := crashSignature("iPadOS", message, stack); got != want {
				t.Fatalf("样本=%d，签名=%s，原签名=%s", i, got, want)
			}
		}
	}
}

func TestCrashSignatureLineMemoryBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stack string
	}{
		{"首行即应用帧", "input_runtime::page::select\n" + strings.Repeat("\n", 100000)},
		{"只有上限外有应用帧", strings.Repeat("\n", 100000) + "input_runtime::page::select"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := previousCrashSignatureLines("ios", "合成崩溃", tc.stack)
			var signature string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					signature = crashSignature("ios", "合成崩溃", tc.stack)
				}
			})
			if signature != want {
				t.Fatalf("签名=%s，原签名=%s", signature, want)
			}
			t.Logf("%s; %s", result, result.MemString())
			// 预算覆盖最多 256 行的规范化，以及 race 下正则对象池丢弃缓存的固定开销。
			// 旧实现拆分十万行约需 1.6 MiB，仍会超出预算。
			if bytes := result.AllocedBytesPerOp(); bytes > 128<<10 {
				t.Fatalf("长栈签名分配 %d 字节，超过 128 KiB 预算", bytes)
			}
		})
	}
}

func BenchmarkCrashSignatureLines(b *testing.B) {
	for _, tc := range []struct {
		name  string
		stack string
	}{
		{"普通栈", "std::panicking::begin_panic\ninput_runtime::page::select"},
		{"首行即应用帧长后缀", "input_runtime::page::select\n" + strings.Repeat("\n", 100000)},
		{"上限外应用帧", strings.Repeat("\n", 100000) + "input_runtime::page::select"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			want := previousCrashSignatureLines("ios", "合成崩溃", tc.stack)
			b.ReportAllocs()
			for b.Loop() {
				if got := crashSignature("ios", "合成崩溃", tc.stack); got != want {
					b.Fatalf("签名=%s，原签名=%s", got, want)
				}
			}
		})
	}
}
