// Package httpquery 提供 HTTP 查询参数的共用解析规则。
package httpquery

import (
	"net/url"
	"strconv"
)

// Int 读取首个参数值；缺失或为空时直接返回默认值，否则解析十进制整数并检查闭区间。
// 无效时仍返回 Atoi 的结果，调用方必须先检查 ok；不裁剪空白或强制规范数字格式。
func Int(query url.Values, name string, defaultValue, minimum, maximum int) (value int, ok bool) {
	raw := query.Get(name)
	if raw == "" {
		return defaultValue, true
	}
	value, err := strconv.Atoi(raw)
	return value, err == nil && value >= minimum && value <= maximum
}
