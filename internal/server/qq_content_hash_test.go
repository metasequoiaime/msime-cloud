package server

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"
)

func TestQQContentHashMatchesPreviousFormat(t *testing.T) {
	cases := [][]string{
		nil, {}, {""}, {"", ""}, {"水杉", "标题"}, {"a", "b"}, {"a\x00b"},
		{"a\x00", "b"}, {"a", "\x00b"}, {"\xff\xfe", "水\x00杉", "😀"},
	}
	for _, size := range []int{31, 32, 33, 63, 64, 65, 255, 256, 257, 1023, 1024, 1025, 4095, 4096, 4097, 8193} {
		text := strings.Repeat("a", size)
		cases = append(cases, []string{text}, []string{text, ""}, []string{"", text}, []string{text, "", strings.Repeat("水", size)})
	}
	random := rand.New(rand.NewSource(11))
	for range 300 {
		parts := make([]string, random.Intn(10))
		for i := range parts {
			data := make([]byte, random.Intn(1024))
			_, _ = random.Read(data)
			parts[i] = string(data)
		}
		cases = append(cases, parts)
	}
	for _, parts := range cases {
		sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		if got, want := qqContentHash(parts...), hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("内容哈希格式变化：各段字节数=%v，结果=%q，期望=%q", hashPartLengths(parts), got, want)
		}
	}
}

func hashPartLengths(parts []string) []int {
	lengths := make([]int, len(parts))
	for i, part := range parts {
		lengths[i] = len(part)
	}
	return lengths
}

func TestQQContentHashUsesBoundedMemory(t *testing.T) {
	parts := []string{"合成标题", "已完成", strings.Repeat("合成正文", 32768), "1"}
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqContentHash(parts...)
		}
	})
	if len(got) != sha256.Size*2 {
		t.Fatalf("内容哈希长度=%d，期望 %d", len(got), sha256.Size*2)
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 16<<10 {
		t.Fatalf("内容哈希分配 %d 字节，超过 16 KiB 预算", allocated)
	}
}

func BenchmarkQQContentHash(b *testing.B) {
	for _, tc := range []struct {
		name  string
		parts []string
	}{
		{"empty", nil},
		{"document_id", []string{"合成指南", "安装 > 设置"}},
		{"document_text", []string{strings.Repeat("合成正文", 375)}},
		{"history_64k", []string{"合成标题", "已完成", strings.Repeat("x", 64<<10), "1"}},
		{"history_384k", []string{"合成标题", "已完成", strings.Repeat("合成正文", 32768), "1"}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqContentHash(tc.parts...)
			}
		})
	}
}
