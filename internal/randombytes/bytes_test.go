package randombytes_test

import (
	"bytes"
	"crypto/rand"
	"strconv"
	"testing"
	"testing/cryptotest"

	"github.com/metasequoiaime/MSIME-Backend/internal/randombytes"
)

func TestNewPreservesCryptoRandomStream(t *testing.T) {
	for _, size := range []int{0, 1, 12, 16, 24, 32, 64} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			cryptotest.SetGlobalRandom(t, 42)
			var want [2][]byte
			for i := range want {
				want[i] = make([]byte, size)
				_, _ = rand.Read(want[i])
			}
			cryptotest.SetGlobalRandom(t, 42)
			first, second := randombytes.New(size), randombytes.New(size)
			if first == nil || second == nil || !bytes.Equal(first, want[0]) || !bytes.Equal(second, want[1]) {
				t.Fatal("随机源、读取字节数或连续生成行为变化")
			}
			if size > 0 {
				first[0] ^= 0xff
				if !bytes.Equal(second, want[1]) {
					t.Fatal("生成结果共用了可变缓冲区")
				}
			}
		})
	}
}

func TestNewRejectsNegativeSize(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("负长度未触发 panic")
		}
	}()
	randombytes.New(-1)
}
