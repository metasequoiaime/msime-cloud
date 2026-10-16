package httpjson

import (
	"encoding/json"
	"net/http"
)

// Write 写入 JSON 响应，保留调用方设置的缓存、鉴权等响应头。
// 状态码先于编码提交；编码或连接写入失败时不追加其他响应，沿用现有端点的行为。
func Write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WriteError 只写入调用方给定的固定错误码，不接收上游错误或用户输入。
func WriteError(w http.ResponseWriter, status int, code string) {
	Write(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}
