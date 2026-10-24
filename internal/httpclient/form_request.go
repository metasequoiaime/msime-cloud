package httpclient

import (
	"context"
	"net/http"
	"strings"
)

// NewFormBodyRequest 使用已编码的表单正文创建请求，不重排字段或再次转义；保留精确长度和重放能力，鉴权、字符集及响应内容协商由调用方设置。
func NewFormBodyRequest(ctx context.Context, method, target, encoded string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}
