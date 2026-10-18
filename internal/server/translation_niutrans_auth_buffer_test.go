package server

import (
	"crypto/md5"
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func previousNiuTransTextAuth(appID, apiKey, source, target, timestamp, text string) string {
	canonical := "apikey=" + apiKey + "&appId=" + appID + "&from=" + source + "&srcText=" + text + "&timestamp=" + timestamp + "&to=" + target
	checksum := md5.Sum([]byte(canonical))
	return hex.EncodeToString(checksum[:])
}

func TestNiuTransTextAuthFixedVectors(t *testing.T) {
	if got := niutransAuthString("", "", "", "", "", ""); got != "3ad6bd0a2c85d0241596e45393a6126a" {
		t.Fatalf("空字段签名变化：结果=%s", got)
	}
	if got := niutransAuthString("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", "水杉 & + = 😀\n"); got != "652bb9cbe36f457decfe0bf8c8b774ed" {
		t.Fatalf("Unicode 和特殊字符签名变化：结果=%s", got)
	}
}

func TestNiuTransTextAuthRawBytesCompatibility(t *testing.T) {
	rng := rand.New(rand.NewSource(113))
	for size := 0; size < 256; size++ {
		var fields [6]string
		for i := range fields {
			data := make([]byte, size)
			_, _ = rng.Read(data)
			fields[i] = string(data)
		}
		got := niutransAuthString(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5])
		want := previousNiuTransTextAuth(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5])
		if got != want {
			t.Fatalf("原始字节签名变化：字段长度=%d，结果=%s，期望=%s", size, got, want)
		}
	}
}

func niuTransTextAuthInputs() []struct{ name, text string } {
	return []struct{ name, text string }{
		{"empty", ""},
		{"word", "水杉输入法"},
		{"ascii_chunk", strings.Repeat("a", niutransTextCharacters)},
		{"chinese_request", strings.Repeat("杉", contract.TranslationInputBytes/3)},
		{"emoji_request", strings.Repeat("😀", contract.TranslationInputBytes/4)},
	}
}

func TestNiuTransTextAuthBufferAllocationBudget(t *testing.T) {
	for _, tc := range niuTransTextAuthInputs() {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(50, func() {
				got = niutransAuthString("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", tc.text)
			})
			if got != previousNiuTransTextAuth("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", tc.text) {
				t.Fatal("签名结果变化")
			}
			if allocations > 2 {
				t.Fatalf("文本签名分配 %.0f 次，期望仅分配签名字节缓冲区和摘要字符串", allocations)
			}
		})
	}
}

func BenchmarkNiuTransTextAuthBuffer(b *testing.B) {
	for _, tc := range niuTransTextAuthInputs() {
		for _, impl := range []struct {
			name string
			auth func(string, string, string, string, string, string) string
		}{{"previous", previousNiuTransTextAuth}, {"current", niutransAuthString}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var got string
				for b.Loop() {
					got = impl.auth("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", tc.text)
				}
				if got != previousNiuTransTextAuth("synthetic-app", "synthetic-key", "auto", "en", "1700000000000", tc.text) {
					b.Fatal("签名结果变化")
				}
			})
		}
	}
}
