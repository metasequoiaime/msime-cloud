package server

import (
	"context"
	"io"
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

// newRequest 使用管理员配置的地址和凭据创建请求，内容协商及发送策略由调用方设置。
func (e Endpoint) newRequest(ctx context.Context, method string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.URL, body)
	if err != nil {
		return nil, err
	}
	e.authorizeRequest(req)
	return req, nil
}

// authorizeRequest 只为配置了凭据的端点添加 Bearer 鉴权。
func (e Endpoint) authorizeRequest(req *http.Request) {
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
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

// newJSONBodyRequest 复用公共 JSON 正文构造并添加端点鉴权，响应内容协商由调用方决定。
func (e Endpoint) newJSONBodyRequest(ctx context.Context, method string, value any) (*http.Request, error) {
	req, err := httpclient.NewJSONPayloadRequest(ctx, method, e.URL, value)
	if err != nil {
		return nil, err
	}
	e.authorizeRequest(req)
	return req, nil
}

// newJSONPayloadRequest 先编码 JSON 值，再沿用端点鉴权和内容协商；保留精确正文长度及重放能力，编码失败时不构造请求。
func (e Endpoint) newJSONPayloadRequest(ctx context.Context, method string, value any) (*http.Request, error) {
	req, err := e.newJSONBodyRequest(ctx, method, value)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}
