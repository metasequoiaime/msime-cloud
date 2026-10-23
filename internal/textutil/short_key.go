package textutil

// ValidShortKey 校验服务键、分发渠道等短键：1～32 个小写 ASCII 字母、数字、下划线或横线，首字符须为字母或数字。
func ValidShortKey(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if i == 0 || !(c == '_' || c == '-') {
			return false
		}
	}
	return true
}
