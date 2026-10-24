package account

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// 原 UUID 正则仅用于冻结参考与兼容性对照。
var crashUUIDPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// 冻结 UUID 扫描前的消息与签名流程，验证已有分组保持一致。
func previousNormalizeCrashUUIDMessage(message string) string {
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
	line = maskCrashHexRuns(line)
	line = maskCrashDigits(line)
	return normalizeCrashWhitespace(line)
}

func previousCrashUUIDSignature(platform, message, stack string) string {
	normalized := previousNormalizeCrashUUIDMessage(message)
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

func TestCrashUUIDMessageAllocationBudget(t *testing.T) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	for _, tc := range []struct {
		name, message string
		maximum       float64
	}{
		{"whole", uuid, 0},
		{"letters", "abcdefab-abcd-abcd-abcd-abcdefabcdef", 0},
		{"upper", strings.ToUpper(uuid), 0},
		{"bounded", "crash " + uuid + " value", 1},
		{"embedded", "module" + uuid + "symbol", 1},
		{"adjacent", uuid + uuid, 1},
		{"partial", "module" + strings.Repeat("1", 10000) + "-symbol", 1},
		{"long_letters", "g" + strings.Repeat("f", 10000) + "-symbol", 0},
		{"no_uuid", strings.Repeat("module", 1000) + "-123", 1},
		{"plain", "synthetic crash", 0},
		{"digits", "synthetic crash 123", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = normalizeCrashMessage(tc.message) })
			if want := previousNormalizeCrashUUIDMessage(tc.message); got != want {
				t.Fatalf("结果=%q，原结果=%q", got, want)
			}
			if allocations > tc.maximum {
				t.Fatalf("分配 %.0f 次，预算最多 %.0f 次", allocations, tc.maximum)
			}
		})
	}
}

func checkCrashUUIDCompatibility(t *testing.T, text string) {
	t.Helper()
	if got, want := maskCrashUUIDs(text), crashUUIDPattern.ReplaceAllString(text, "?"); got != want {
		t.Fatalf("输入=%q，掩码=%q，正则结果=%q", text, got, want)
	}
	if got, want := normalizeCrashMessage(text), previousNormalizeCrashUUIDMessage(text); got != want {
		t.Fatalf("输入=%q，消息=%q，原消息=%q", text, got, want)
	}
	const stack = "0 libsystem_kernel.dylib 0x100 __pthread_kill + 8\n1 msime 0x200 keyboard.layout() + 12"
	if got, want := crashSignature("MacOS", text, stack), previousCrashUUIDSignature("MacOS", text, stack); got != want {
		t.Fatalf("输入=%q，签名=%q，原签名=%q", text, got, want)
	}
}

func TestCrashUUIDCompatibility(t *testing.T) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	for _, text := range []string{
		"", "plain", "-", uuid, strings.ToUpper(uuid),
		"abcdefab-abcd-abcd-abcd-abcdefabcdef", "00000000-0000-0000-0000-000000000000",
		"module" + uuid + "symbol", "_" + uuid + "_", "f" + uuid + "f", "水" + uuid + "杉",
		"K" + uuid, "\xff" + uuid + "\xfe", uuid + uuid, uuid + "-" + uuid,
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"0x" + uuid, uuid + "123", "0X123 " + uuid + " deadbeef 42",
		"g" + strings.Repeat("f", 10000) + "-symbol", "module" + strings.Repeat("1", 10000) + "-symbol",
		strings.Repeat(uuid+" ", 1000), "first " + uuid + "\nsecond " + uuid,
	} {
		checkCrashUUIDCompatibility(t, text)
	}
	// 每个字节位置分别覆盖截断、插入、删除、Unicode 与非法 UTF-8。
	for pos := 0; pos <= len(uuid); pos++ {
		checkCrashUUIDCompatibility(t, uuid[:pos])
		for _, atom := range []string{"-", "f", "0", "g", "_", " ", "\n", "K", "水", "\xff", "\x00"} {
			checkCrashUUIDCompatibility(t, uuid[:pos]+atom+uuid[pos:])
			if pos < len(uuid) {
				checkCrashUUIDCompatibility(t, uuid[:pos]+atom+uuid[pos+1:])
			}
		}
		if pos < len(uuid) {
			checkCrashUUIDCompatibility(t, uuid[:pos]+uuid[pos+1:])
		}
	}
	random := rand.New(rand.NewSource(1508))
	atoms := []string{uuid, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "01234567", "abcdefab", "abcd", "0x", "123", "g", "_", "-", "?", " ", "\t", "\n", "\r", "\v", "K", "水", "é", "１２", "\xff", "\x00"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(40) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkCrashUUIDCompatibility(t, text.String())
	}
}

