// Package httpjson 提供账户与服务端共用的 JSON 请求校验。
package httpjson

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Decode 在指定字节上限内解析唯一一个 JSON 值，拒绝未知字段。
// reject 沿用调用方的错误响应格式和响应头；成功时不写入响应。
func Decode(w http.ResponseWriter, r *http.Request, v any, maxBytes int64, reject func(http.ResponseWriter, int, string)) bool {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		reject(w, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		reject(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}
