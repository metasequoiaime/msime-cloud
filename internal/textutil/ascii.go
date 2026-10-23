package textutil

import "unicode/utf8"

// IsASCII 判断字符串是否只含 ASCII 字节，空字符串也成立。
func IsASCII(text string) bool {
	for i := range len(text) {
		if text[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// EqualFoldASCII 只忽略 ASCII 字母的大小写，其余字节原样比较，包括无效 UTF-8。
func EqualFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ac, bc := a[i], b[i]
		if ac >= 'A' && ac <= 'Z' {
			ac += 'a' - 'A'
		}
		if bc >= 'A' && bc <= 'Z' {
			bc += 'a' - 'A'
		}
		if ac != bc {
			return false
		}
	}
	return true
}

// HasUpperASCII 检测纯 ASCII 文本是否含大写字母；第二个返回值表示是否为纯 ASCII。
// 遇到非 ASCII 字节（包括无效 UTF-8）时返回 false, false；空字符串返回 false, true。
func HasUpperASCII(text string) (hasUpper, ascii bool) {
	for i := range len(text) {
		c := text[i]
		if c >= utf8.RuneSelf {
			return false, false
		}
		if c >= 'A' && c <= 'Z' {
			hasUpper = true
		}
	}
	return hasUpper, true
}
