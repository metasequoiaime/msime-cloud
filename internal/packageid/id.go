// Package packageid 提供皮肤和插件共用的安装包标识符校验。
package packageid

// Valid 要求 1～64 个小写 ASCII 字母、数字、点、横线或下划线，以字母或数字开头，拒绝 nul 和尾点。
// 内置包等业务保留 ID 由调用方另行判断；这不是通用 Windows 文件名校验。
func Valid(id string) bool {
	if len(id) == 0 || len(id) > 64 || id == "nul" || id[len(id)-1] == '.' {
		return false
	}
	if c := id[0]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return false
	}
	for i := 1; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
