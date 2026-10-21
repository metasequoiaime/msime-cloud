package server

import (
	"context"
	"io"
	"net/http"
)

// newRequest 使用管理员配置的地址和凭据创建请求，内容协商及发送策略由调用方设置。
func (e Endpoint) newRequest(ctx context.Context, method string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.URL, body)
	if err != nil {
		return nil, err
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	return req, nil
}
