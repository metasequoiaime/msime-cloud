package digest

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestHMACSHA256(t *testing.T) {
	// RFC 4231 的固定向量覆盖普通密钥、二进制数据和超过 SHA-256 块长的密钥。
	for _, tc := range []struct {
		name      string
		key, data []byte
		want      string
	}{
		{"empty", nil, nil, "b613679a0814d9ec772f95d778c35fc5ff1697c493715653c6c712144292c5ad"},
		{"rfc4231-text", []byte("Jefe"), []byte("what do ya want for nothing?"), "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"},
		{"rfc4231-binary", bytes.Repeat([]byte{0xaa}, 20), bytes.Repeat([]byte{0xdd}, 50), "773ea91e36800e46854db8ebd09181a72959098b3ef8c122d9635514ced565fe"},
		{"rfc4231-long-key", bytes.Repeat([]byte{0xaa}, 131), []byte("Test Using Larger Than Block-Size Key - Hash Key First"), "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hex.EncodeToString(HMACSHA256(tc.key, tc.data)); got != tc.want {
				t.Fatalf("HMAC-SHA256 不匹配：得到 %q，期望 %q", got, tc.want)
			}
		})
	}
}
