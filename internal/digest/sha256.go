// Package digest 提供各功能共用的摘要编码。
package digest

import (
	"crypto/sha256"
	"encoding/hex"
)

// SHA256 返回原始字节的完整 SHA-256 摘要，编码为 64 位小写十六进制字符串。
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
