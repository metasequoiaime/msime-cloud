package account

import (
	"math/rand"
	"strings"
	"testing"
)

func TestCrashMessageCopyCompatibility(t *testing.T) {
	inputs := []string{
		"", "\n \t\r\n", "synthetic crash", "EXC_BAD_ACCESS", "合成崩溃消息",
		"first nonblank line\n0xdeadbeef", "a b c", "a  b", "a\tb", "a\fb", "a\rb",
		"a\vb", "a\u00a0b", "a\u3000b", "a \v b", "  a b  ", "\xff\xfe synthetic crash",
		"deadbee", "deadbeef", "ABCDEFAB", "gdeadbeef", "deadbeefg", "_deadbeef_", "水deadbeef杉",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee_",
		"gaaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeeeg", "aaaaaaa-bbbb-cccc-dddd-eeeeeee",
		"0xABCDEF", "１２３４", "١٢٣٤", "a1b", "synthetic crash 123", "a\x00b",
	}
	random := rand.New(rand.NewSource(692))
	atoms := []string{"a", "B", "c", "D", "e", "F", "g", "0", "12", "deadbeef", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "_", "-", " ", "\t", "\n", "\r", "\v", "\f", "水", "😀", "\u00a0", "\u3000", "\xff", "\xfe"}
	for range 1000 {
		var message strings.Builder
		for range random.Intn(80) {
			message.WriteString(atoms[random.Intn(len(atoms))])
		}
		inputs = append(inputs, message.String())
	}
	for _, message := range inputs {
		if got, want := normalizeCrashMessage(message), previousNormalizeCrashMessageCopies(message); got != want {
			t.Fatalf("消息=%q，规范化=%q，原结果=%q", message, got, want)
		}
		const stack = "std::panicking::begin_panic\ninput_runtime::page::select"
		if got, want := crashSignature("darwin", message, stack), previousCrashSignatureCopies("darwin", message, stack); got != want {
			t.Fatalf("消息=%q，签名=%q，原签名=%q", message, got, want)
		}
	}
}

func TestCrashMessageAvoidsUnchangedCopies(t *testing.T) {
	for _, tc := range []struct {
		message string
		maximum float64
	}{
		{"synthetic crash message", 0},
		{strings.Repeat("合成崩溃消息", 100), 0},
		{"EXC_BAD_ACCESS", 1},
	} {
		want := previousNormalizeCrashMessageCopies(tc.message)
		allocs := testing.AllocsPerRun(100, func() {
			if got := normalizeCrashMessage(tc.message); got != want {
				t.Fatalf("消息=%q，规范化=%q，原结果=%q", tc.message, got, want)
			}
		})
		if allocs > tc.maximum {
			t.Errorf("消息字节数=%d，分配=%.0f，期望最多 %.0f 次", len(tc.message), allocs, tc.maximum)
		}
	}
}

func BenchmarkCrashMessageCopies(b *testing.B) {
	for _, tc := range []struct{ name, message string }{
		{"Lowercase", "synthetic crash message"},
		{"Uppercase", "EXC_BAD_ACCESS"},
		{"Unicode", strings.Repeat("合成崩溃消息", 100)},
		{"Digits", "synthetic crash 123"},
		{"Spaces", "synthetic\t crash  message"},
		{"LetterUUID", "_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee_"},
		{"Identifiers", "Crash 01234567-89ab-cdef-0123-456789abcdef abcd1234 at 0xABCDEF + 123"},
		{"LateDigit", strings.Repeat("合成崩溃消息", 100) + "123"},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{{"Previous", previousNormalizeCrashMessageCopies}, {"Current", normalizeCrashMessage}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashNormalizationResult = impl.normalize(tc.message)
				}
			})
		}
	}
}
