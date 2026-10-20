package account

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image/png"
	"io"
	"testing"
)

// 保留逐帧新建解压器的实现，测量复用工作区对完整解码流程的影响。
func previousAPNGZlibStreamExact(parts [][]byte) bool {
	r := &apngDataReader{parts: parts}
	z, err := zlib.NewReader(r)
	if err != nil {
		return false
	}
	_, err = io.Copy(io.Discard, z)
	z.Close()
	r.advance()
	return err == nil && len(r.parts) == 0
}

func previousAPNGDecode(a *candidateAPNG) bool {
	images := make([]apngFrame, 0, len(a.frames)+1)
	if a.images > len(a.frames) {
		images = append(images, apngFrame{width: uint32(a.width), height: uint32(a.height), data: a.idat})
	}
	images = append(images, a.frames...)
	for _, frame := range images {
		decoded, err := png.Decode(bytes.NewReader(a.framePNG(frame, nil)))
		if err != nil || decoded.Bounds().Dx() != int(frame.width) || decoded.Bounds().Dy() != int(frame.height) || !previousAPNGZlibStreamExact(frame.data) {
			return false
		}
	}
	return true
}

// 每帧使用不同的真实像素数据，覆盖解压窗口和校验和的状态重置。
func apngZlibReuseFixture(tb testing.TB, count int) *candidateAPNG {
	tb.Helper()
	a := &candidateAPNG{width: 12, height: 10, images: count}
	for i := range count {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, apngTestImage(a.width, a.height, i)); err != nil {
			tb.Fatal(err)
		}
		frame := apngFrame{width: uint32(a.width), height: uint32(a.height)}
		data := encoded.Bytes()
		for pos := len(pngSignature); pos < len(data); {
			length := int(binary.BigEndian.Uint32(data[pos:]))
			body := data[pos+8 : pos+8+length]
			switch string(data[pos+4 : pos+8]) {
			case "IHDR":
				a.ihdr = body
			case "IDAT":
				frame.data = append(frame.data, body)
			}
			pos += length + 12
		}
		a.frames = append(a.frames, frame)
	}
	a.idat = a.frames[0].data
	return a
}

func TestAPNGZlibReuseCompatibility(t *testing.T) {
	for _, count := range []int{1, 4, maxCandidateAnimationFrames} {
		for _, thumbnail := range []bool{false, true} {
			a := apngZlibReuseFixture(t, count)
			if thumbnail {
				a.images++
			}
			if !a.decode() || !previousAPNGDecode(a) {
				t.Fatal("有效动画被拒绝")
			}
			// 损坏末帧，保证前面的有效帧已经使用了解压器。
			last := &a.frames[len(a.frames)-1]
			saved := last.data
			for _, suffix := range [][]byte{{0}, saved[0]} {
				last.data = append(append([][]byte{}, saved...), suffix)
				if got, want := a.decode(), previousAPNGDecode(a); got != want || got {
					t.Fatal("尾随数据或第二个流的拒绝规则改变")
				}
			}
			for _, cut := range []int{1, len(saved[0]) - 1} {
				last.data = [][]byte{saved[0][:cut]}
				if a.decode() || previousAPNGDecode(a) {
					t.Fatal("截断流被接受")
				}
			}
			last.data = saved
			if !a.decode() {
				t.Fatal("后续解码受到先前失败影响")
			}
		}
	}
}

func TestAPNGZlibReuseStreamSequence(t *testing.T) {
	var validator apngZlibValidator
	for _, level := range []int{zlib.NoCompression, zlib.BestSpeed, zlib.DefaultCompression, zlib.HuffmanOnly} {
		stream := apngZlibPartsStream(t, bytes.Repeat([]byte("不同的帧像素😀"), 4096), level)
		corrupt := bytes.Clone(stream)
		corrupt[len(corrupt)-1] ^= 1
		for _, data := range [][]byte{nil, stream, stream[:1], stream, corrupt, stream, append(bytes.Clone(stream), 0), stream} {
			parts := apngZlibParts(data, 7)
			if got, want := validator.exact(parts), previousAPNGZlibStreamExact(parts); got != want {
				t.Fatalf("压缩级别 %d 的连续流判断改变：实际=%t，期望=%t", level, got, want)
			}
		}
	}
}

func TestAPNGZlibReuseMemoryBudget(t *testing.T) {
	a := apngZlibReuseFixture(t, maxCandidateAnimationFrames)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if !a.decode() {
				b.Fatal("有效动画被拒绝")
			}
		}
	})
	t.Logf("%s；%s", result, result.MemString())
	// 包含全部 PNG 像素解码开销，但排除每帧额外分配约 40 KiB 的严格校验工作区。
	if allocated := result.AllocedBytesPerOp(); allocated > int64(maxCandidateAnimationFrames)*64<<10 {
		t.Fatalf("120 帧解码分配 %d 字节，超过每帧 64 KiB 预算", allocated)
	}
}

func BenchmarkAPNGZlibReuse(b *testing.B) {
	for _, count := range []int{1, 4, maxCandidateAnimationFrames} {
		a := apngZlibReuseFixture(b, count)
		for _, implementation := range []struct {
			name string
			run  func(*candidateAPNG) bool
		}{{"previous", previousAPNGDecode}, {"current", (*candidateAPNG).decode}} {
			b.Run(fmt.Sprintf("%d帧/%s", count, implementation.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if !implementation.run(a) {
						b.Fatal("有效动画被拒绝")
					}
				}
			})
		}
	}
}
