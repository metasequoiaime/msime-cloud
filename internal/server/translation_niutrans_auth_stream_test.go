package server

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// previousNiuTransCanonicalBuffer 保留完整缓冲区的基线，便于测量本次分块计算的收益。
func previousNiuTransCanonicalBuffer(appID, apiKey, source, target, timestamp, text string) string {
	size := len("apikey=&appId=&from=&srcText=&timestamp=&to=") + len(apiKey) + len(appID) + len(source) + len(text) + len(timestamp) + len(target)
	canonical := make([]byte, 0, size)
	canonical = append(canonical, "apikey="...)
	canonical = append(canonical, apiKey...)
	canonical = append(canonical, "&appId="...)
	canonical = append(canonical, appID...)
	canonical = append(canonical, "&from="...)
	canonical = append(canonical, source...)
	canonical = append(canonical, "&srcText="...)
	canonical = append(canonical, text...)
	canonical = append(canonical, "&timestamp="...)
	canonical = append(canonical, timestamp...)
	canonical = append(canonical, "&to="...)
	canonical = append(canonical, target...)
	checksum := md5.Sum(canonical) // #nosec G401 -- 上游协议要求使用 MD5。
	return hex.EncodeToString(checksum[:])
}

func TestNiuTransTextAuthStreamingBoundaries(t *testing.T) {
	rng := rand.New(rand.NewSource(86))
	for _, size := range []int{0, 1, 15, 55, 56, 63, 64, 65, 511, 512, 1023, 1024, 1025, 2047, 2048, 2049, 65536} {
		for field := range 6 {
			var fields [6]string
			data := make([]byte, size)
			_, _ = rng.Read(data)
			fields[field] = string(data)
			if got, want := niutransAuthString(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]), previousNiuTransCanonicalBuffer(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]); got != want {
				t.Fatalf("分块边界签名变化：字段=%d 字节数=%d 结果=%s 预期=%s", field, size, got, want)
			}
		}
	}
	for range 1000 {
		var fields [6]string
		for i := range fields {
			data := make([]byte, rng.Intn(4097))
			_, _ = rng.Read(data)
			fields[i] = string(data)
		}
		if got, want := niutransAuthString(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]), previousNiuTransCanonicalBuffer(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]); got != want {
			t.Fatal("多字段随机原始字节签名不兼容")
		}
	}
}

func TestNiuTransTextAuthStreamingAllocationBudget(t *testing.T) {
	inputs := append(niuTransTextAuthInputs(), struct{ name, text string }{"oversized", strings.Repeat("合成正文", 16384)})
	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = niutransAuthString("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", tc.text)
				}
			})
			if got != previousNiuTransCanonicalBuffer("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", tc.text) {
				t.Fatal("签名结果变化")
			}
			// 不允许为待签名正文保留堆缓冲，只分配最终的 32 字节十六进制摘要。
			if result.AllocsPerOp() > 1 || result.AllocedBytesPerOp() > 64 {
				t.Fatalf("文本签名分配 %d 次、%d 字节，超过一次、64 字节预算", result.AllocsPerOp(), result.AllocedBytesPerOp())
			}
		})
	}
}

func BenchmarkNiuTransTextAuthStreaming(b *testing.B) {
	for _, tc := range niuTransTextAuthInputs() {
		for _, impl := range []struct {
			name string
			auth func(string, string, string, string, string, string) string
		}{{"buffer", previousNiuTransCanonicalBuffer}, {"stream", niutransAuthString}} {
			b.Run(fmt.Sprintf("%s/%s", tc.name, impl.name), func(b *testing.B) {
				b.ReportAllocs()
				var got string
				for b.Loop() {
					got = impl.auth("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", tc.text)
				}
				if got != previousNiuTransCanonicalBuffer("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", tc.text) {
					b.Fatal("签名结果变化")
				}
			})
		}
	}
}
