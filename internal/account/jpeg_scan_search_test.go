package account

import (
	"bytes"
	"image"
	"image/jpeg"
	"math/rand/v2"
	"testing"
)

// previousJPEGScans 保留逐字节实现，校验标记查找的兼容性并提供性能基线。
func previousJPEGScans(data []byte) int {
	scans := 0
	for i := 2; i+1 < len(data); {
		if data[i] != 0xFF {
			i++
			continue
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0xD9:
			return scans
		case marker == 0x00 || marker >= 0xD0 && marker <= 0xD7:
			i += 2
			continue
		}
		if i+3 >= len(data) {
			return scans
		}
		i += 2 + (int(data[i+2])<<8 | int(data[i+3]))
		if marker != 0xDA {
			continue
		}
		scans++
		for i+1 < len(data) && (data[i] != 0xFF || data[i+1] == 0x00 || data[i+1] >= 0xD0 && data[i+1] <= 0xD7) {
			i++
		}
	}
	return scans
}

func jpegScanSearchImage(tb testing.TB, side int) []byte {
	tb.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	random := rand.New(rand.NewPCG(31, 37))
	for i := range img.Pix {
		img.Pix[i] = byte(random.Uint32())
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 90}); err != nil {
		tb.Fatal(err)
	}
	return out.Bytes()
}

func TestJPEGScanSearchCompatibility(t *testing.T) {
	check := func(data []byte) {
		t.Helper()
		if got, want := jpegScans(data), previousJPEGScans(data); got != want {
			t.Fatalf("扫描计数改变：字节数=%d，实际=%d，原实现=%d", len(data), got, want)
		}
	}
	image := jpegScanSearchImage(t, 32)
	for end := 0; end <= len(image); end++ {
		check(image[:end])
	}
	for _, scans := range []int{1, 32, 33, 1000} {
		check(candidateJPEGWithScans(t, image, scans))
	}
	for first := range 256 {
		for second := range 256 {
			check([]byte{0xff, 0xd8, 0xff, byte(first), byte(second), 0, 0xff, 0xda, 0, 2, 0xff, 0xd9})
		}
	}
	for _, marker := range []byte{0, 0xd0, 0xd7, 0xff, 0xd9, 0xda} {
		for _, size := range []int{0, 1, 2, 255, 4096} {
			data := append([]byte{0xff, 0xd8, 0xff, 0xda, 0, 2}, bytes.Repeat([]byte{0x17}, size)...)
			data = append(data, 0xff, marker, 0xff, 0xda, 0, 2, 0xff, 0xd9)
			check(data)
		}
	}
	random := rand.New(rand.NewPCG(41, 43))
	for range 2000 {
		data := make([]byte, random.IntN(4096))
		for i := range data {
			data[i] = byte(random.Uint32())
		}
		check(data)
	}
}

func FuzzJPEGScanSearch(f *testing.F) {
	f.Add([]byte{0xff, 0xd8, 0xff, 0xda, 0, 2, 1, 0xff, 0, 0xff, 0xd0, 0xff, 0xff, 0xd9})
	f.Add(jpegScanSearchImage(f, 8))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if got, want := jpegScans(data), previousJPEGScans(data); got != want {
			t.Fatalf("扫描计数改变：实际=%d，原实现=%d", got, want)
		}
	})
}

func benchmarkJPEGScanSearch(b *testing.B, scan func([]byte) int, data []byte, want int) {
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if got := scan(data); got != want {
			b.Fatalf("扫描计数=%d，期望=%d", got, want)
		}
	}
}

func TestJPEGScanSearchBudget(t *testing.T) {
	data := append([]byte{0xff, 0xd8, 0xff, 0xda, 0, 2}, bytes.Repeat([]byte{0x17}, 1<<20)...)
	data = append(data, 0xff, 0xd9)
	previous := testing.Benchmark(func(b *testing.B) { benchmarkJPEGScanSearch(b, previousJPEGScans, data, 1) })
	current := testing.Benchmark(func(b *testing.B) { benchmarkJPEGScanSearch(b, jpegScans, data, 1) })
	t.Logf("原实现：%s；当前：%s", previous, current)
	// 标记稀疏时应批量跳过普通字节，留出宽松余量以容忍测量波动。
	if current.NsPerOp()*2 >= previous.NsPerOp() {
		t.Fatalf("稀疏标记查找未至少减半：原实现=%d ns/op，当前=%d ns/op", previous.NsPerOp(), current.NsPerOp())
	}
}

func BenchmarkJPEGScanSearch(b *testing.B) {
	sparse := append([]byte{0xff, 0xd8, 0xff, 0xda, 0, 2}, bytes.Repeat([]byte{0x17}, 1<<20)...)
	sparse = append(sparse, 0xff, 0xd9)
	dense := append([]byte{0xff, 0xd8, 0xff, 0xda, 0, 2}, bytes.Repeat([]byte{0xff, 0}, 1<<19)...)
	dense = append(dense, 0xff, 0xd9)
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"sparse1MiB", sparse},
		{"stuffed1MiB", dense},
		{"jpeg1024", jpegScanSearchImage(b, 1024)},
		{"jpeg32", jpegScanSearchImage(b, 32)},
	} {
		want := previousJPEGScans(tc.data)
		for _, impl := range []struct {
			name string
			scan func([]byte) int
		}{
			{"previous", previousJPEGScans}, {"current", jpegScans},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) { benchmarkJPEGScanSearch(b, impl.scan, tc.data, want) })
		}
	}
}
