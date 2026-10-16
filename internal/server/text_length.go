package server

// utf16Length 统计 UTF-16 码元数，不构造编码切片；补充平面字符计两个码元。
func utf16Length(text string) int {
	n := 0
	for _, r := range text {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}