func TestCrashUUIDByteMutations(t *testing.T) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	check := func(text string) {
		t.Helper()
		if got, want := maskCrashUUIDs(text), crashUUIDPattern.ReplaceAllString(text, "?"); got != want {
			t.Fatalf("输入=%q，掩码=%q，正则结果=%q", text, got, want)
		}
	}
	for pos := range len(uuid) {
		for c := range 256 {
			check(uuid[:pos] + string([]byte{byte(c)}) + uuid[pos+1:])
		}
	}
	// 穷举外部字节对及首段、分隔符、末段附近的相邻字节对。
	for first := range 256 {
		for second := range 256 {
			pair := string([]byte{byte(first), byte(second)})
			check(pair[:1] + uuid + pair[1:])
			for _, pos := range []int{7, 12, 22, 34} {
				check(uuid[:pos] + pair + uuid[pos+2:])
			}
		}
	}
}

func TestCrashUUIDSpans(t *testing.T) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	for _, text := range []string{
		"", "---", uuid, "f" + uuid + "f", "module" + uuid + "symbol", uuid + uuid,
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"gdeadbeef " + uuid, "\xff" + uuid + "\xfe" + uuid,
	} {
		for from := 0; from <= len(text); from++ {
			want := -1
			if match := crashUUIDPattern.FindStringIndex(text[from:]); match != nil {
				want = from + match[0]
			}
			if got := nextCrashUUID(text, from); got != want {
				t.Fatalf("输入=%q，起点=%d，范围起点=%d，正则起点=%d", text, from, got, want)
			}
		}
	}
}

func TestCrashUUIDReplacementAllocationBudget(t *testing.T) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	for _, text := range []string{"", "plain", uuid, strings.ToUpper(uuid), "module" + uuid + "symbol", uuid + uuid, strings.Repeat("f", 10000) + "-symbol", strings.Repeat(uuid, 1000)} {
		want := crashUUIDPattern.ReplaceAllString(text, "?")
		maximum := float64(1)
		if text == want || want == "?" {
			maximum = 0
		}
		var got string
		allocations := testing.AllocsPerRun(100, func() { got = maskCrashUUIDs(text) })
		if got != want {
			t.Fatalf("结果=%q，正则结果=%q", got, want)
		}
		if allocations > maximum {
			t.Fatalf("输入长度=%d，分配 %.0f 次，预算最多 %.0f 次", len(text), allocations, maximum)
		}
	}
}

func TestCrashUUIDOutputSizeBudget(t *testing.T) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	for _, text := range []string{uuid, "crash " + uuid + " value", strings.Repeat(uuid, 1000), strings.Repeat(uuid+" ", 1000)} {
		want := crashUUIDPattern.ReplaceAllString(text, "?")
		measured := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				crashUUIDResult = maskCrashUUIDs(text)
			}
		})
		if crashUUIDResult != want {
			t.Fatalf("结果=%q，正则结果=%q", crashUUIDResult, want)
		}
		if bytes := measured.AllocedBytesPerOp(); bytes > int64(len(want)*2+64) {
			t.Fatalf("输出长度=%d，分配字节=%d", len(want), bytes)
		}
	}
}

func FuzzCrashUUIDCompatibility(f *testing.F) {
	for _, text := range []string{"", "01234567-89ab-cdef-0123-456789abcdef", "abcdefab-abcd-abcd-abcd-abcdefabcdef", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "0x01234567-89ab-cdef-0123-456789abcdef", "\xff01234567-89ab-cdef-0123-456789abcdef\xfe"} {
		f.Add(text)
	}
	f.Fuzz(checkCrashUUIDCompatibility)
}

var crashUUIDResult string

func BenchmarkCrashUUIDMessage(b *testing.B) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	for _, tc := range []struct{ name, text string }{
		{"Plain", "synthetic crash"}, {"Digits", "synthetic crash 123"},
		{"Whole", uuid}, {"Bounded", "crash " + uuid + " value"},
		{"Embedded", "module" + uuid + "symbol"}, {"Many", strings.Repeat(uuid+" ", 32)},
		{"NoUUID", strings.Repeat("module", 1000) + "-123"},
		{"LongDigits", "module" + strings.Repeat("1", 10000) + "-symbol"},
		{"LongLetters", "g" + strings.Repeat("f", 10000) + "-symbol"},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{
			{"Previous", previousNormalizeCrashUUIDMessage}, {"Current", normalizeCrashMessage},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashUUIDResult = impl.normalize(tc.text)
				}
			})
		}
	}
}

func BenchmarkCrashUUIDSignature(b *testing.B) {
	const uuid = "01234567-89ab-cdef-0123-456789abcdef"
	for _, tc := range []struct{ name, text string }{
		{"Plain", "synthetic crash"}, {"Digits", "synthetic crash 123"}, {"Whole", uuid},
		{"Embedded", "module" + uuid + "symbol"}, {"NoUUID", strings.Repeat("module", 1000) + "-123"},
	} {
		for _, impl := range []struct {
			name      string
			signature func(string, string, string) string
		}{
			{"Previous", previousCrashUUIDSignature}, {"Current", crashSignature},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashUUIDResult = impl.signature("ios", tc.text, "input_runtime::page::select")
				}
			})
		}
	}
}
