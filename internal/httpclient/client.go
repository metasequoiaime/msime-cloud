// Package httpclient 提供外部接口共用的 HTTP 客户端策略。
package httpclient

import (
	"net/http"
	"time"
)

// New 创建使用指定超时、不跟随重定向的客户端；传输与 Cookie 保持标准库默认值。
func New(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: StopRedirect}
}

// StopRedirect 保留首个重定向响应及未关闭的响应体，由调用方处理状态和关闭响应体。
func StopRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
