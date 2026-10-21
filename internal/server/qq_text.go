package server

// qqRegexpSpace 匹配 Go 正则 \s 的五种 ASCII 空白，不包含垂直制表符。
func qqRegexpSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\f', '\r':
		return true
	default:
		return false
	}
}
