package textutil

// ValidHex 校验文本是否恰好表示 decodedSize 个字节，允许大小写，不裁剪空白或规范化文本。
func ValidHex(value string, decodedSize int) bool {
	if len(value)%2 != 0 || len(value)/2 != decodedSize {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			continue
		}
		return false
	}
	return true
}
