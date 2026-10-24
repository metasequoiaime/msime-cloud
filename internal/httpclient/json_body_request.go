package httpclient

import (
	"bytes"
	"context"
	"net/http"
)

// NewJSONBodyRequest 使用已编码的 JSON 正文创建请求，不重新编码或校验字节；保留精确长度和重放能力，鉴权及响应内容协商由调用方设置。
func NewJSONBodyRequest(ctx context.Context, method, target string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}
