package boundedio

import (
	"bytes"
	"io"
)

// ReadBuffer 清空并复用 buffer，最多读取 limit+1 字节；错误、超限判断和关闭由调用方处理。
// 合法的 sizeHint 只作容量提示，并预留 EOF 探测空间；buffer 必须非 nil，limit 必须非负且小于最大 int64。
func ReadBuffer(buffer *bytes.Buffer, body io.Reader, sizeHint, limit int64) error {
	buffer.Reset()
	if sizeHint >= bytes.MinRead && sizeHint <= limit {
		capacity := int(sizeHint) + bytes.MinRead
		if buffer.Cap() < capacity {
			*buffer = *bytes.NewBuffer(make([]byte, 0, capacity))
		}
	}
	_, err := buffer.ReadFrom(io.LimitReader(body, limit+1))
	return err
}
