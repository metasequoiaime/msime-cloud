package textutil

import "testing"

func TestValidUTF8Text(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		maxBytes    int
		want        bool
	}{
		{"空文本", "", 0, true},
		{"负上限", "", -1, false},
		{"零上限", "a", 0, false},
		{"ASCII边界", "abc", 3, true},
		{"ASCII越界", "abc", 2, false},
		{"中文边界", "水杉", 6, true},
		{"按字节限制中文", "水杉", 5, false},
		{"四字节字符", "😀", 4, true},
		{"按字节限制表情", "😀", 3, false},
		{"空白不裁剪", " \t\n\r", 4, true},
		{"其他控制字符保留", "\x01\x1b\x7f", 3, true},
		{"前置NUL", "\x00abc", 4, false},
		{"中间NUL", "a\x00bc", 4, false},
		{"末尾NUL", "abc\x00", 4, false},
		{"仅NUL", "\x00", 1, false},
		{"非法字节", "\xff", 1, false},
		{"独立续字节", "\x80", 1, false},
		{"截断UTF8", "\xe6\xb0", 2, false},
		{"过长NUL编码", "\xc0\x80", 2, false},
		{"代理码点", "\xed\xa0\x80", 3, false},
		{"替换字符", "\ufffd", 3, true},
		{"字面转义", `\u0000`, 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidUTF8Text(tc.value, tc.maxBytes); got != tc.want {
				t.Fatalf("文本 %q，上限 %d 字节：结果 %t，期望 %t", tc.value, tc.maxBytes, got, tc.want)
			}
		})
	}
}
