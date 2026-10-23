package account

import (
	"bytes"
	"encoding/json"
	"testing"
)

var preferenceIntegerSink bool

func TestPreferenceIntegerCompatibility(t *testing.T) {
	for _, key := range []string{"appearance.font_size", "platform.android.keyboard_height_adjustment", "platform.android.handwriting_delay_ms", "platform.android.handwriting_stroke_width"} {
		field := preferenceFields[key]
		low, high := int64(0), int64(1000000)
		if field.Minimum != nil {
			low = *field.Minimum
		}
		if field.Maximum != nil {
			high = *field.Maximum
		}
		check := func(raw []byte) {
			t.Helper()
			before := bytes.Clone(raw)
			var value int64
			want := json.Unmarshal(raw, &value) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && value >= low && value <= high
			if got := validPreference(key, raw); got != want {
				t.Fatalf("整数偏好 %s=%q：得到 %v，期望 %v", key, raw, got, want)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("整数校验修改了原始字节")
			}
		}
		for _, raw := range []string{"", "0", "-0", "1", "-1", "-47", "-46", "55", "56", "199", "200", "1500", "1501", "8", "9", "999999", "1000000", "1000001", "9223372036854775807", "-9223372036854775808", "9223372036854775808", "-9223372036854775809", "999999999999999999999999", "+1", "01", "-01", "00", "-", "--1", "0x1", "1_0", "1.0", "1e0", "1E+0", "1\x00", "1 2", "null", "true", `"1"`, "[]", "{}", "1,", "\u00a01", "1\u3000", "\xef\xbb\xbf1", "\xff1", "1\xfe"} {
			check([]byte(raw))
		}
		for _, literal := range []string{"0", "-0", "1", "-46", "1000000", "null"} {
			for c := range 128 {
				check(append([]byte{byte(c)}, literal...))
				check(append([]byte(literal), byte(c)))
			}
			check([]byte(" \t\r\n" + literal + "\n\r\t "))
		}
	}
}

func TestPreferenceIntegerAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ key, raw string }{
		{"appearance.font_size", "18"},
		{"appearance.font_size", "1000000"},
		{"appearance.font_size", " \t\r\n-0\n\r\t "},
		{"platform.android.keyboard_height_adjustment", "-46"},
		{"platform.android.handwriting_delay_ms", "1500"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			raw := json.RawMessage(tc.raw)
			allocs := testing.AllocsPerRun(100, func() { preferenceIntegerSink = validPreference(tc.key, raw) })
			if !preferenceIntegerSink {
				t.Fatal("合法整数值被拒绝")
			}
			if allocs != 0 {
				t.Fatalf("整数偏好校验分配 %.0f 次，期望零分配", allocs)
			}
		})
	}
}

func BenchmarkPreferenceInteger(b *testing.B) {
	for _, tc := range []struct{ name, key, raw string }{
		{"positive", "appearance.font_size", "18"},
		{"maximum", "appearance.font_size", "1000000"},
		{"negative", "platform.android.keyboard_height_adjustment", "-46"},
		{"whitespace", "appearance.font_size", " \t\r\n-0\n\r\t "},
	} {
		b.Run(tc.name, func(b *testing.B) {
			raw := json.RawMessage(tc.raw)
			b.ReportAllocs()
			for b.Loop() {
				preferenceIntegerSink = validPreference(tc.key, raw)
				if !preferenceIntegerSink {
					b.Fatal("合法整数值被拒绝")
				}
			}
		})
	}
}
