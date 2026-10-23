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

func TestRandomTokenAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		generate func() string
		budget   float64
	}{
		{"后台令牌", adminRandom, 2},
		{"QQ附件名", qqMediaName, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var token string
			allocations := testing.AllocsPerRun(100, func() { token = tc.generate() })
			t.Logf("分配次数：%g；令牌长度：%d", allocations, len(token))
			if allocations > tc.budget {
				t.Fatalf("生成令牌分配 %g 次，预算 %g 次", allocations, tc.budget)
			}
		})
	}
}
