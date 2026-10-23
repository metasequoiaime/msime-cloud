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

// newJSONRequest 创建期望 JSON 响应的请求；正文类型可为 JSON、表单或 multipart，留空时不添加 Content-Type。
func (e Endpoint) newJSONRequest(ctx context.Context, method, contentType string, body io.Reader) (*http.Request, error) {
	req, err := e.newRequest(ctx, method, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}
