package server

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
	"testing/cryptotest"
)

func TestRandomTokenEncodingCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		generate func() string
		size     int
		encode   func([]byte) string
	}{
		{"后台令牌", adminRandom, 32, hex.EncodeToString},
		{"QQ附件名", qqMediaName, 16, hex.EncodeToString},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cryptotest.SetGlobalRandom(t, 42)
			var want [2]string
			for i := range want {
				b := make([]byte, tc.size)
				_, _ = rand.Read(b)
				want[i] = tc.encode(b)
			}
			cryptotest.SetGlobalRandom(t, 42)
			for i := range want {
				if got := tc.generate(); got != want[i] {
					t.Fatalf("第 %d 次生成的字节数或编码变化", i+1)
				}
			}
		})
	}
}

// 保留抽取前的固定大小实现，避免把某个平台的标准库分配次数当成跨平台常量。
func previousAdminRandom() string {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}

func previousQQMediaName() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestRandomTokenAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name               string
		generate, previous func() string
	}{
		{"后台令牌", adminRandom, previousAdminRandom},
		{"QQ附件名", qqMediaName, previousQQMediaName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var token string
			baseline := testing.AllocsPerRun(100, func() { token = tc.previous() })
			current := testing.AllocsPerRun(100, func() { token = tc.generate() })
			t.Logf("原实现分配次数：%g；当前：%g；令牌长度：%d", baseline, current, len(token))
			if current > baseline {
				t.Fatalf("生成令牌分配 %g 次，超过同平台原实现 %g 次", current, baseline)
			}
		})
	}
}
