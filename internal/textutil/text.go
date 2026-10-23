package textutil

import (
	"strings"
	"unicode/utf8"
)

// ValidUTF8Text 校验字节上限、UTF-8 编码和 NUL；允许空文本及其他控制字符，不裁剪空白。
func ValidUTF8Text(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
