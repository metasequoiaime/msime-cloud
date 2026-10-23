package httpclient

import (
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
)

// ReadResponseBody 最多读取 limit+1 字节，保留超限探测字节和读取错误；状态码、超限处理和关闭正文由调用方负责。
// 大响应按范围内的声明长度预留正文与 EOF 探测空间，小响应或未知长度沿用标准缓冲。
func ReadResponseBody(response *http.Response, limit int64) ([]byte, error) {
	return boundedio.ReadAll(response.Body, response.ContentLength, limit)
}
