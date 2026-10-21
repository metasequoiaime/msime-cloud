package account

import (
	"bytes"
	"encoding/json"
	"testing"
)

var preferenceBooleanSink bool

func TestPreferenceBooleanCompatibility(t *testing.T) {
	check := func(raw []byte) {
		t.Helper()
		var value bool
		want := json.Unmarshal(raw, &value) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
		if got := validPreference("general.enable_emoji", raw); got != want {
			t.Fatalf("布尔值 %q：得到 %v，期望 %v", raw, got, want)
		}
	}
	for _, raw := range []string{"", "true", "false", "null", "TRUE", "False", `"true"`, "0", "1", "[]", "{}", "true false", "truex", "false,", "tru", "fals", "\u00a0true", "false\u3000", "\xfftrue", "false\xfe", "\xef\xbb\xbftrue"} {
		check([]byte(raw))
	}
	for _, literal := range []string{"true", "false", "null"} {
		for c := range 128 {
			check(append([]byte{byte(c)}, literal...))
			check(append([]byte(literal), byte(c)))
		}
		check([]byte(" \t\r\n" + literal + "\n\r\t "))
	}
}

func TestPreferenceBooleanAllocationBudget(t *testing.T) {
	for _, raw := range []string{"true", "false", " \t\r\ntrue\n\r\t ", " \tfalse\n"} {
		t.Run(raw, func(t *testing.T) {
			value := json.RawMessage(raw)
			allocs := testing.AllocsPerRun(100, func() { preferenceBooleanSink = validPreference("general.enable_emoji", value) })
			if !preferenceBooleanSink {
				t.Fatal("合法布尔值被拒绝")
			}
			if allocs != 0 {
				t.Fatalf("布尔偏好校验分配 %.0f 次，期望零分配", allocs)
			}
		})
	}
}

func BenchmarkPreferenceBoolean(b *testing.B) {
	for _, tc := range []struct{ name, raw string }{
		{"true", "true"},
		{"false", "false"},
		{"whitespace", " \t\r\ntrue\n\r\t "},
	} {
		b.Run(tc.name, func(b *testing.B) {
			raw := json.RawMessage(tc.raw)
			b.ReportAllocs()
			for b.Loop() {
				preferenceBooleanSink = validPreference("general.enable_emoji", raw)
				if !preferenceBooleanSink {
					b.Fatal("合法布尔值被拒绝")
				}
			}
		})
	}
}
