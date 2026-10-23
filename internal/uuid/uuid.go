// Package uuid 提供反馈与上游请求共用的 UUID 生成方法。
package uuid

import (
	"encoding/hex"

	"github.com/metasequoiaime/MSIME-Backend/internal/randombytes"
)

// NewV4 使用密码学安全随机数生成 RFC 9562 UUID v4，返回小写、带连字符的字符串。
func NewV4() (string, error) {
	b := randombytes.New(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
