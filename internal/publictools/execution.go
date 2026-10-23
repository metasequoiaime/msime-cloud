package publictools

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// executeToolRequest 共用调用期限、取消与响应校验；请求和超时时长由各入口显式选择。
func executeToolRequest(ctx context.Context, execute Execute, request Request, timeout time.Duration) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	data, err := execute(ctx, request)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if len(data) > MaxResponse || !json.Valid(data) {
		return Result{}, errors.New("invalid_tool_response")
	}
	return Result{Source: request.Path, Data: data}, nil
}
