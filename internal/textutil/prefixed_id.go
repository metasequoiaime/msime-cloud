package textutil

import "strconv"

// ParsePrefixedID 解析单字节前缀和正的十进制 int64 编号，拒绝符号、前导零和其他非规范写法。
func ParsePrefixedID(value string, prefix byte) (int64, bool) {
	if len(value) < 2 || value[0] != prefix {
		return 0, false
	}
	id, err := strconv.ParseInt(value[1:], 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != value[1:] {
		return 0, false
	}
	return id, true
}
