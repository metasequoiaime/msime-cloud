package textutil

// ValidClientLabel 校验平台、版本、语言或服务商等客户端短标识：1～64 个 ASCII 字母、数字、点、下划线、加号或横线，首字符须为字母或数字。
func ValidClientLabel(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i == 0 || !(c == '.' || c == '_' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}
