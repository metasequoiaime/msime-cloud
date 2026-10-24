package boundedio

import (
	"io"
)

// ReadAllSized 按范围内的 sizeHint 精确分配首段正文，继续检查尾部，最多读取 limit+1 字节。
// 准确声明长度时保留精确容量，适合缓存正文；声明长度不作为完整性依据，读取错误及关闭由调用方处理。
func ReadAllSized(body io.Reader, sizeHint, limit int64) ([]byte, error) {
	reader := io.LimitReader(body, limit+1)
	if sizeHint <= 0 || sizeHint > limit {
		return io.ReadAll(reader)
	}
	data := make([]byte, int(sizeHint))
	for read := 0; read < len(data); {
		n, err := reader.Read(data[read:])
		read += n
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return data[:read], err
		}
	}
	// 不用 ReadFull，避免丢弃与最后一段正文同时返回的非 EOF 错误。
	tail, err := io.ReadAll(reader)
	return append(data, tail...), err
}
