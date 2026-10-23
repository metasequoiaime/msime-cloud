package httpjson

import (
	"errors"
	"net/http"
)

// IsBodyTooLarge 判断请求体是否超过 `MaxBytesReader` 的上限，包括包装或合并后的错误。
// 状态码和错误文案由各接口决定。
func IsBodyTooLarge(err error) bool {
	var limit *http.MaxBytesError
	return errors.As(err, &limit)
}
