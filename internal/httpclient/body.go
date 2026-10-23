package httpclient

import (
	"bytes"
	"io"
	"net/http"
)

// ReadResponseBody 最多读取 limit+1 字节，保留超限探测字节和读取错误；状态码、超限处理和关闭正文由调用方负责。
// 大响应按范围内的声明长度预留正文与 EOF 探测空间，小响应或未知长度沿用标准缓冲。
func ReadResponseBody(response *http.Response, limit int64) ([]byte, error) {
	return ReadBody(response.Body, response.ContentLength, limit)
}

// ReadBody 最多读取 limit+1 字节，合法的 contentLength 只作容量提示；错误、超限判断和关闭由调用方处理。
func ReadBody(body io.Reader, contentLength, limit int64) ([]byte, error) {
	reader := io.LimitReader(body, limit+1)
	if contentLength >= bytes.MinRead && contentLength <= limit {
		buffer := bytes.NewBuffer(make([]byte, 0, int(contentLength)+bytes.MinRead))
		_, err := buffer.ReadFrom(reader)
		return buffer.Bytes(), err
	}
	return io.ReadAll(reader)
}
