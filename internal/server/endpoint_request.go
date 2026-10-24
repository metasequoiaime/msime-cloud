package server

import (
	"bytes"
	"context"
	"encoding/json"
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

// newJSONPayloadRequest 先编码 JSON 值，再沿用端点鉴权和内容协商；保留精确正文长度及重放能力，编码失败时不构造请求。
func (e Endpoint) newJSONPayloadRequest(ctx context.Context, method string, value any) (*http.Request, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return e.newJSONRequest(ctx, method, "application/json", bytes.NewReader(payload))
}
