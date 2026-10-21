// Package httpjson 提供共用的 JSON 请求校验和响应写入。
package httpjson

import (
	"net/http"
	"strings"
)

// Decode 在指定字节上限内解析唯一一个 JSON 值，拒绝未知字段。
// reject 沿用调用方的错误响应格式和响应头；成功时不写入响应。
func Decode(w http.ResponseWriter, r *http.Request, v any, maxBytes int64, reject func(http.ResponseWriter, int, string)) bool {
	mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	mediaType = strings.Trim(mediaType, " \t")
	// 媒体类型按 ASCII 忽略大小写；先比较字节长度，避免 Unicode 折叠接受非 ASCII 类型名。
	if len(mediaType) != len("application/json") || !strings.EqualFold(mediaType, "application/json") {
		reject(w, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if DecodeValue(r.Body, v) != nil {
		reject(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}
