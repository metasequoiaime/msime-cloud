package account

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

var preferenceNumberSink bool

// 保留旧数字校验器作为独立对照，避免新实现复用同一套长度计算。
func legacyValidPreferenceNumber(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) || !json.Valid(raw) {
		return false
	}
	value := string(raw)
	negative := value[0] == '-'
	if negative {
		value = value[1:]
	}
	mantissa := value
	var exponent int64
	if i := strings.IndexAny(value, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(value[i+1:], 10, 32)
		if err != nil || exponent > maximumPreferenceNumericExponent || exponent < -maximumPreferenceNumericExponent {
			return false
		}
		mantissa = value[:i]
	}
	var fractionDigits int64
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fractionDigits = int64(len(mantissa) - i - 1)
	}
	if fractionDigits-exponent > maximumPreferenceNumericScale {
		return false
	}
	digits := strings.TrimLeft(strings.ReplaceAll(mantissa, ".", ""), "0")
	if digits == "" {
		return true
	}
	if negative {
		return false
	}
	integerDigits := int64(len(digits)) + exponent - fractionDigits
	if integerDigits < 7 {
		return true
	}
	return integerDigits == 7 && digits[0] == '1' && strings.Trim(digits[1:], "0") == ""
}

func TestPreferenceNumberCompatibility(t *testing.T) {
	check := func(raw []byte) {
		t.Helper()
		before := bytes.Clone(raw)
		want := legacyValidPreferenceNumber(raw)
		if got := validPreferenceNumber(raw); got != want {
			t.Fatalf("数字 %q：得到 %v，期望 %v", raw, got, want)
		}
		if !bytes.Equal(raw, before) {
			t.Fatal("数字校验修改了原始字节")
		}
	}
	for _, raw := range []string{
		"", "0", "-0", "1.5", "1000000", "1000000.000000000000000", "1e+6", "1e-400",
		"0." + strings.Repeat("0", 19999) + "1e20000", "1" + strings.Repeat("0", 1000) + "e-1000",
		"1e-16384", "-1", "1000001", "01", "+1", "null", "true", "1.", "1e",
		" \n1.5\t", "\u00a01.5", "1.5\u3000", string([]byte{'1', 0xff}),
		"1e" + strings.Repeat("0", 1000), "-0e-" + strings.Repeat("0", 1000),
	} {
		check([]byte(raw))
	}
	for _, integer := range []string{"0", "1", "10", "999999", "1000000", "1000001"} {
		for _, fraction := range []string{"", ".0", ".000001", ".5", ".1000", ".000000000000001"} {
			for _, exponent := range []string{"", "e0", "E+0", "e-0", "e1", "e-1", "e6", "e-6", "e-16383", "e-16384", "e1073741823", "e1073741824", "e-1073741823", "e2147483648"} {
				for _, sign := range []string{"", "-"} {
					raw := sign + integer + fraction + exponent
					check([]byte(raw))
					check([]byte(" \t\r\n" + raw + "\n\r\t "))
				}
			}
		}
	}
}

func TestPreferenceNumberAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"short", "1.5"},
		{"long_integer", "1" + strings.Repeat("0", 1000) + "e-1000"},
		{"long_fraction", "0." + strings.Repeat("0", 19999) + "1e20000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(tc.raw)
			allocs := testing.AllocsPerRun(20, func() { preferenceNumberSink = validPreferenceNumber(raw) })
			if !preferenceNumberSink {
				t.Fatal("合法数字被拒绝")
			}
			if allocs != 0 {
				t.Fatalf("数字校验分配 %.0f 次，期望零分配", allocs)
			}
		})
	}
}

func FuzzPreferenceNumberCompatibility(f *testing.F) {
	for _, raw := range []string{"0", "-0", "1.5", "1000000", "1000000.000000001", "1e-400", "0e1073741823", "1e-16384", "1.0000001", " \t1.5\n", "0.000001e12", "1" + strings.Repeat("0", 1000) + "e-1000", "0." + strings.Repeat("0", 19999) + "1e20000"} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		want := legacyValidPreferenceNumber(raw)
		if got := validPreferenceNumber(raw); got != want {
			t.Fatalf("数字 %q：得到 %v，期望 %v", raw, got, want)
		}
	})
}

func BenchmarkPreferenceNumber(b *testing.B) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"short", "1.5"},
		{"long_integer", "1" + strings.Repeat("0", 1000) + "e-1000"},
		{"long_fraction", "0." + strings.Repeat("0", 19999) + "1e20000"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			raw := json.RawMessage(tc.raw)
			if !validPreferenceNumber(raw) {
				b.Fatal("合法数字被拒绝")
			}
			b.ReportAllocs()
			for b.Loop() {
				preferenceNumberSink = validPreferenceNumber(raw)
				if !preferenceNumberSink {
					b.Fatal("合法数字被拒绝")
				}
			}
		})
	}
}
