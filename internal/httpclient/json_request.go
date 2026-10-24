package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
)

// NewJSONPayloadRequest 编码 JSON 正文并设置 Content-Type，保留精确长度和重放能力；鉴权、Accept 及发送策略由调用方决定。
func NewJSONPayloadRequest(ctx context.Context, method, target string, value any) (*http.Request, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return NewJSONBodyRequest(ctx, method, target, payload)
}
