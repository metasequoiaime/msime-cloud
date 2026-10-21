package digest

import (
	"crypto/hmac"
	"crypto/sha256"
)

// HMACSHA256 返回原始字节的完整 HMAC-SHA256，编码与恒定时间比较由调用方处理。
func HMACSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}
