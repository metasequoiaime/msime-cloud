package httpquery

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestInt(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct {
		name, raw                        string
		fallback, minimum, maximum, want int
		ok                               bool
	}{
		{"默认值", "", 20, 1, 100, 20, true},
		{"默认值不重新校验", "", 0, 1, 100, 0, true},
		{"下界", "1", 20, 1, 100, 1, true},
		{"上界", "100", 20, 1, 100, 100, true},
		{"零偏移", "0", 0, 0, 100000, 0, true},
		{"负偏移", "-1", 0, 0, 100000, -1, false},
		{"页数为零", "0", 1, 1, 10000, 0, false},
		{"超出上界", "101", 20, 1, 100, 101, false},
		{"前导零", "01", 20, 1, 100, 1, true},
		{"正号", "+2", 20, 1, 100, 2, true},
		{"空白", " 2", 20, 1, 100, 0, false},
		{"非整数", "2.0", 20, 1, 100, 0, false},
		{"非法字符", "bad", 20, 1, 100, 0, false},
		{"非ASCII数字", "２", 20, 1, 100, 0, false},
		{"整数上界", strconv.Itoa(maxInt), 20, 1, maxInt, maxInt, true},
		{"正溢出", strings.Repeat("9", 100), 20, 1, maxInt, maxInt, false},
		{"负溢出", "-" + strings.Repeat("9", 100), 20, 1, maxInt, -maxInt - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Int(url.Values{"page": {tc.raw}}, "page", tc.fallback, tc.minimum, tc.maximum)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("解析 %q = (%d, %t)，期望 (%d, %t)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestIntFirstValue(t *testing.T) {
	for _, tc := range []struct {
		query url.Values
		want  int
		ok    bool
	}{
		{nil, 20, true},
		{url.Values{"other": {"bad"}}, 20, true},
		{url.Values{"limit": nil}, 20, true},
		{url.Values{"limit": {"", "-1"}}, 20, true},
		{url.Values{"limit": {"3", "bad"}}, 3, true},
		{url.Values{"limit": {"bad", "3"}}, 0, false},
	} {
		if got, ok := Int(tc.query, "limit", 20, 1, 100); got != tc.want || ok != tc.ok {
			t.Fatalf("参数 %v = (%d, %t)，期望 (%d, %t)", tc.query, got, ok, tc.want, tc.ok)
		}
	}
}
