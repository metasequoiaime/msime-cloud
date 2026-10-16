package account

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundedDiagnosticArgumentsByteCompatibility(t *testing.T) {
	cases := []string{
		"", " \n\r\t", "null", "\u2003null\u00a0", " \n{\"limit\":3} \t", "[1,true,null]", `"水杉"`, "false", "123", "not JSON", "\x00", "a\xfftail",
		"\x85{}\xa0", "\u2003{\"limit\":3}\u3000", `{"kind":"\u0000"}`, `{"kind":"\ud800"}`, `{"limit":1e200000}`,
		strings.Repeat("a", 1<<20) + "\xff\x00tail",
		"\xff" + strings.Repeat("a", 1<<20) + "\x00tail",
		strings.Repeat(" \t", 1<<18) + `{"limit":3}` + strings.Repeat("\u2003", 1<<17),
	}
	rng := rand.New(rand.NewSource(20261016))
	for i := 0; i < 500; i++ {
		raw := make([]byte, rng.Intn(2048))
		if _, err := rng.Read(raw); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, string(raw))
	}
	for i, text := range cases {
		raw := json.RawMessage(text)
		original := bytes.Clone(raw)
		if got, want := boundedDiagnosticArguments(raw), boundedDiagnosticArgumentsBeforeBytes(raw); !bytes.Equal(got, want) {
			t.Fatalf("第 %d 组完整参数编码不一致：实际 %q，期望 %q", i, got, want)
		}
		if !bytes.Equal(raw, original) {
			t.Fatalf("第 %d 组输入被修改", i)
		}
	}
}

func TestBoundedDiagnosticArgumentsOwnsResult(t *testing.T) {
	for _, text := range []string{`{"limit":3}`, " \t{\"limit\":3}\n", "not JSON", "null", "\xfftail"} {
		raw := json.RawMessage(text)
		got := boundedDiagnosticArguments(raw)
		want := bytes.Clone(got)
		for i := range raw {
			raw[i] = 'x'
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("修改输入影响了返回值：%q", text)
		}
		input := bytes.Clone(raw)
		got[0] = 'y'
		if !bytes.Equal(raw, input) {
			t.Fatalf("修改返回值影响了输入：%q", text)
		}
	}
}

func TestBoundedDiagnosticArgumentsValidJSONAllocationBudget(t *testing.T) {
	raw := json.RawMessage(` {"limit":3} `)
	var got json.RawMessage
	allocations := testing.AllocsPerRun(100, func() { got = boundedDiagnosticArguments(raw) })
	if !bytes.Equal(got, []byte(`{"limit":3}`)) {
		t.Fatal("合法 JSON 未原样保留")
	}
	if allocations > 1 {
		t.Fatalf("合法 JSON 分配 %.0f 次，预算为独立返回值的 1 次分配", allocations)
	}
}

func TestBoundedDiagnosticArgumentsByteMemoryBudget(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"discarded_tail", strings.Repeat("a", 1<<20) + "\xff\x00tail"},
		{"normalized_prefix", "\xff" + strings.Repeat("a", 1<<20) + "\x00tail"},
		{"padded_json", strings.Repeat(" ", 1<<20) + `{"limit":3}` + "\u2003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(tc.text)
			want := boundedDiagnosticArgumentsBeforeBytes(raw)
			if got := boundedDiagnosticArguments(raw); !bytes.Equal(got, want) {
				t.Fatal("完整参数编码结果变化")
			}
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					boundedDiagnosticArguments(raw)
				}
			})
			if allocated := result.AllocedBytesPerOp(); allocated > 16<<10 {
				t.Fatalf("每次参数处理分配 %d 字节，预算 16 KiB", allocated)
			}
		})
	}
}

func BenchmarkBoundedDiagnosticArguments(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"valid_json", ` {"limit":3} `},
		{"invalid_short", "not JSON"},
		{"controls_1024", strings.Repeat("\x01", 1024)},
		{"discarded_tail", strings.Repeat("a", 1<<20) + "\xff\x00tail"},
		{"normalized_prefix", "\xff" + strings.Repeat("a", 1<<20) + "\x00tail"},
		{"padded_json", strings.Repeat(" ", 1<<20) + `{"limit":3}` + "\u2003"},
	} {
		raw := json.RawMessage(tc.text)
		for _, implementation := range []struct {
			name  string
			bound func(json.RawMessage) json.RawMessage
		}{
			{"before", boundedDiagnosticArgumentsBeforeBytes},
			{"after", boundedDiagnosticArguments},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					implementation.bound(raw)
				}
			})
		}
	}
}

// 以下冻结本次改动前的完整处理路径，用于兼容性差分和配对基准。
func boundedDiagnosticArgumentsBeforeBytes(arguments json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(arguments))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`{}`)
	}
	if len(trimmed) <= diagnosticAccessArgumentBytes && utf8.ValidString(trimmed) && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return quotedDiagnosticArgumentsBeforeBytes(trimmed)
}

func quotedDiagnosticArgumentsBeforeBytes(trimmed string) json.RawMessage {
	cut := trimmed[:min(len(trimmed), diagnosticAccessArgumentBytes)]
	if !utf8.ValidString(cut) || strings.IndexByte(cut, 0) >= 0 {
		// 只生成上限内的规范化前缀；连续非法字节仍合并成一个替代字符。
		var buffer [diagnosticAccessArgumentBytes]byte
		prefix := buffer[:0]
		invalid := false
		for offset := 0; offset < len(trimmed) && len(prefix) < len(buffer); {
			r, size := utf8.DecodeRuneInString(trimmed[offset:])
			offset += size
			if r == utf8.RuneError && size == 1 {
				if invalid {
					continue
				}
				invalid = true
			} else {
				invalid = false
			}
			if r == 0 {
				// 实际 NUL 转成可见文本，允许像旧实现一样在这段 ASCII 文本中间截断。
				replacement := `\u0000`
				prefix = append(prefix, replacement[:min(len(replacement), len(buffer)-len(prefix))]...)
			} else {
				if utf8.RuneLen(r) > len(buffer)-len(prefix) {
					break
				}
				prefix = utf8.AppendRune(prefix, r)
			}
		}
		cut = string(prefix)
	}

	end := len(cut)
	// 每个源字节最多转义成六个 JSON 字节；短文本无需逐字计算预算。
	if len(cut) > (diagnosticAccessArgumentBytes-2)/6 {
		end = 0
		encoded := 2 // 为 JSON 字符串两侧的引号留出空间。
		for end < len(cut) {
			r, size := utf8.DecodeRuneInString(cut[end:])
			width := size
			switch r {
			case '\\', '"', '\b', '\f', '\n', '\r', '\t':
				width = 2
			case '<', '>', '&', '\u2028', '\u2029':
				width = 6
			default:
				if r < 0x20 {
					width = 6
				}
			}
			if encoded+width > diagnosticAccessArgumentBytes {
				break
			}
			encoded += width
			end += size
		}
	}
	quoted, _ := json.Marshal(cut[:end])
	return quoted
}
