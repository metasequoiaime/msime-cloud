// Package boundedio 提供带大小上限和容量提示的有限读取。
package boundedio

import (
	"bytes"
	"io"
)

// ReadAll 最多读取 limit+1 字节，合法的 sizeHint 只作容量提示；错误、超限判断和关闭由调用方处理。
func ReadAll(body io.Reader, sizeHint, limit int64) ([]byte, error) {
	reader := io.LimitReader(body, limit+1)
	if sizeHint >= bytes.MinRead && sizeHint <= limit {
		buffer := bytes.NewBuffer(make([]byte, 0, int(sizeHint)+bytes.MinRead))
		_, err := buffer.ReadFrom(reader)
		return buffer.Bytes(), err
	}
	return io.ReadAll(reader)
}
