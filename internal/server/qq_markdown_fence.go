package server

import "strings"

// qqMarkdownFence 记录当前代码围栏的字符和长度；三个 Markdown 读取路径共用边界规则。
type qqMarkdownFence struct {
	char   byte
	length int
}

// consume 读取已裁掉前导空白的行，只有真正打开或关闭围栏时返回 true。
// 保留调用方对 Unicode 空白与缩进的宽松处理；关闭围栏必须同字符、不短于开头，且尾部只有空白。
func (f *qqMarkdownFence) consume(line string) bool {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return false
	}
	char := line[0]
	length := 1
	for length < len(line) && line[length] == char {
		length++
	}
	if length < 3 {
		return false
	}
	if f.length == 0 {
		// 反引号围栏的说明文字不能再含反引号，否则只是行内代码。
		if char == '`' && strings.ContainsRune(line[length:], '`') {
			return false
		}
		f.char, f.length = char, length
		return true
	}
	if char != f.char || length < f.length || strings.TrimSpace(line[length:]) != "" {
		return false
	}
	*f = qqMarkdownFence{}
	return true
}
