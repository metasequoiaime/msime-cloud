package httpjson

import (
	"encoding/json"
	"errors"
	"io"
)

// DecodeValue 解析唯一一个 JSON 值，拒绝未知字段和非空白尾随数据。
// 调用方负责限制读取大小，并将错误映射为适合当前接口的响应。
func DecodeValue(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	return nil
}
