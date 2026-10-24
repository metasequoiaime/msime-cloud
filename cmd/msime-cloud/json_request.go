package main

import (
	"encoding/json"
	"net/http"
)

// newJSONPostRequest 共用 CLI 的 JSON POST 编码；鉴权、发送和响应处理仍由调用方决定。
func newJSONPostRequest(path string, value any) (request, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return request{}, err
	}
	return request{method: http.MethodPost, path: path, body: body, contentType: "application/json"}, nil
}
