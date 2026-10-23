// Package textutil 提供各功能共用的文本处理方法。
package textutil

// UTF16Length 统计 UTF-16 码元数，不构造编码切片；补充平面字符计两个码元。
// 无效 UTF-8 的每个错误字节按 U+FFFD 计数，与先转换 rune 再编码的结果一致。
func UTF16Length(text string) int {
	n := 0
	for _, r := range text {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}
