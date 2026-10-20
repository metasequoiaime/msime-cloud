package account

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"testing"
)

// 固定原来的拼接与校验规则，验证流结束位置和错误处理没有改变。
func previousZlibStreamExact(parts [][]byte) bool {
	r := bytes.NewReader(bytes.Join(parts, nil))
	z, err := zlib.NewReader(r)
	if err == nil {
		_, err = io.Copy(io.Discard, z)
	}
	return err == nil && r.Len() == 0
}

func apngZlibPartsStream(tb testing.TB, raw []byte, level int) []byte {
	tb.Helper()
	var compressed bytes.Buffer
	w, err := zlib.NewWriterLevel(&compressed, level)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		tb.Fatal(err)
	}
	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}
	return compressed.Bytes()
}

// 在真实数据段之间加入空段，确保读取和末尾检查都能越过空 IDAT。
func apngZlibParts(data []byte, count int) [][]byte {
	parts := [][]byte{nil, {}}
	for i := range count {
		parts = append(parts, data[i*len(data)/count:(i+1)*len(data)/count], nil)
	}
	return append(parts, []byte{})
}

func TestZlibPartsCompatibility(t *testing.T) {
	check := func(data []byte) {
		t.Helper()
		for _, count := range []int{1, 2, 7, max(1, len(data))} {
			parts := apngZlibParts(data, count)
			savedParts := slices.Clone(parts)
			savedBytes := bytes.Clone(data)
			if got, want := zlibStreamExact(parts), previousZlibStreamExact(parts); got != want {
				t.Fatalf("%d 段、%d 字节流的判断改变：实际=%t，期望=%t", count, len(data), got, want)
			}
			if !bytes.Equal(data, savedBytes) {
				t.Fatal("校验修改了压缩数据")
			}
			for i := range parts {
				if len(parts[i]) != len(savedParts[i]) || !bytes.Equal(parts[i], savedParts[i]) {
					t.Fatal("校验修改了调用方的数据段")
				}
			}
		}
	}
	for _, level := range []int{zlib.NoCompression, zlib.BestSpeed, zlib.DefaultCompression, zlib.HuffmanOnly} {
		for _, raw := range [][]byte{nil, []byte("候选窗😀"), bytes.Repeat([]byte("frame pixels"), 100)} {
			stream := apngZlibPartsStream(t, raw, level)
			if !previousZlibStreamExact([][]byte{stream}) {
				t.Fatal("生成的 zlib 流无效")
			}
			check(stream)
			for _, cut := range []int{0, 1, 2, len(stream) - 5, len(stream) - 1} {
				check(stream[:cut])
			}
			for _, index := range []int{0, len(stream) / 2, len(stream) - 1} {
				corrupt := bytes.Clone(stream)
				corrupt[index] ^= 0x80
				check(corrupt)
			}
			check(append(bytes.Clone(stream), 0))
			check(append(bytes.Clone(stream), stream...))
		}
	}
	random := rand.New(rand.NewPCG(47, 91))
	for range 100 {
		data := make([]byte, random.IntN(256))
		for i := range data {
			data[i] = byte(random.IntN(256))
		}
		check(data)
	}
}

func TestZlibPartsTrailingBytes(t *testing.T) {
	stream := apngZlibPartsStream(t, []byte("synthetic frame pixels"), zlib.DefaultCompression)
	for _, parts := range [][][]byte{
		{stream, nil, {0}},
		{stream[:len(stream)-1], stream[len(stream)-1:], {}, {1}},
		{stream, nil, stream},
	} {
		if zlibStreamExact(parts) {
			t.Fatal("流后的多余字节或第二个流被接受")
		}
	}
	if !zlibStreamExact(apngZlibParts(stream, len(stream))) {
		t.Fatal("按字节拆分且末尾只有空段的流被拒绝")
	}
}

func TestZlibPartsMemoryBudget(t *testing.T) {
	raw := bytes.Repeat([]byte{1, 2, 3, 4}, 240<<10)
	stream := apngZlibPartsStream(t, raw, zlib.NoCompression)
	for _, count := range []int{1, 64} {
		t.Run(fmt.Sprintf("%d段", count), func(t *testing.T) {
			parts := apngZlibParts(stream, count)
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					if !zlibStreamExact(parts) {
						b.Fatal("有效流被拒绝")
					}
				}
			})
			t.Logf("%s；%s", result, result.MemString())
			// 解压器可分配窗口和工作区，但不应复制接近 1 MiB 的完整压缩流。
			if allocated := result.AllocedBytesPerOp(); allocated > 256<<10 {
				t.Fatalf("分段流校验分配 %d 字节，超过 256 KiB 预算", allocated)
			}
		})
	}
}

func BenchmarkZlibPartsReader(b *testing.B) {
	for _, tc := range []struct {
		name         string
		raw          []byte
		level, count int
	}{
		{"small", bytes.Repeat([]byte("pixel"), 64), zlib.DefaultCompression, 1},
		{"large-single", bytes.Repeat([]byte{1, 2, 3, 4}, 240<<10), zlib.NoCompression, 1},
		{"large-64", bytes.Repeat([]byte{1, 2, 3, 4}, 240<<10), zlib.NoCompression, 64},
	} {
		parts := apngZlibParts(apngZlibPartsStream(b, tc.raw, tc.level), tc.count)
		for _, implementation := range []struct {
			name string
			run  func([][]byte) bool
		}{{"previous", previousZlibStreamExact}, {"current", zlibStreamExact}} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if !implementation.run(parts) {
						b.Fatal("有效流被拒绝")
					}
				}
			})
		}
	}
}
