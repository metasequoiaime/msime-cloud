package account

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 端到端加密各个域（云剪贴板、输入习惯同步）共用的纯格式校验。两个域的设备身份、公钥编号和封装的字节布局相同，只是域分隔标签和服务端表各自一套，协议见 msime 主仓库的 docs/cloud-clipboard-e2e.md 和 docs/learning-sync.md。这里只放不查库、与域无关的检查。

const (
	// e2eWrapBytes 是封装解码后的字节数：32 字节临时公钥、24 字节 nonce、32 字节密钥和 16 字节 tag。
	e2eWrapBytes = 32 + 24 + 32 + 16
	// e2eDeviceNameMax 是设备名的最大字符数。
	e2eDeviceNameMax = 64
)

// isLowerHex 判断 s 是否恰好是 n 个小写十六进制字符。
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// decodeE2EBase64 按不带填充的 base64url 严格解码。标准库会跳过换行符、接受末尾多余的位，这里先限定字符集并用 Strict 拒绝非规范编码，保证每个值只有一种写法。
func decodeE2EBase64(s string) ([]byte, bool) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return nil, false
		}
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	return b, e == nil
}

// validE2EDevice 检查设备编号和公钥：公钥是 32 字节，编号是公钥 SHA-256 前 16 字节的小写十六进制。
func validE2EDevice(id, publicKey string) bool {
	key, ok := decodeE2EBase64(publicKey)
	if !ok || len(key) != 32 || !isLowerHex(id, 32) {
		return false
	}
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:16]) == id
}

// validE2EDeviceName 检查设备名：不超过 64 个字符、不含控制字符，可以为空。
func validE2EDeviceName(name string) bool {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > e2eDeviceNameMax {
		return false
	}
	return strings.IndexFunc(name, unicode.IsControl) < 0
}

// validE2EWrap 检查封装："v1." 加 139 个 base64url 字符，解码后 104 字节。
func validE2EWrap(wrap string) bool {
	body, ok := strings.CutPrefix(wrap, "v1.")
	if !ok {
		return false
	}
	b, ok := decodeE2EBase64(body)
	return ok && len(b) == e2eWrapBytes
}
