package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQuotedDiagnosticArgumentsCompatibility(t *testing.T) {
	check := func(t *testing.T, text string) {
		t.Helper()
		got, want := quotedDiagnosticArguments(text), quotedDiagnosticArgumentsBeforePrefix(text)
		if !bytes.Equal(got, want) {
			t.Fatalf("截断后的 JSON 不一致：实际 %q，期望 %q", got, want)
		}
		var decoded string
		if len(got) > diagnosticAccessArgumentBytes || json.Unmarshal(got, &decoded) != nil || !utf8.ValidString(decoded) || strings.ContainsRune(decoded, 0) {
			t.Fatalf("截断结果不满足存储约束：%q", got)
		}
	}
	for _, text := range []string{
		"", "plain text", "水杉😀é", "a\xff\xc0\xafz", "a\xef\xbf\xbd\xffz", "\xe6\xb0",
		strings.Repeat("\xff", 2048) + "tail",
		strings.Repeat("\xff", 2048) + "\x00tail",
		strings.Repeat("\x00", 2048),
		strings.Repeat("\\\"\b\f\n\r\t<>&\u2028\u2029水😀", 150),
	} {
		check(t, text)
	}
	for value := range 256 {
		t.Run(fmt.Sprintf("byte_%02x", value), func(t *testing.T) {
			check(t, strings.Repeat(string([]byte{byte(value)}), 1024))
		})
	}
	for prefix := 1008; prefix <= 1026; prefix++ {
		for _, suffix := range []string{"\x00tail", "水tail", "😀tail", "\u2028tail", "\xff\xc0\xaftail", "<>&tail", "\\\"tail"} {
			check(t, strings.Repeat("a", prefix)+suffix)
		}
	}
	rng := rand.New(rand.NewSource(20261016))
	alphabet := []string{"a", "水", "😀", "é", "\ufffd", "<", ">", "&", "\u2028", "\u2029", "\x00", "\x01", "\b", "\f", "\n", "\r", "\t", "\\", "\"", "\xff", "\xc0\xaf", "\xe6\xb0"}
	for i := 0; i < 300; i++ {
		var text strings.Builder
		for n := rng.Intn(1100); n > 0; n-- {
			text.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		check(t, text.String())
	}
}

func TestQuotedDiagnosticArgumentsAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"controls", strings.Repeat("\x01", 1024)},
		{"html", strings.Repeat("<>&", 342)},
		{"nul", strings.Repeat("\x00", 1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got json.RawMessage
			allocations := testing.AllocsPerRun(10, func() {
				got = quotedDiagnosticArguments(tc.text)
			})
			if len(got) > diagnosticAccessArgumentBytes || !json.Valid(got) {
				t.Fatalf("结果不是上限内的 JSON：%q", got)
			}
			if allocations > 8 {
				t.Fatalf("截断分配 %.0f 次，预算 8 次", allocations)
			}
		})
	}
}

func TestQuotedDiagnosticArgumentsLongTailMemoryBudget(t *testing.T) {
	text := strings.Repeat("a", 1<<20) + "\xff\x00tail"
	if got := quotedDiagnosticArguments(text); len(got) != diagnosticAccessArgumentBytes || !json.Valid(got) {
		t.Fatal("长尾输入没有返回预期大小的 JSON 字符串")
	}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			quotedDiagnosticArguments(text)
		}
	})
	if allocated := result.AllocedBytesPerOp(); allocated > 16<<10 {
		t.Fatalf("长尾截断每次分配 %d 字节，预算 16 KiB", allocated)
	}
}

func BenchmarkQuotedDiagnosticArguments(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"short", "not JSON"},
		{"ascii_1024", strings.Repeat("a", 1024)},
		{"controls_1024", strings.Repeat("\x01", 1024)},
		{"nul_1024", strings.Repeat("\x00", 1024)},
		{"html_1024", strings.Repeat("<>&", 342)},
		{"unicode", strings.Repeat("水😀\u2028", 150)},
		{"long_dirty_tail", strings.Repeat("a", 1<<20) + "\xff\x00tail"},
	} {
		for _, implementation := range []struct {
			name  string
			quote func(string) json.RawMessage
		}{
			{"before", quotedDiagnosticArgumentsBeforePrefix},
			{"after", quotedDiagnosticArguments},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					implementation.quote(tc.text)
				}
			})
		}
	}
}

// quotedDiagnosticArgumentsBeforePrefix 保留旧实现，固定字节级截断语义并用于配对基准。
func quotedDiagnosticArgumentsBeforePrefix(trimmed string) json.RawMessage {
	// 无效字节替换为可见字符，保留其后的内容，再按 UTF-8 边界截断。
	trimmed = strings.ToValidUTF8(trimmed, "\ufffd")
	// 实际 NUL 也不能存入 `jsonb` 字符串，保留为可见的转义文本。
	trimmed = strings.ReplaceAll(trimmed, "\x00", `\u0000`)
	cut := trimmed[:min(len(trimmed), diagnosticAccessArgumentBytes)]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	quoted, _ := json.Marshal(cut)
	// 转义可能让字符串变长，继续缩短直到不超过上限。
	for len(quoted) > diagnosticAccessArgumentBytes {
		cut = cut[:len(cut)-1]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		quoted, _ = json.Marshal(cut)
	}
	return quoted
}
