package account

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

type preferenceStringSample struct{ name, key, raw string }

func preferenceStringSamples() []preferenceStringSample {
	photo, _ := json.Marshal(`{"photo":"data:image/jpeg;base64,` + strings.Repeat("x", 780000) + `"}`)
	return []preferenceStringSample{
		{"empty", "appearance.font", `""`},
		{"ascii", "appearance.font", `"Arial"`},
		{"unicode", "appearance.font", `"微软雅黑😀"`},
		{"whitespace", "appearance.font", " \t\r\n\"Arial\"\n\r\t "},
		{"escaped_unicode", "appearance.font", `"\ud83d\ude00\/\u4e2d\n"`},
		{"prompt", "ai_assistant.prompt", `"` + strings.Repeat("x", 8192) + `"`},
		{"escaped_photo", "platform.ios.custom_keyboard_skin", string(photo)},
	}
}

// 保留旧解码器及原 Unicode 检查作为对照，期望值不依赖新的长度计数。
func legacyValidPreferenceString(key string, raw json.RawMessage) bool {
	field, ok := preferenceFields[key]
	if !ok || field.Type != "string" || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	max := 1024
	if field.MaxLength > 0 {
		max = field.MaxLength
	}
	if strings.Contains(key, "prompt") {
		max = 8192
	}
	var value string
	return json.Unmarshal(raw, &value) == nil && len(value) <= max && !strings.ContainsRune(value, 0) && legacyPreferenceStringUnicode(raw)
}

func checkPreferenceStringCompatibility(t *testing.T, key string, raw []byte) {
	t.Helper()
	before := bytes.Clone(raw)
	want := legacyValidPreferenceString(key, raw)
	if got := validPreference(key, raw); got != want {
		t.Fatalf("字符串偏好 %s=%q：得到 %v，期望 %v", key, raw, got, want)
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("字符串校验修改了原始字节")
	}
}

func TestPreferenceStringCompatibility(t *testing.T) {
	for _, key := range []string{"appearance.font", "ai_assistant.prompt", "platform.ios.keyboard_layout", "platform.ios.custom_keyboard_skin"} {
		for _, raw := range []string{"", `""`, `"Arial"`, `"微软雅黑😀"`, `"a\/b"`, `"a\nb"`, `"\u0000"`, `"\u0001"`, `"\u007f\u0080\u07ff\u0800\uffff"`, `"\ud800\udc00"`, `"\udbff\udfff"`, `"\ud800"`, `"\udc00"`, `"\\ud800"`, `"\u005cud800"`, `"\ud800\\udc00"`, `"\ud800\ud800"`, `"\ufffd"`, `"\u12"`, `"\x41"`, `"a" "b"`, `"a"true`, `"a`, `"a"x`, "\"a\nb\"", "\"a\x00b\"", "\"\xff\"", "\xff", "\u00a0\"Arial\"", "\"Arial\"\u3000", "\xef\xbb\xbf\"Arial\"", "null", "true", "1", "[]", "{}"} {
			checkPreferenceStringCompatibility(t, key, []byte(raw))
		}
		for _, literal := range []string{`""`, `"Arial"`, `"\ud83d\ude00"`, `"\u0000"`, "null"} {
			for c := range 128 {
				checkPreferenceStringCompatibility(t, key, append([]byte{byte(c)}, literal...))
				checkPreferenceStringCompatibility(t, key, append([]byte(literal), byte(c)))
			}
			checkPreferenceStringCompatibility(t, key, []byte(" \t\r\n"+literal+"\n\r\t "))
		}
	}
}

func TestPreferenceStringByteLimits(t *testing.T) {
	for _, tc := range []struct {
		key   string
		limit int
	}{{"platform.ios.keyboard_layout", 32}, {"appearance.font", 1024}, {"ai_assistant.prompt", 8192}} {
		for _, unit := range []struct {
			raw   string
			bytes int
		}{{"x", 1}, {"é", 2}, {"中", 3}, {"😀", 4}, {`\"`, 1}, {`\n`, 1}, {`\/`, 1}, {`\\`, 1}, {`\u0001`, 1}, {`\u0041`, 1}, {`\u00e9`, 2}, {`\u4e2d`, 3}, {`\ud83d\ude00`, 4}} {
			for delta := range 2 {
				length := tc.limit + delta
				raw := []byte(`"` + strings.Repeat(unit.raw, length/unit.bytes) + strings.Repeat("x", length%unit.bytes) + `"`)
				if got, want := validPreference(tc.key, raw), delta == 0; got != want {
					t.Fatalf("字符串偏好 %s unit=%q bytes=%d：得到 %v，期望 %v", tc.key, unit.raw, length, got, want)
				}
				checkPreferenceStringCompatibility(t, tc.key, raw)
			}
		}
	}
}

var preferenceStringSink bool

func TestPreferenceStringAllocationBudget(t *testing.T) {
	for _, tc := range preferenceStringSamples() {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(tc.raw)
			allocs := testing.AllocsPerRun(20, func() { preferenceStringSink = validPreference(tc.key, raw) })
			if !preferenceStringSink {
				t.Fatal("合法字符串偏好被拒绝")
			}
			if allocs != 0 {
				t.Fatalf("字符串偏好校验分配 %.0f 次，期望零分配", allocs)
			}
		})
	}
}

func FuzzPreferenceStringCompatibility(f *testing.F) {
	for _, raw := range []string{`"Arial"`, `"微软雅黑😀"`, `"a\/b"`, `"a\nb"`, `"\u0000"`, `"\u0001"`, `"\u007f\u0080\u07ff\u0800\uffff"`, `"\ud800\udc00"`, `"\udbff\udfff"`, `"\ud800"`, `"\udc00"`, `"\\ud800"`, `"\u005cud800"`, "\xff", "\"\xff\"", " \t\"Arial\"\n"} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		for _, key := range []string{"appearance.font", "ai_assistant.prompt", "platform.ios.keyboard_layout", "platform.ios.custom_keyboard_skin"} {
			checkPreferenceStringCompatibility(t, key, raw)
		}
	})
}

func BenchmarkPreferenceString(b *testing.B) {
	for _, tc := range preferenceStringSamples() {
		b.Run(tc.name, func(b *testing.B) {
			raw := json.RawMessage(tc.raw)
			if !validPreference(tc.key, raw) {
				b.Fatal("合法字符串偏好被拒绝")
			}
			b.ReportAllocs()
			for b.Loop() {
				preferenceStringSink = validPreference(tc.key, raw)
				if !preferenceStringSink {
					b.Fatal("合法字符串偏好被拒绝")
				}
			}
		})
	}
}

func legacyPreferenceStringUnicode(raw json.RawMessage) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		code, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if len(raw)-i-1 < 6 || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, _ := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
