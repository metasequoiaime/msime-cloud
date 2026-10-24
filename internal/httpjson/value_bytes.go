package httpjson

import "bytes"

// DecodeBytes 按 DecodeValue 的严格规则解析字节切片，拒绝未知字段和非空白尾随数据。
// 调用方负责限制输入大小，并将错误映射为适合当前接口的响应。
func DecodeBytes(raw []byte, v any) error {
	return DecodeValue(bytes.NewReader(raw), v)
}
