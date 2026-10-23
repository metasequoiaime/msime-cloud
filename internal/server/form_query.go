package server

import "strings"

const formQueryHex = "0123456789ABCDEF"

func formQueryEscapeSafe(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~'
}

// formQueryEscapeLen 返回表单编码后的字节数，空格按单个加号计算。
func formQueryEscapeLen(text string) int {
	length := len(text)
	for i := 0; i < len(text); i++ {
		if text[i] == ' ' {
			continue
		}
		if !formQueryEscapeSafe(text[i]) {
			length += 2
		}
	}
	return length
}

// appendFormQueryEscape 按 QueryEscape 规则将值追加到正文，使用局部缓冲避免临时编码字符串。
func appendFormQueryEscape(builder *strings.Builder, text string) {
	var buffer [1024]byte
	used := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		width := 1
		if c != ' ' && !formQueryEscapeSafe(c) {
			width = 3
		}
		if used+width > len(buffer) {
			builder.Write(buffer[:used])
			used = 0
		}
		switch {
		case c == ' ':
			buffer[used] = '+'
			used++
		case formQueryEscapeSafe(c):
			buffer[used] = c
			used++
		default:
			buffer[used] = '%'
			buffer[used+1] = formQueryHex[c>>4]
			buffer[used+2] = formQueryHex[c&15]
			used += 3
		}
	}
	if used > 0 {
		builder.Write(buffer[:used])
	}
}
